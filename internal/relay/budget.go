package relay

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"agent-relay/internal/models"
)

// legacyCreatedAtLayout is the fixed-width layout the db layer writes;
// created_at is parsed as RFC3339Nano first and falls back to this.
const legacyCreatedAtLayout = "2006-01-02T15:04:05.000000Z"

// applyBudget filters messages to fit within maxBytes using a utility score.
// P0 messages always bypass the budget. Remaining messages are scored and
// selected greedily until the byte budget is exhausted.
//
// Utility = 0.7 * priorityScore + 0.2 * tagScore + 0.1 * freshnessScore,
// renormalised to 0.875 * priorityScore + 0.125 * freshnessScore when the
// agent or the message carries no tags (the tag term is undefined there).
//
// Every call with at least one candidate emits exactly one "[budget]" log
// line holding the full selection record, so a run can be replayed offline.
func applyBudget(messages []models.Message, agentTags []string, maxBytes int) []models.Message {
	if len(messages) == 0 {
		return messages
	}

	agentTagSet := make(map[string]bool, len(agentTags))
	for _, t := range agentTags {
		agentTagSet[t] = true
	}

	// Score every candidate up front so the journal covers the whole input.
	type scored struct {
		msg   models.Message
		score float64
		bytes int
	}
	now := time.Now().UTC()
	all := make([]scored, len(messages))
	for i, m := range messages {
		all[i] = scored{msg: m, score: utility(m, agentTagSet, now), bytes: messageBytes(m)}
	}

	journal := budgetJournal{Path: "get_inbox", BudgetMax: maxBytes}
	for _, s := range all {
		score := s.score
		journal.Candidates = append(journal.Candidates, s.msg.ID)
		journal.Scores = append(journal.Scores, budgetScore{
			ID: s.msg.ID, Priority: s.msg.Priority, Score: &score, Bytes: s.bytes,
			HasTask: s.msg.TaskID != nil && *s.msg.TaskID != "",
		})
	}
	defer func() { journal.emit() }()

	if maxBytes <= 0 {
		// No budget: nothing is filtered.
		for _, s := range all {
			journal.Selected = append(journal.Selected, s.msg.ID)
			journal.BudgetUsed += s.bytes
		}
		return messages
	}

	// Separate P0 (always included) from the rest
	var p0 []models.Message
	var rest []scored
	usedBytes := 0
	for _, s := range all {
		if s.msg.Priority == "P0" {
			p0 = append(p0, s.msg)
			usedBytes += s.bytes
			journal.Selected = append(journal.Selected, s.msg.ID)
		} else {
			rest = append(rest, s)
		}
	}

	if usedBytes >= maxBytes {
		// P0 alone exceeds budget — return only P0
		for _, s := range rest {
			journal.Omitted = append(journal.Omitted, s.msg.ID)
		}
		journal.BudgetUsed = usedBytes
		return p0
	}

	// Sort by utility descending (stable, so equal scores keep input order)
	sort.SliceStable(rest, func(i, j int) bool {
		return rest[i].score > rest[j].score
	})

	// Greedily select until budget
	var selected []models.Message
	for _, s := range rest {
		if usedBytes+s.bytes > maxBytes {
			journal.Omitted = append(journal.Omitted, s.msg.ID)
			continue
		}
		selected = append(selected, s.msg)
		usedBytes += s.bytes
		journal.Selected = append(journal.Selected, s.msg.ID)
	}
	journal.BudgetUsed = usedBytes

	// Combine P0 + selected, re-sort by priority index ASC (unknown last),
	// created_at DESC
	result := append(p0, selected...)
	sort.SliceStable(result, func(i, j int) bool {
		pi, pj := priorityIndex(result[i].Priority), priorityIndex(result[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return result[i].CreatedAt > result[j].CreatedAt
	})

	return result
}

// budgetScore is one candidate's line in the selection journal. Score is
// the applyBudget utility; it is absent for selectors that rank without one
// (projectMessages orders by priority rank + recency).
type budgetScore struct {
	ID       string   `json:"id"`
	Priority string   `json:"priority"`
	Score    *float64 `json:"score,omitempty"`
	Bytes    int      `json:"bytes"`
	HasTask  bool     `json:"has_task"` // messages.task_id set (DEC-wraith-linkage-1 coverage)
}

// budgetJournal is the replayable record of one budgeted selection run
// (applyBudget on get_inbox, projectMessages on get_session_context).
// Path names the selector; Agent is the recipient when the caller knows it.
type budgetJournal struct {
	Path       string        `json:"path"`
	Agent      string        `json:"agent,omitempty"`
	Candidates []string      `json:"candidates"`
	Selected   []string      `json:"selected"`
	Omitted    []string      `json:"omitted"`
	Scores     []budgetScore `json:"scores"`
	BudgetUsed int           `json:"budget_used"`
	BudgetMax  int           `json:"budget_max"`
}

func (j *budgetJournal) emit() {
	if j.Selected == nil {
		j.Selected = []string{}
	}
	if j.Omitted == nil {
		j.Omitted = []string{}
	}
	payload, err := json.Marshal(j)
	if err != nil {
		log.Printf("[budget] journal marshal failed: %v", err)
		return
	}
	log.Printf("[budget] %s", payload)
}

// priorityIndex maps P0..P3 to 0..3; any other value (including "") is 4,
// so it ranks after P3.
func priorityIndex(p string) int {
	switch p {
	case "P0":
		return 0
	case "P1":
		return 1
	case "P2":
		return 2
	case "P3":
		return 3
	}
	return 4
}

func utility(m models.Message, agentTagSet map[string]bool, now time.Time) float64 {
	// Priority score: P0=1, P1=0.67, P2=0.33, P3=0; unknown scores as P3
	priIdx := priorityIndex(m.Priority)
	if priIdx > 3 {
		priIdx = 3
	}
	priorityScore := 1.0 - float64(priIdx)/3.0

	// Freshness: hyperbolic decay over 1 hour
	freshnessScore := freshness(m.CreatedAt, now)

	// Tag Jaccard similarity; undefined when either side has no tags, in
	// which case its weight is redistributed proportionally.
	msgTags := extractTags(m.Metadata)
	if len(msgTags) == 0 || len(agentTagSet) == 0 {
		return 0.875*priorityScore + 0.125*freshnessScore
	}
	tagScore := jaccard(msgTags, agentTagSet)

	return 0.7*priorityScore + 0.2*tagScore + 0.1*freshnessScore
}

// freshness returns 1/(1+age_hours). An unparseable created_at scores 0;
// a future created_at is clamped to age 0 (score 1).
func freshness(createdAt string, now time.Time) float64 {
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		created, err = time.Parse(legacyCreatedAtLayout, createdAt)
		if err != nil {
			return 0
		}
	}
	age := now.Sub(created).Seconds()
	if age < 0 {
		age = 0
	}
	return 1.0 / (1.0 + age/3600.0)
}

func jaccard(msgTags map[string]bool, agentTags map[string]bool) float64 {
	if len(msgTags) == 0 || len(agentTags) == 0 {
		return 0
	}
	intersection := 0
	for t := range msgTags {
		if agentTags[t] {
			intersection++
		}
	}
	union := len(msgTags) + len(agentTags) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func extractTags(metadata string) map[string]bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return nil
	}
	raw, ok := m["tags"]
	if !ok {
		return nil
	}
	var tags []string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil
	}
	set := make(map[string]bool, len(tags))
	for _, t := range tags {
		set[t] = true
	}
	return set
}

func messageBytes(m models.Message) int {
	return len(m.ID) + len(m.From) + len(m.To) + len(m.Subject) + len(m.Content) + len(m.Metadata)
}

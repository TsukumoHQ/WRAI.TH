package relay

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"agent-relay/internal/db"
)

// evaluateExceptionLadders walks every open systemic exception up its frozen
// ladder (design 1111292b §4-§6, T2). Only in class_budget_mode=on: shadow and
// off do nothing. Per tick: start the ladder of new systemics, then for each
// active rung either fire its request (idempotent by tag), or read its
// outcome and resolve / advance. At most one step per systemic per tick.
func evaluateExceptionLadders(database *db.DB, notifier ackNotifier, now time.Time) {
	if database.GetSetting(db.SettingClassBudgetMode) != db.ClassBudgetModeOn {
		return
	}
	pending, err := database.PendingSystemics()
	if err != nil {
		log.Printf("[ladder] pending: %v", err)
	}
	for _, s := range pending {
		if _, err := database.StartLadder(s, now); err != nil {
			log.Printf("[ladder] start %s (%s/%s): %v", s.ID, s.Kind, s.ReasonCode, err)
		}
	}
	rungs, err := database.ActiveRungs()
	if err != nil {
		log.Printf("[ladder] active rungs: %v", err)
		return
	}
	for _, a := range rungs {
		if err := stepRung(database, notifier, a, now); err != nil {
			log.Printf("[ladder] %s rung %d (%s): %v", a.ExceptionID, a.Rung, a.Name(), err)
		}
	}
}

// ladderNext is the rung after the current one (the human is always last, so
// a non-human rung always has a next).
func ladderNext(a db.ActiveRung) int { return a.Rung + 1 }

func pastDeadline(a db.ActiveRung, now time.Time) bool {
	if a.DeadlineAt == "" {
		return false
	}
	d, err := time.Parse("2006-01-02T15:04:05.000000Z", a.DeadlineAt)
	return err == nil && now.After(d)
}

// stepRung performs one step of one systemic's current rung.
func stepRung(database *db.DB, notifier ackNotifier, a db.ActiveRung, now time.Time) error {
	name := a.Name()
	// Hard stop (design §5): once Niwa's attempts plus the ladder's own exceed
	// twice the class budget, jump straight to the supervisor.
	if sup := a.Snapshot.Index(db.RungSupervisorAgent); sup > a.Rung && a.Snapshot.TotalAttemptBudget > 0 &&
		a.Snapshot.UpstreamAttempts+a.Rung > 2*a.Snapshot.TotalAttemptBudget {
		_, err := database.AdvanceRung(a, db.ObligationInactive, map[string]any{"skipped": "hard_stop"}, sup, now)
		return err
	}
	switch name {
	case db.RungMatchPrecedent:
		ev := map[string]any{"precedent": database.PrecedentFor(a.ReasonCode, a.ExceptionID)}
		_, err := database.AdvanceRung(a, db.ObligationFulfilled, ev, ladderNext(a), now)
		return err
	case db.RungConsultKnowledge:
		return consultKnowledge(database, a, now)
	case db.RungWaitIfVolume:
		return waitIfVolume(database, a, now)
	case db.RungAskSource, db.RungSupervisorAgent, db.RungHuman:
		return messageRung(database, notifier, a, now)
	case db.RungRouteSpecialist, db.RungAdversarialReview, db.RungReversibleAction:
		return taskRung(database, a, now)
	}
	return fmt.Errorf("unknown rung %q", name)
}

// consultKnowledge searches constraints/decisions for the class (FTS5, no
// model) and carries the hits to the next rungs. It never resolves.
func consultKnowledge(database *db.DB, a db.ActiveRung, now time.Time) error {
	query := strings.ReplaceAll(a.ReasonCode, "_", " ")
	hits, _ := database.SearchMemoryRanked(a.Project, "relay-sweeper", query, nil, "", 5, false)
	var keys []string
	for _, h := range hits {
		if h.Layer == "constraints" || h.Layer == "decision" {
			keys = append(keys, h.Key)
		}
	}
	_, err := database.AdvanceRung(a, db.ObligationFulfilled, map[string]any{"knowledge_hits": keys}, ladderNext(a), now)
	return err
}

// waitIfVolume continues only while the class still produces more than n
// instances per window; a quiet window resolves the systemic as quiesced.
func waitIfVolume(database *db.DB, a db.ActiveRung, now time.Time) error {
	n, _ := strconv.Atoi(a.Params()["n"])
	minutes, _ := strconv.Atoi(a.Params()["minutes"])
	if minutes <= 0 {
		minutes = 24 * 60
	}
	since := now.Add(-time.Duration(minutes) * time.Minute).UTC().Format("2006-01-02T15:04:05.000000Z")
	vol := database.LinkedVolume(a.ExceptionID, since)
	switch {
	case vol > n:
		_, err := database.AdvanceRung(a, db.ObligationFulfilled, map[string]any{"volume": vol}, ladderNext(a), now)
		return err
	case vol == 0 && pastDeadline(a, now):
		_, err := database.ResolveAtRung(a, "self", "quiesced", map[string]any{"volume": 0}, now)
		return err
	}
	return nil
}

// taskRung dispatches the rung's typed ticket once, then reads the task.
func taskRung(database *db.DB, a db.ActiveRung, now time.Time) error {
	ref := a.ActionRef()
	if ref == "" {
		target, profile, why := taskTarget(database, a)
		if target == "" {
			_, err := database.AdvanceRung(a, db.ObligationInactive, map[string]any{"skipped": why}, ladderNext(a), now)
			return err
		}
		tag := db.LadderTag(a.ExceptionID, a.Rung)
		id := database.FindLadderTask(a.Project, tag)
		if id == "" {
			task, err := database.DispatchTask(a.Project, profile, "relay-sweeper", tag+" "+taskTitle(a),
				taskBody(a), "P2", nil, strPtrOrNil(a.Snapshot.Board), taskTicket(a), false, nil)
			if err != nil {
				// A persistent refusal (board, typed-ticket or title guard) must
				// not pin the ladder: past the deadline the rung fails and moves on.
				if pastDeadline(a, now) {
					_, aerr := database.AdvanceRung(a, db.ObligationUnfulfilled, map[string]any{"outcome": "dispatch_failed", "error": err.Error()}, ladderNext(a), now)
					return aerr
				}
				return fmt.Errorf("dispatch: %w", err)
			}
			id = task.ID
		}
		return database.SetRungAction(a, target, map[string]any{"action_ref": id, "profile": profile})
	}
	task, err := database.GetTask(ref, a.Project)
	if err != nil || task == nil {
		return fmt.Errorf("rung task %s: %v", ref, err)
	}
	switch task.Status {
	case "done":
		if next := ladderNext(a); next < len(a.Snapshot.Rungs) && a.Snapshot.Rungs[next].Rung == db.RungAdversarialReview {
			_, err := database.AdvanceRung(a, db.ObligationFulfilled, map[string]any{"outcome": "pending_review"}, next, now)
			return err
		}
		by := "peer"
		if a.Name() == db.RungReversibleAction {
			by = "self"
		}
		_, err := database.ResolveAtRung(a, by, "fixed", map[string]any{"outcome": "done"}, now)
		return err
	case "cancelled":
		_, err := database.AdvanceRung(a, db.ObligationUnfulfilled, map[string]any{"outcome": "cancelled"}, ladderNext(a), now)
		return err
	}
	if pastDeadline(a, now) {
		_, err := database.AdvanceRung(a, db.ObligationUnfulfilled, map[string]any{"outcome": "deadline"}, ladderNext(a), now)
		return err
	}
	return nil
}

// taskTarget picks who a task rung goes to: the doctrine owner (specialist,
// reversible action) or a reviewer that is neither the owner nor one of the
// reviewer profiles Niwa already used. why explains an empty target.
func taskTarget(database *db.DB, a db.ActiveRung) (agent, profile, why string) {
	reviewed := map[string]bool{}
	for _, p := range a.Snapshot.ReviewerProfiles {
		reviewed[p] = true
	}
	candidate := a.Owner
	if a.Name() == db.RungAdversarialReview {
		candidate = database.LiveSupervisor(a.Project, a.Owner)
	}
	if !database.IsLiveAgent(a.Project, candidate) {
		return "", "", "no_live_owner"
	}
	ag, err := database.GetAgent(a.Project, candidate)
	if err != nil || ag == nil || ag.ProfileSlug == nil || *ag.ProfileSlug == "" {
		return "", "", "owner_has_no_profile"
	}
	if reviewed[*ag.ProfileSlug] {
		return "", "", "profile_already_reviewed_upstream"
	}
	return ag.Name, *ag.ProfileSlug, ""
}

func taskTitle(a db.ActiveRung) string {
	switch a.Name() {
	case db.RungAdversarialReview:
		return "review the doctrine repair for recurring " + a.ReasonCode
	case db.RungReversibleAction:
		return fmt.Sprintf("run %s (compensate: %s) for recurring %s", a.Params()["tool"], a.Params()["compensate"], a.ReasonCode)
	}
	return "repair the doctrine behind recurring " + a.ReasonCode
}

func taskBody(a db.ActiveRung) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Systemic exception %s: class %s in project %s exceeded its rate budget.\n", a.ExceptionID, a.ReasonCode, a.Project)
	fmt.Fprintf(&b, "Evidence (instances, attribution): %s\n", a.SystemicEvidence)
	if a.Snapshot.UpstreamAttempts > 0 {
		fmt.Fprintf(&b, "Niwa already spent %d gate attempts on these instances: do not re-run a gate round.\n", a.Snapshot.UpstreamAttempts)
	}
	b.WriteString("Earlier ladder rungs are listed in the exception_attempts view.\n")
	return b.String()
}

func taskTicket(a db.ActiveRung) db.TypedTicket {
	goal := "The recurring exception class " + a.ReasonCode + " stops recurring because its doctrine is repaired."
	ac := `["Names the artifact changed (memory key, skill, profile or lane rule) and links the evidence","No gate round is re-run"]`
	if a.Name() == db.RungAdversarialReview {
		goal = "An independent review concurs with or rejects the doctrine repair for " + a.ReasonCode + "."
		ac = `["States concur or reject with the reason","Reviewer is not the repair's author"]`
	}
	return db.TypedTicket{Goal: goal, AcceptanceCriteria: ac, Dod: "Task done with the result naming the change; the relay resolves the systemic on done."}
}

// messageRung sends ask_source / supervisor_agent / human once, then reads
// the replies. No answer obligation is opened on these messages: the ladder
// is the escalation, and the answer chain would reach the human early.
func messageRung(database *db.DB, notifier ackNotifier, a db.ActiveRung, now time.Time) error {
	ref := a.ActionRef()
	if ref == "" {
		to, why := messageTarget(database, a)
		if to == "" {
			_, err := database.AdvanceRung(a, db.ObligationInactive, map[string]any{"skipped": why}, ladderNext(a), now)
			return err
		}
		id, err := sendRungMessage(database, notifier, a, to, "")
		if err != nil {
			if pastDeadline(a, now) && a.Name() != db.RungHuman {
				_, aerr := database.AdvanceRung(a, db.ObligationUnfulfilled, map[string]any{"outcome": "send_failed", "error": err.Error()}, ladderNext(a), now)
				return aerr
			}
			return err
		}
		return database.SetRungAction(a, to, map[string]any{"action_ref": id})
	}
	replies, err := database.RepliesTo(ref)
	if err != nil {
		return err
	}
	if a.Name() == db.RungHuman {
		return humanOutcome(database, notifier, a, replies, now)
	}
	for _, r := range replies {
		if !strings.EqualFold(r.From, a.Bearer) {
			continue
		}
		var meta struct {
			Verdict string `json:"verdict"`
		}
		_ = json.Unmarshal([]byte(r.Metadata), &meta)
		if meta.Verdict == "fixed" {
			by := "source"
			if a.Name() == db.RungSupervisorAgent {
				by = "supervisor"
			}
			_, err := database.ResolveAtRung(a, by, "fixed", map[string]any{"reply": r.ID}, now)
			return err
		}
		_, err := database.AdvanceRung(a, db.ObligationFulfilled, map[string]any{"reply": r.ID, "verdict": meta.Verdict}, ladderNext(a), now)
		return err
	}
	if pastDeadline(a, now) {
		_, err := database.AdvanceRung(a, db.ObligationUnfulfilled, map[string]any{"outcome": "no_answer"}, ladderNext(a), now)
		return err
	}
	return nil
}

// messageTarget: ask_source → the live top raiser; supervisor_agent → a live
// supervisor (owner's reports_to, project executive, any live executive —
// never the founder, never an inactive agent); human → the operator.
func messageTarget(database *db.DB, a db.ActiveRung) (string, string) {
	switch a.Name() {
	case db.RungAskSource:
		if s := database.TopRaiser(a.Project, a.ExceptionID); s != "" {
			return s, ""
		}
		return "", "no_live_source"
	case db.RungSupervisorAgent:
		if s := database.LiveSupervisor(a.Project, a.Owner); s != "" {
			return s, ""
		}
		return "", "no_live_supervisor"
	case db.RungHuman:
		return ackFounder, ""
	}
	return "", "not_a_message_rung"
}

func sendRungMessage(database *db.DB, notifier ackNotifier, a db.ActiveRung, to, note string) (string, error) {
	tag := db.LadderTag(a.ExceptionID, a.Rung)
	if note == "" {
		if id := database.FindLadderMessage(a.Project, tag); id != "" {
			return id, nil
		}
	}
	var subject, body, action string
	meta := map[string]any{"exception_id": a.ExceptionID, "rung": a.Rung}
	switch a.Name() {
	case db.RungAskSource:
		action = "ask"
		subject = tag + " recurring " + a.ReasonCode + ": is it fixed?"
		body = fmt.Sprintf("You raised most instances of the recurring class %s (systemic %s). Reply with metadata {\"verdict\": \"fixed\"|\"known\"|\"needs_fix\", \"note\": \"...\"}.", a.ReasonCode, a.ExceptionID)
		meta["schema"] = map[string]any{"verdict": []string{"fixed", "known", "needs_fix"}}
	case db.RungSupervisorAgent:
		action = "decide"
		subject = tag + " recurring " + a.ReasonCode + " reached you"
		body = fmt.Sprintf("Systemic %s (%s) was not resolved by the earlier agent rungs (see exception_attempts). Reply with metadata {\"verdict\": \"fixed\"} once repaired, or any other verdict to escalate.", a.ExceptionID, a.ReasonCode)
		meta["schema"] = map[string]any{"verdict": []string{"fixed", "escalate"}}
	default:
		action = "decide"
		subject = tag + " decide: recurring " + a.ReasonCode
		body = fmt.Sprintf("Systemic %s (%s) exhausted every agent rung (see exception_attempts). Reply with metadata %s.", a.ExceptionID, a.ReasonCode, humanSchemaHint)
		meta["schema"] = humanSchema
	}
	if note != "" {
		body = note + "\n" + body
	}
	meta["evidence"] = json.RawMessage(nonEmptyJSON(a.SystemicEvidence))
	raw, _ := json.Marshal(meta)
	msg, _, err := database.InsertMessageWithDeliveries(a.Project, "relay", to, "notification", subject, body, string(raw),
		"P1", -1, nil, nil, []string{to}, action)
	if err != nil {
		return "", fmt.Errorf("send %s: %w", a.Name(), err)
	}
	notifier.Notify(a.Project, to, "relay", subject, msg.ID)
	return msg.ID, nil
}

func nonEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" || !json.Valid([]byte(s)) {
		return "{}"
	}
	return s
}

// humanSchema is the terminal rung's inquiry schema (design §6).
var humanSchema = map[string]any{
	"decision":    []string{"fix_doctrine", "accept_known", "wont_fix", "reassign"},
	"scope":       []string{"lane", "project", "global"},
	"generalize":  "boolean",
	"expires_in":  "duration, max 90d (e.g. 72h, 30d)",
	"reassign_to": "agent, required when decision=reassign",
}

const humanSchemaHint = `{"decision": "fix_doctrine|accept_known|wont_fix|reassign", "scope": "lane|project|global", "generalize": true|false, "expires_in": "30d", "reassign_to": "<agent, if reassign>"}`

// HumanDecision is a validated human rung answer.
type HumanDecision struct {
	Decision   string `json:"decision"`
	Scope      string `json:"scope"`
	Generalize *bool  `json:"generalize"`
	ExpiresIn  string `json:"expires_in"`
	ReassignTo string `json:"reassign_to"`
	expires    time.Duration
}

// parseHumanDecision validates reply metadata against humanSchema.
func parseHumanDecision(metadata string) (HumanDecision, error) {
	var h HumanDecision
	if err := json.Unmarshal([]byte(metadata), &h); err != nil {
		return h, fmt.Errorf("metadata is not a JSON object")
	}
	switch h.Decision {
	case "fix_doctrine", "accept_known", "wont_fix", "reassign":
	default:
		return h, fmt.Errorf("decision must be one of fix_doctrine|accept_known|wont_fix|reassign")
	}
	switch h.Scope {
	case "lane", "project", "global":
	default:
		return h, fmt.Errorf("scope must be one of lane|project|global")
	}
	if h.Generalize == nil {
		return h, fmt.Errorf("generalize (true|false) is required")
	}
	d, err := parseExpiry(h.ExpiresIn)
	if err != nil {
		return h, err
	}
	h.expires = d
	if h.Decision == "reassign" && strings.TrimSpace(h.ReassignTo) == "" {
		return h, fmt.Errorf("reassign_to is required when decision=reassign")
	}
	return h, nil
}

func parseExpiry(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("expires_in is required (e.g. 72h, 30d)")
	}
	var d time.Duration
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("expires_in %q is not a duration", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("expires_in %q is not a duration", s)
		}
	}
	if d <= 0 || d > 90*24*time.Hour {
		return 0, fmt.Errorf("expires_in must be > 0 and <= 90d")
	}
	return d, nil
}

// humanOutcome applies the first valid operator answer; an invalid answer
// gets one re-ask quoting the error; the TTL expires the systemic.
func humanOutcome(database *db.DB, notifier ackNotifier, a db.ActiveRung, replies []db.LadderReply, now time.Time) error {
	var lastErr error
	var lastReply string
	for _, r := range replies {
		if !strings.EqualFold(r.From, ackFounder) {
			continue
		}
		h, err := parseHumanDecision(r.Metadata)
		if err != nil {
			lastErr, lastReply = err, r.ID
			continue
		}
		ev := map[string]any{"reply": r.ID, "decision": h.Decision, "scope": h.Scope, "generalize": *h.Generalize, "expires_in": h.ExpiresIn}
		switch h.Decision {
		case "accept_known":
			until := now.Add(h.expires).UTC().Format("2006-01-02T15:04:05.000000Z")
			if err := database.SuppressClass(database.SystemicKind(a.ExceptionID), a.ReasonCode, until); err != nil {
				return fmt.Errorf("suppress: %w", err)
			}
			ev["suppressed_until"] = until
			_, err := database.ResolveAtRung(a, "human", "known", ev, now)
			return err
		case "wont_fix":
			_, err := database.ResolveAtRung(a, "human", "wont_fix", ev, now)
			return err
		case "reassign":
			ev["reassign_to"] = h.ReassignTo
			_, err := database.ResolveAtRung(a, "human", "reassigned", ev, now)
			return err
		default:
			_, err := database.ResolveAtRung(a, "human", "fixed", ev, now)
			return err
		}
	}
	if lastErr != nil {
		if reasked, _ := a.Evidence["reasked"].(bool); !reasked {
			id, err := sendRungMessage(database, notifier, a, ackFounder,
				fmt.Sprintf("Your answer %s could not be applied: %v.", lastReply, lastErr))
			if err != nil {
				return err
			}
			return database.SetRungAction(a, ackFounder, map[string]any{"action_ref": id, "reasked": true, "invalid_reply": lastReply})
		}
	}
	if pastDeadline(a, now) {
		_, err := database.ResolveAtRung(a, "expired", "expired", map[string]any{"outcome": "ttl"}, now)
		return err
	}
	return nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

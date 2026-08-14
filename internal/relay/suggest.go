package relay

import (
	"fmt"
	"sort"
	"strings"
)

// levenshtein returns the edit distance between a and b, compared
// case-insensitively. Classic iterative two-row implementation — no
// external dependency needed for a handful of short agent names.
func levenshtein(a, b string) int {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	ra, rb := []rune(a), []rune(b)

	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			min := del
			if ins < min {
				min = ins
			}
			if sub < min {
				min = sub
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

// nearbyAgentNames returns the names in candidates that are a plausible "did
// you mean" match for target, combining two independent heuristics:
//
//  1. Levenshtein distance <= maxDist (typos / small edits) — the original
//     behavior.
//  2. Token-aware match (see tokenAwareMatch): catches token-insertion
//     aliases like 'frontend-lead' -> 'frontend-tech-lead', which sit 5 edits
//     apart (Levenshtein misses them entirely) but are exactly the target's
//     tokens plus one inserted in the middle.
//
// Levenshtein matches are listed first, sorted by (distance ascending, name
// ascending); token-only matches follow, sorted by name. A candidate that
// qualifies both ways appears once, in the Levenshtein position — priority
// goes to the stronger (edit-distance) signal. The combined list is capped
// at max entries. Used to produce a "did you mean" hint when a send_message
// recipient doesn't resolve to a registered agent.
func nearbyAgentNames(candidates []string, target string, maxDist, max int) []string {
	type scored struct {
		name string
		dist int
	}
	var levMatches []scored
	levSeen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		d := levenshtein(c, target)
		if d <= maxDist {
			levMatches = append(levMatches, scored{name: c, dist: d})
			levSeen[strings.ToLower(c)] = true
		}
	}
	sort.Slice(levMatches, func(i, j int) bool {
		if levMatches[i].dist != levMatches[j].dist {
			return levMatches[i].dist < levMatches[j].dist
		}
		return levMatches[i].name < levMatches[j].name
	})

	targetTokens := strings.Split(strings.ToLower(target), "-")
	var tokenMatches []string
	for _, c := range candidates {
		if levSeen[strings.ToLower(c)] {
			continue // already ranked via the stronger Levenshtein signal
		}
		if tokenAwareMatch(targetTokens, strings.Split(strings.ToLower(c), "-")) {
			tokenMatches = append(tokenMatches, c)
		}
	}
	sort.Strings(tokenMatches)

	names := make([]string, 0, len(levMatches)+len(tokenMatches))
	for _, m := range levMatches {
		names = append(names, m.name)
	}
	names = append(names, tokenMatches...)
	if len(names) > max {
		names = names[:max]
	}
	return names
}

// tokenAwareMatch reports whether candTokens is a plausible token-insertion
// alias of targetTokens: every token of targetTokens must appear in
// candTokens, in order, as a (not necessarily contiguous) subsequence — and
// target/candidate must share their first token or their last token, which
// keeps an unrelated candidate that happens to contain the same words in a
// different role from matching. Both slices are expected pre-lowercased.
// Example: targetTokens=[frontend,lead], candTokens=[frontend,tech,lead] ->
// true (frontend...lead is a subsequence, first token shared).
func tokenAwareMatch(targetTokens, candTokens []string) bool {
	if len(targetTokens) == 0 || len(candTokens) == 0 {
		return false
	}
	sameFirst := targetTokens[0] == candTokens[0]
	sameLast := targetTokens[len(targetTokens)-1] == candTokens[len(candTokens)-1]
	if !sameFirst && !sameLast {
		return false
	}
	i := 0
	for _, tok := range candTokens {
		if i < len(targetTokens) && tok == targetTokens[i] {
			i++
		}
	}
	return i == len(targetTokens)
}

// unknownRecipientError builds the rejection message for a send_message 'to'
// that doesn't resolve to a registered active agent in project, appending a
// "did you mean" hint (Levenshtein distance <= 2, up to 3 names) when a
// close match exists among that project's known agents. Best-effort: a
// lookup failure while building suggestions is silently ignored — the
// rejection itself still happens.
func (h *Handlers) unknownRecipientError(project, to string) string {
	msg := fmt.Sprintf("unknown recipient '%s' — not a registered active agent, '*', 'team:<slug>', or conversation_id.", to)
	if candidates, err := h.db.ListAgents(project); err == nil {
		names := make([]string, len(candidates))
		for i, a := range candidates {
			names[i] = a.Name
		}
		if sugg := nearbyAgentNames(names, to, 2, 3); len(sugg) > 0 {
			msg += fmt.Sprintf(" Did you mean: %s?", strings.Join(sugg, ", "))
		}
	}
	return msg
}

// unknownCrossProjectRecipientError is the sendCrossProject counterpart of
// unknownRecipientError: same rejection semantics (nil or soft-deleted
// recipient), suggestions drawn from the TARGET project's roster rather than
// the caller's, and the message names that target project explicitly since
// it isn't the project the caller is sitting in.
func (h *Handlers) unknownCrossProjectRecipientError(dstProject, to string) string {
	msg := fmt.Sprintf("unknown recipient '%s' — not a registered active agent in project '%s'.", to, dstProject)
	if candidates, err := h.db.ListAgents(dstProject); err == nil {
		names := make([]string, len(candidates))
		for i, a := range candidates {
			names[i] = a.Name
		}
		if sugg := nearbyAgentNames(names, to, 2, 3); len(sugg) > 0 {
			msg += fmt.Sprintf(" Did you mean: %s?", strings.Join(sugg, ", "))
		}
	}
	return msg
}

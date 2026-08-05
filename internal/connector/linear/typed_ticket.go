package linear

import "strings"

// ticket is the typed-ticket (V-lifecycle) parsed out of a Linear issue
// description, plus which required sections were absent. The format is the same
// markdown a relay dispatch carries (see docs/typed-tickets.md): the discipline
// lives in the issue body itself — no Linear custom fields, no per-workspace
// config, human-readable.
type ticket struct {
	goal       string
	acceptance []string
	dod        string
	missing    []string // subset of {"goal","acceptance_criteria","dod"}, dispatch order
}

// parseTicket reads the Goal / Acceptance Criteria / DoD markdown sections from
// a Linear issue description. Headers are matched case-insensitively at any
// depth ("#", "##", "### "); Acceptance Criteria must carry ≥1 bullet item.
func parseTicket(description string) ticket {
	secs := splitSections(description)
	var t ticket
	t.goal = strings.TrimSpace(secs["goal"])
	t.dod = strings.TrimSpace(secs["dod"])
	t.acceptance = bulletItems(secs["acceptance criteria"])

	if t.goal == "" {
		t.missing = append(t.missing, "goal")
	}
	if len(t.acceptance) == 0 {
		t.missing = append(t.missing, "acceptance_criteria")
	}
	if t.dod == "" {
		t.missing = append(t.missing, "dod")
	}
	return t
}

// splitSections maps a canonical section key → the raw body under its header.
// Canonical keys: "goal", "acceptance criteria", "dod". Synonyms fold in
// ("definition of done" → "dod", "acceptance criterion" → "acceptance criteria").
func splitSections(description string) map[string]string {
	secs := map[string]string{}
	cur := ""
	var body []string
	flush := func() {
		if cur != "" {
			secs[cur] = strings.TrimSpace(strings.Join(body, "\n"))
		}
		body = body[:0]
	}
	for _, line := range strings.Split(description, "\n") {
		if title, ok := headerTitle(line); ok {
			flush()
			cur = canonSection(title)
			continue
		}
		if cur != "" {
			body = append(body, line)
		}
	}
	flush()
	return secs
}

// headerTitle returns the text of a markdown ATX header ("## Goal" → "Goal"),
// or ("", false) if the line is not a header.
func headerTitle(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "#") {
		return "", false
	}
	s = strings.TrimLeft(s, "#")
	if !strings.HasPrefix(s, " ") && s != "" {
		return "", false // "#foo" is not a header, "# foo" is
	}
	return strings.TrimSpace(s), true
}

// canonSection folds a header title to a canonical section key (lowercased).
func canonSection(title string) string {
	switch strings.ToLower(strings.TrimSpace(title)) {
	case "goal":
		return "goal"
	case "acceptance criteria", "acceptance criterion", "acceptance":
		return "acceptance criteria"
	case "dod", "definition of done":
		return "dod"
	default:
		return strings.ToLower(strings.TrimSpace(title))
	}
}

// bulletItems extracts the non-blank list items under a section body. A line is
// an item when it starts with a bullet ("-", "*", "+") or an ordered marker
// ("1." / "1)"). Non-list lines are ignored, so prose between bullets is fine.
func bulletItems(body string) []string {
	var items []string
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		item, ok := stripBullet(s)
		if !ok {
			continue
		}
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func stripBullet(s string) (string, bool) {
	for _, m := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(s, m) {
			return s[len(m):], true
		}
	}
	// Ordered marker: leading digits then "." or ")" then a space.
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') {
		rest := s[i+1:]
		if strings.HasPrefix(rest, " ") {
			return rest[1:], true
		}
	}
	return "", false
}

// refusalComment is the loud rejection posted back to the Linear issue when a
// required section is missing on a typed-ticket project. It names the missing
// sections and points at the format doc — never a silent relay log.
func refusalComment(missing []string) string {
	return "⛔ **Typed ticket required — this issue was NOT dispatched.**\n\n" +
		"Missing section(s): **" + strings.Join(missing, ", ") + "**.\n\n" +
		"Add these markdown sections to the issue description:\n\n" +
		"```\n## Goal\n<one-line intent>\n\n## Acceptance Criteria\n- <testable item>\n- <testable item>\n\n## DoD\n<definition of done>\n```\n\n" +
		"Format: `docs/typed-tickets.md`. The task will mirror and dispatch automatically once the description carries all three sections."
}

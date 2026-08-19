package relay

import (
	"encoding/json"
	"strings"
)

// Typed-status caps (DEC-relay-comms-discipline-1 mechanism B). These are
// accept-and-slot safety limits, NOT rejection gates: an over-long slot or note
// is truncated and the overflow recorded, the status is NEVER refused. The GUARD
// from the decision — never cap/gate a question or a real blocker MESSAGE — is
// upheld because send_status is a passive 'status' report (no-wake, surfaced not
// woken per Ruling-3); an agent escalates a blocker via a question, which this
// path never touches.
const (
	statusMaxSlotItems = 20  // per slot (done/doing/blockers)
	statusMaxItemBytes = 200 // per item
	statusMaxNoteBytes = 280 // the single free-text valve
	statusSchema       = 1
)

// statusPayload is the canonical typed-status document stored in a status
// message's metadata. Truncated records what accept-and-slot had to clip so the
// reader knows the view is partial (omitted entirely when nothing was clipped).
type statusPayload struct {
	StatusSchema int              `json:"status_schema"`
	Done         []string         `json:"done"`
	Doing        []string         `json:"doing"`
	Blockers     []string         `json:"blockers"`
	Note         string           `json:"note,omitempty"`
	Truncated    *statusTruncated `json:"truncated,omitempty"`
}

type statusTruncated struct {
	Done     int  `json:"done,omitempty"`     // items dropped past the cap
	Doing    int  `json:"doing,omitempty"`    //
	Blockers int  `json:"blockers,omitempty"` //
	Items    int  `json:"items,omitempty"`    // individual items whose text was clipped
	Note     bool `json:"note,omitempty"`     // note text was clipped
}

// capSlot trims blanks, clips each item to statusMaxItemBytes (rune-safe), drops
// empties, and caps the item count. Returns the kept items, the count dropped
// past the cap, and how many individual items were text-clipped. Never errors —
// a nil/empty input yields an empty (non-nil) slice so the JSON slot is [].
func capSlot(items []string) (kept []string, dropped, clipped int) {
	kept = []string{}
	for _, raw := range items {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if len(kept) >= statusMaxSlotItems {
			dropped++
			continue
		}
		if cut, did := truncatePreview(s, statusMaxItemBytes); did {
			s = cut
			clipped++
		}
		kept = append(kept, s)
	}
	return kept, dropped, clipped
}

// buildStatusPayload slots the three lists + note into the canonical document,
// applying the accept-and-slot caps. Returns a human-readable content rendering
// (so get_inbox shows the status without a metadata parse), the metadata JSON,
// and the kept-blocker count (surfaced, never a wake trigger).
func buildStatusPayload(done, doing, blockers []string, note string) (content, metadata string, blockerCount int) {
	var tr statusTruncated
	var cd, cg, cb int

	p := statusPayload{StatusSchema: statusSchema}
	p.Done, tr.Done, cd = capSlot(done)
	p.Doing, tr.Doing, cg = capSlot(doing)
	p.Blockers, tr.Blockers, cb = capSlot(blockers)
	tr.Items = cd + cg + cb

	if n := strings.TrimSpace(note); n != "" {
		if cut, did := truncatePreview(n, statusMaxNoteBytes); did {
			n = cut
			tr.Note = true
		}
		p.Note = n
	}

	if tr != (statusTruncated{}) {
		p.Truncated = &tr
	}

	b, err := json.Marshal(p)
	if err != nil {
		// Marshal of plain strings/ints cannot realistically fail; degrade to an
		// empty-but-valid document rather than propagate an error to the caller.
		b = []byte(`{"status_schema":1,"done":[],"doing":[],"blockers":[]}`)
	}
	return renderStatusContent(p), string(b), len(p.Blockers)
}

// renderStatusContent produces the inbox-visible text. Empty slots are omitted;
// an all-empty status still renders a marker so it is never a blank body.
func renderStatusContent(p statusPayload) string {
	var lines []string
	lines = append(lines, "STATUS")
	if len(p.Done) > 0 {
		lines = append(lines, "DONE: "+strings.Join(p.Done, "; "))
	}
	if len(p.Doing) > 0 {
		lines = append(lines, "DOING: "+strings.Join(p.Doing, "; "))
	}
	if len(p.Blockers) > 0 {
		lines = append(lines, "BLOCKERS: "+strings.Join(p.Blockers, "; "))
	}
	if p.Note != "" {
		lines = append(lines, "NOTE: "+p.Note)
	}
	if len(lines) == 1 {
		lines = append(lines, "(no items)")
	}
	return strings.Join(lines, "\n")
}

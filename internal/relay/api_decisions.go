package relay

import (
	"net/http"
)

// apiDecisionGraph serves GET /api/decisions/graph?project=<p> — the living-ADR
// graph (steal #11) rendered as a mermaid flowchart string. Read-only; a
// version-controlled, GitHub-native view of the causal decision ledger.
func (r *Relay) apiDecisionGraph(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	mermaid, err := r.DB.DecisionGraphMermaid(project)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to render decision graph", err)
		return
	}
	writeJSON(w, map[string]any{"project": project, "mermaid": mermaid})
}

// apiRelevantDecisions serves GET /api/decisions/relevant?project=<p>&area=<a> —
// the gate-check read (steal #11): the LIVE decisions governing an area, so the
// QA gate can check whether a change contradicts a settled decision. An empty
// area returns all live decisions. Read-only.
func (r *Relay) apiRelevantDecisions(w http.ResponseWriter, req *http.Request) {
	project := projectFromRequest(req)
	area := req.URL.Query().Get("area")
	decs, err := r.DB.RelevantDecisions(project, area)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to read decisions", err)
		return
	}
	writeJSON(w, map[string]any{"project": project, "area": area, "decisions": decs, "count": len(decs)})
}

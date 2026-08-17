package relay

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// NormalizeProject canonicalizes a project identifier: trimmed, lowercased,
// underscores folded to hyphens. One repo checked out as `synergix_prod`
// (underscore) while its role skills registered on `synergix-prod` (hyphen)
// split every agent across two namespaces — the watchdog then listened on
// the wrong one and dispatches never woke anyone. Folding at every entry
// point makes the two spellings one project. The remote suffix (`name@host`)
// passes through untouched apart from casing.
func NormalizeProject(p string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(p)), "_", "-")
}

// resolveProject returns the project for a tool call, most explicit first:
//
//  1. the `project` tool parameter;
//  2. the HTTP session project (?project= URL param);
//  3. the calling agent's own registration, when it names exactly ONE
//     project — an agent whose MCP connection carries no project would
//     otherwise be unresolved even though it belongs to exactly one.
//
// Returns "" when nothing resolves. There is no "default" catch-all: an
// unresolved project is an error at the write boundary (guardIdentity), not a
// shared bucket that silently collects untargeted writes.
func (h *Handlers) resolveProject(ctx context.Context, req mcp.CallToolRequest) string {
	if p := req.GetString("project", ""); p != "" {
		return NormalizeProject(p)
	}
	if ctxProject := ProjectFromContext(ctx); ctxProject != "" {
		return NormalizeProject(ctxProject)
	}
	if agent := resolveAgent(ctx, req); agent != "" {
		if projects, err := h.db.ProjectsOfAgent(agent); err == nil && len(projects) == 1 {
			return NormalizeProject(projects[0])
		}
	}
	return ""
}

// suggestProject returns the closest existing project name to `miss`
// (normalized-equal first, then smallest edit distance ≤ 3), or "" when
// nothing is close enough to be a plausible typo.
func (h *Handlers) suggestProject(miss string) string {
	names, err := h.db.ProjectNames()
	if err != nil {
		return ""
	}
	norm := NormalizeProject(miss)
	best, bestDist := "", 4
	for _, name := range names {
		candidate := NormalizeProject(name)
		if candidate == norm {
			return name
		}
		if d := editDistance(norm, candidate); d < bestDist {
			best, bestDist = name, d
		}
	}
	return best
}

// editDistance is plain Levenshtein over bytes — project names are ASCII
// slugs, and both inputs arrive normalized.
func editDistance(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

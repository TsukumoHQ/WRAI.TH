package relay

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ctxProject returns a context scoped to a project, as HTTPContextFunc would set
// it on a real connection.
func ctxProject(project string) context.Context {
	return context.WithValue(context.Background(), projectKey, project)
}

// readResource decodes a resource handler's single JSON text body.
func readResource(t *testing.T, contents []mcp.ResourceContents, err error, uri string) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("resource read error: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("want 1 resource content, got %d", len(contents))
	}
	tc, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("resource content is not text: %T", contents[0])
	}
	if tc.URI != uri || tc.MIMEType != "application/json" {
		t.Fatalf("unexpected uri/mime: %q %q", tc.URI, tc.MIMEType)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &body); err != nil {
		t.Fatalf("resource body not JSON: %v\n%s", err, tc.Text)
	}
	return body
}

func readReq(uri string) mcp.ReadResourceRequest {
	var r mcp.ReadResourceRequest
	r.Params.URI = uri
	return r
}

// The catalogs surface real project data without a tool call.
func TestResourceCatalogsReflectProjectData(t *testing.T) {
	h := testHandlers(t)
	registerActive(t, h, "p1", "bot-a", nil)
	disp, _ := h.HandleDispatchTask(ctx, call(map[string]any{
		"project": "p1", "as": "bot-a", "profile": "dev", "title": "catalog me",
	}))
	if disp.IsError {
		t.Fatalf("dispatch failed: %s", expectError(t, disp))
	}
	c := ctxProject("p1")

	tc, terr := h.resourceTasks(c, readReq(resourceTasksURI))
	tasks := readResource(t, tc, terr, resourceTasksURI)
	if tasks["project"] != "p1" {
		t.Fatalf("tasks catalog not scoped to p1: %v", tasks)
	}
	trows, _ := tasks["tasks"].([]any)
	if len(trows) == 0 {
		t.Fatalf("task catalog empty, expected the dispatched task: %v", tasks)
	}

	ac, aerr := h.resourceAgents(c, readReq(resourceAgentsURI))
	agents := readResource(t, ac, aerr, resourceAgentsURI)
	arows, _ := agents["agents"].([]any)
	if len(arows) == 0 {
		t.Fatalf("agent roster empty, expected bot-a: %v", agents)
	}

	bc, berr := h.resourceBoards(c, readReq(resourceBoardsURI))
	boards := readResource(t, bc, berr, resourceBoardsURI)
	if _, ok := boards["boards"]; !ok {
		t.Fatalf("boards catalog missing boards key: %v", boards)
	}

	mc, merr := h.resourceMemory(c, readReq(resourceMemoryURI))
	mem := readResource(t, mc, merr, resourceMemoryURI)
	if _, ok := mem["decisions"]; !ok {
		t.Fatalf("memory catalog missing decisions: %v", mem)
	}
	if _, ok := mem["memory_index"]; !ok {
		t.Fatalf("memory catalog missing memory_index: %v", mem)
	}
}

// The memory index must NEVER include a memory's value (compact index only).
func TestMemoryResourceIndexHasNoValues(t *testing.T) {
	h := testHandlers(t)
	if _, err := h.db.SetMemory("p1", "bot-a", "secret-key", "SENSITIVE VALUE", "[]", "project", "stated", "context"); err != nil {
		t.Fatalf("set memory: %v", err)
	}
	mc, merr := h.resourceMemory(ctxProject("p1"), readReq(resourceMemoryURI))
	mem := readResource(t, mc, merr, resourceMemoryURI)
	idx, _ := mem["memory_index"].([]any)
	if len(idx) == 0 {
		t.Fatalf("memory_index empty: %v", mem)
	}
	for _, row := range idx {
		m, _ := row.(map[string]any)
		if _, hasValue := m["value"]; hasValue {
			t.Fatalf("memory_index leaked a value field: %v", m)
		}
	}
}

// A connection with no project gets a hint, never another project's data.
func TestResourceUnscopedReturnsHint(t *testing.T) {
	h := testHandlers(t)
	tc, terr := h.resourceTasks(ctxProject(""), readReq(resourceTasksURI))
	body := readResource(t, tc, terr, resourceTasksURI)
	if body["project"] != "" || body["note"] == nil {
		t.Fatalf("unscoped read should return an empty-project hint: %v", body)
	}
}

// Registration is additive and read-only: RegisterResources wires all four
// catalogs onto a server that advertises resource capability.
func TestRegisterResourcesWiresCatalogs(t *testing.T) {
	if len(resourceCatalogURIs) != 4 {
		t.Fatalf("expected 4 catalog URIs, got %d", len(resourceCatalogURIs))
	}
	h := testHandlers(t)
	srv := server.NewMCPServer("test", "0.0.1", server.WithResourceCapabilities(false, false))
	// Must not panic and must register without touching tools.
	h.RegisterResources(srv)
}

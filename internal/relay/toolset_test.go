package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestDiscoverTools(t *testing.T) {
	h := testHandlers(t)

	res, err := h.HandleDiscoverTools(context.Background(), call(map[string]any{"category": "messaging"}))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	data := parseJSON(t, res)
	if data["category"] != "messaging" {
		t.Errorf("category = %v", data["category"])
	}
	tools := data["tools"].([]any)
	if len(tools) != 6 {
		t.Errorf("messaging tools = %d, want 6", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] == "" || first["inputSchema"] == nil {
		t.Errorf("tool schema incomplete: %v", first)
	}

	res, _ = h.HandleDiscoverTools(context.Background(), call(map[string]any{"category": "nope"}))
	expectError(t, res)
}

func TestCallToolDispatch(t *testing.T) {
	h := testHandlers(t)

	// Register an agent through the dispatcher, args as object
	res, err := h.HandleCallTool(context.Background(), call(map[string]any{
		"tool": "register_agent",
		"args": map[string]any{"name": "scout", "project": "p1"},
	}))
	if err != nil {
		t.Fatalf("call_tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("call_tool error: %s", res.Content[0].(mcp.TextContent).Text)
	}

	// Verify it really registered, args as JSON string (fallback path)
	res, err = h.HandleCallTool(context.Background(), call(map[string]any{
		"tool": "list_agents",
		"args": `{"project":"p1","format":"json"}`,
	}))
	if err != nil {
		t.Fatalf("call_tool list: %v", err)
	}
	data := parseJSON(t, res)
	if int(data["count"].(float64)) != 1 {
		t.Errorf("agent count = %v, want 1", data["count"])
	}

	// Unknown tool
	res, _ = h.HandleCallTool(context.Background(), call(map[string]any{"tool": "no_such_tool"}))
	expectError(t, res)
}

func TestToolsModeFilter(t *testing.T) {
	h := testHandlers(t)
	var all []mcp.Tool
	for _, rt := range h.toolRegistry() {
		all = append(all, rt.Tool)
	}
	all = append(all, discoverToolsTool(), callToolTool())

	// Full mode is now opt-in: it must be explicitly requested.
	fullCtx := context.WithValue(context.Background(), toolsModeKey, ToolsModeFull)
	full := toolsModeFilter(fullCtx, all)
	if len(full) != len(all)-2 {
		t.Errorf("full mode: %d tools, want %d (discovery pair hidden)", len(full), len(all)-2)
	}

	// Discovery is the default: a bare context resolves to discovery. It exposes
	// the discovery pair PLUS the onboarding core (so create_project works
	// directly without paying the full list). Count = 2 + core tools present.
	discCtx := context.Background()
	disc := toolsModeFilter(discCtx, all)
	wantDisc := 2
	got := map[string]bool{}
	for _, t := range disc {
		got[t.Name] = true
	}
	for name := range coreDiscoveryTools {
		for _, t := range all {
			if t.Name == name {
				wantDisc++
				break
			}
		}
	}
	if len(disc) != wantDisc {
		t.Errorf("discovery mode: %d tools, want %d (discovery pair + core)", len(disc), wantDisc)
	}
	if !got["discover_tools"] || !got["call_tool"] || !got["create_project"] {
		t.Errorf("discovery must expose discover_tools + call_tool + create_project; got %v", got)
	}
}

func TestHTTPContextFuncToolsMode(t *testing.T) {
	// Explicit full opts out of the discovery default.
	req := &http.Request{URL: &url.URL{RawQuery: "project=p1&tools=full"}}
	ctx := HTTPContextFunc(context.Background(), req)
	if ToolsModeFromContext(ctx) != ToolsModeFull {
		t.Error("tools=full not propagated")
	}

	// No tools param → discovery (the default).
	req = &http.Request{URL: &url.URL{RawQuery: "project=p1"}}
	ctx = HTTPContextFunc(context.Background(), req)
	if ToolsModeFromContext(ctx) != ToolsModeDiscovery {
		t.Error("default mode should be discovery")
	}

	// Explicit discovery is also honored.
	req = &http.Request{URL: &url.URL{RawQuery: "project=p1&tools=discovery"}}
	ctx = HTTPContextFunc(context.Background(), req)
	if ToolsModeFromContext(ctx) != ToolsModeDiscovery {
		t.Error("tools=discovery not propagated")
	}
}

// Sanity: the discovery payload for the largest category stays bounded.
func TestDiscoverPayloadSize(t *testing.T) {
	h := testHandlers(t)
	for _, c := range toolCategories {
		res, err := h.HandleDiscoverTools(context.Background(), call(map[string]any{"category": c.name}))
		if err != nil {
			t.Fatalf("discover %s: %v", c.name, err)
		}
		text := res.Content[0].(mcp.TextContent).Text
		if len(text) > 16000 {
			t.Errorf("category %s discovery payload %d bytes — split the category", c.name, len(text))
		}
	}
}

// Guard against schema drift: discover_tools output must round-trip as JSON.
func TestDiscoverSchemasValidJSON(t *testing.T) {
	h := testHandlers(t)
	res, _ := h.HandleDiscoverTools(context.Background(), call(map[string]any{"category": "tasks"}))
	var parsed struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	text := res.Content[0].(mcp.TextContent).Text
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("discover output not valid JSON: %v", err)
	}
	if len(parsed.Tools) != 16 {
		t.Errorf("tasks tools = %d, want 16", len(parsed.Tools))
	}
	for _, tool := range parsed.Tools {
		if tool.InputSchema["type"] != "object" {
			t.Errorf("tool %s inputSchema.type = %v", tool.Name, tool.InputSchema["type"])
		}
	}
}

func TestTableFormat(t *testing.T) {
	h := testHandlers(t)
	ctx := context.Background()

	_, _ = h.HandleRegisterAgent(ctx, call(map[string]any{"name": "scout", "project": "p1", "role": "explorer"}))
	_, _ = h.HandleSendMessage(ctx, call(map[string]any{
		"as": "scout", "project": "p1", "to": "scout",
		"subject": "hello", "content": "line1\nline2\twith tab",
	}))

	res, err := h.HandleGetInbox(ctx, call(map[string]any{"as": "scout", "project": "p1"}))
	if err != nil {
		t.Fatalf("inbox table: %v", err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	lines := strings.Split(text, "\n")
	if len(lines) < 4 {
		t.Fatalf("expected summary+header+divider+1 row, got %d lines:\n%s", len(lines), text)
	}
	if !strings.HasPrefix(lines[1], "|id|delivery_id|from|") {
		t.Errorf("bad header: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "|---|") {
		t.Errorf("missing markdown divider: %q", lines[2])
	}
	if strings.Contains(lines[3], "line2\twith") {
		t.Error("cell tab not sanitized")
	}

	res, _ = h.HandleListAgents(ctx, call(map[string]any{"project": "p1"}))
	text = res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "|scout|explorer|") {
		t.Errorf("agents table missing row: %s", text)
	}

	// json opt-in keeps the structured shape
	res, _ = h.HandleListAgents(ctx, call(map[string]any{"project": "p1", "format": "json"}))
	data := parseJSON(t, res)
	if int(data["count"].(float64)) != 1 {
		t.Errorf("json default broken: %v", data)
	}
}

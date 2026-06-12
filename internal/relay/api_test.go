package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-relay/internal/config"
	"agent-relay/internal/db"

	"github.com/mark3labs/mcp-go/server"
)

// testRelay creates a fully wired Relay with a test DB for API testing.
func testRelay(t *testing.T) *Relay {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.NewTestDB(dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mcpSrv := server.NewMCPServer("test", "0.0.0")
	events := NewEventBus()
	registry := NewSessionRegistry(mcpSrv)
	handlers := NewHandlers(database, registry, nil, events)

	return &Relay{
		MCPServer: mcpSrv,
		DB:        database,
		Registry:  registry,
		Events:    events,
		Handlers:  handlers,
		Config:    config.Config{},
	}
}

func doAPI(r *Relay, method, path string, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, "/api"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, "/api"+path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeAPI(w, req)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.NewDecoder(w.Body).Decode(&data); err != nil {
		t.Fatalf("decode json: %v\nstatus: %d\nbody: %s", err, w.Code, w.Body.String())
	}
	return data
}

func decodeJSONArray(t *testing.T, w *httptest.ResponseRecorder) []any {
	t.Helper()
	var data []any
	if err := json.NewDecoder(w.Body).Decode(&data); err != nil {
		t.Fatalf("decode json array: %v\nstatus: %d\nbody: %s", err, w.Code, w.Body.String())
	}
	return data
}

// --- Project API Tests ---

func TestAPIGetProjects(t *testing.T) {
	r := testRelay(t)

	// Create a project by registering an agent
	_, _, _ = r.DB.RegisterAgent("test-proj", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "GET", "/projects", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	projects := decodeJSONArray(t, w)
	if len(projects) < 1 {
		t.Errorf("expected at least 1 project, got %d", len(projects))
	}
}

func TestAPIGetProject(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("my-proj", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "GET", "/projects/my-proj", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	proj := decodeJSON(t, w)
	if proj["name"] != "my-proj" {
		t.Errorf("expected my-proj, got %v", proj["name"])
	}
}

func TestAPIGetProjectNotFound(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "GET", "/projects/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAPIPatchProject(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("my-proj", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "PATCH", "/projects/my-proj", `{"planet_type":"lava/1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIPatchProjectMissingPlanetType(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("my-proj", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "PATCH", "/projects/my-proj", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- Settings API Tests ---

func TestAPISettings(t *testing.T) {
	r := testRelay(t)

	// Get default
	w := doAPI(r, "GET", "/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	settings := decodeJSON(t, w)
	if settings["sun_type"] != "1" {
		t.Errorf("expected default sun_type=1, got %v", settings["sun_type"])
	}

	// Set
	w2 := doAPI(r, "PUT", "/settings", `{"sun_type":"3"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}

	// Verify
	w3 := doAPI(r, "GET", "/settings", "")
	settings2 := decodeJSON(t, w3)
	if settings2["sun_type"] != "3" {
		t.Errorf("expected sun_type=3, got %v", settings2["sun_type"])
	}
}

// --- Agent API Tests ---

func TestAPIGetAgents(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p1", "bot-b", "qa", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "GET", "/agents?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	agents := decodeJSONArray(t, w)
	if len(agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(agents))
	}
}

func TestAPIGetAllAgents(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p2", "bot-b", "qa", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "GET", "/agents/all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	agents := decodeJSONArray(t, w)
	if len(agents) != 2 {
		t.Errorf("expected 2 agents across projects, got %d", len(agents))
	}
}

func TestAPIGetOrgTree(t *testing.T) {
	r := testRelay(t)
	mgr := "manager"
	_, _, _ = r.DB.RegisterAgent("p1", "manager", "lead", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p1", "dev-1", "dev", "", &mgr, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "GET", "/org?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	tree := decodeJSONArray(t, w)
	if len(tree) != 1 { // 1 root (manager)
		t.Errorf("expected 1 root node, got %d", len(tree))
	}
	root := tree[0].(map[string]any)
	reports := root["reports"].([]any)
	if len(reports) != 1 {
		t.Errorf("expected 1 report, got %d", len(reports))
	}
}

// --- Message API Tests ---

func TestAPIGetAllMessages(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.InsertMessage("p1", "bot-a", "bot-b", "notification", "test", "hello", "{}", "P2", 3600, nil, nil)

	w := doAPI(r, "GET", "/messages/all?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	msgs := decodeJSONArray(t, w)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message, got %d", len(msgs))
	}
}

func TestAPIGetAllMessagesAllProjects(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p2", "bot-b", "qa", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.InsertMessage("p1", "bot-a", "bot-b", "notification", "test", "hello", "{}", "P2", 3600, nil, nil)
	_, _ = r.DB.InsertMessage("p2", "bot-b", "bot-a", "notification", "test", "hey", "{}", "P2", 3600, nil, nil)

	w := doAPI(r, "GET", "/messages/all-projects", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	msgs := decodeJSONArray(t, w)
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages, got %d", len(msgs))
	}
}

func TestAPIPostUserResponse(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "POST", "/user-response", `{"project":"p1","to":"bot-a","content":"yes"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	if data["ok"] != true {
		t.Error("expected ok=true")
	}
	if data["message_id"] == nil || data["message_id"] == "" {
		t.Error("expected message_id")
	}
}

func TestAPIPostUserResponseMissingFields(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/user-response", `{"project":"p1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- Conversation API Tests ---

func TestAPIGetConversations(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p1", "bot-b", "qa", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.CreateConversation("p1", "test conv", "bot-a", []string{"bot-a", "bot-b"})

	w := doAPI(r, "GET", "/conversations?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	convs := decodeJSONArray(t, w)
	if len(convs) != 1 {
		t.Errorf("expected 1 conversation, got %d", len(convs))
	}
}

func TestAPIGetConversationMessages(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p1", "bot-b", "qa", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	conv, _ := r.DB.CreateConversation("p1", "test", "bot-a", []string{"bot-a", "bot-b"})
	_, _ = r.DB.InsertMessage("p1", "bot-a", "", "notification", "test", "hello", "{}", "P2", 3600, nil, &conv.ID)

	w := doAPI(r, "GET", "/conversations/"+conv.ID+"/messages", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	msgs := decodeJSONArray(t, w)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message, got %d", len(msgs))
	}
}

// --- Memory API Tests ---

func TestAPIMemoryCRUD(t *testing.T) {
	r := testRelay(t)

	// Create
	w := doAPI(r, "POST", "/memories", `{"project":"p1","agent_name":"bot-a","key":"test_key","value":"test_value"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	mem := decodeJSON(t, w)
	memID := mem["id"].(string)
	if mem["key"] != "test_key" {
		t.Errorf("expected test_key, got %v", mem["key"])
	}

	// List
	w2 := doAPI(r, "GET", "/memories?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	memories := decodeJSONArray(t, w2)
	if len(memories) != 1 {
		t.Errorf("expected 1 memory, got %d", len(memories))
	}

	// Delete
	w3 := doAPI(r, "DELETE", "/memories/"+memID, "")
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w3.Code, w3.Body.String())
	}

	// Verify deleted
	w4 := doAPI(r, "GET", "/memories?project=p1", "")
	memories2 := decodeJSONArray(t, w4)
	if len(memories2) != 0 {
		t.Errorf("expected 0 memories after delete, got %d", len(memories2))
	}
}

func TestAPIMemoryCreateMissingFields(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/memories", `{"project":"p1","key":"k"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPISearchMemories(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.SetMemory("p1", "bot-a", "deploy_config", "production URL is https://prod.example.com", "[]", "project", "stated", "behavior")

	w := doAPI(r, "GET", "/memories/search?q=deploy", "")
	// FTS5 may not be available in test builds
	if w.Code == http.StatusInternalServerError {
		t.Skip("FTS5 not available in this build")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	results := decodeJSONArray(t, w)
	if len(results) < 1 {
		t.Errorf("expected at least 1 search result, got %d", len(results))
	}
}

func TestAPISearchMemoriesMissingQuery(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "GET", "/memories/search", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- Task API Tests ---

func TestAPITaskCRUD(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	// Dispatch
	w := doAPI(r, "POST", "/tasks", `{"project":"p1","dispatched_by":"bot-a","profile":"dev","title":"Fix bug","description":"fix it"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	task := decodeJSON(t, w)
	taskID := task["id"].(string)
	if task["title"] != "Fix bug" {
		t.Errorf("expected 'Fix bug', got %v", task["title"])
	}

	// Get
	w2 := doAPI(r, "GET", "/tasks/"+taskID+"?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	got := decodeJSON(t, w2)
	if got["title"] != "Fix bug" {
		t.Errorf("expected 'Fix bug', got %v", got["title"])
	}

	// List
	w3 := doAPI(r, "GET", "/tasks?project=p1", "")
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w3.Code)
	}
	tasks := decodeJSONArray(t, w3)
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}
}

func TestAPITaskTransition(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	task, _ := r.DB.DispatchTask("p1", "dev", "bot-a", "task1", "", "P2", nil, nil)

	// Claim (status=accepted)
	w := doAPI(r, "POST", "/tasks/"+task.ID+"/transition", `{"project":"p1","agent":"bot-a","status":"accepted"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	claimed := decodeJSON(t, w)
	if claimed["status"] != "accepted" {
		t.Errorf("expected accepted, got %v", claimed["status"])
	}

	// Start (status=in-progress)
	w2 := doAPI(r, "POST", "/tasks/"+task.ID+"/transition", `{"project":"p1","agent":"bot-a","status":"in-progress"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	started := decodeJSON(t, w2)
	if started["status"] != "in-progress" {
		t.Errorf("expected in-progress, got %v", started["status"])
	}

	// Complete (status=done)
	w3 := doAPI(r, "POST", "/tasks/"+task.ID+"/transition", `{"project":"p1","agent":"bot-a","status":"done","result":"done!"}`)
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w3.Code)
	}
	completed := decodeJSON(t, w3)
	if completed["status"] != "done" {
		t.Errorf("expected done, got %v", completed["status"])
	}
}

func TestAPIGetAllTasks(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.DispatchTask("p1", "dev", "bot-a", "task1", "", "P2", nil, nil)
	_, _ = r.DB.DispatchTask("p1", "dev", "bot-a", "task2", "", "P1", nil, nil)

	w := doAPI(r, "GET", "/tasks/all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	tasks := decodeJSONArray(t, w)
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

// --- Kanban board + cycle API Tests ---

func strptr(s string) *string { return &s }
func intptr(i int) *int       { return &i }

// seedCycleTask seeds a Linear mirror task into a cycle for board/cycle tests.
func seedCycleTask(t *testing.T, r *Relay, id, project, title, state, cycleID, cycleName, start, end string, points int) {
	t.Helper()
	err := r.DB.UpsertLinearTask(db.LinearTaskSeed{
		ID:          id,
		Project:     project,
		Title:       title,
		Priority:    "P1",
		Status:      "in-progress",
		LinearKey:   strptr("SYN-" + id),
		LinearState: strptr(state),
		Points:      intptr(points),
		CycleID:     strptr(cycleID),
		CycleName:   strptr(cycleName),
		CycleStart:  strptr(start),
		CycleEnd:    strptr(end),
	})
	if err != nil {
		t.Fatalf("seed cycle task: %v", err)
	}
}

func TestAPIGetBoardTasks(t *testing.T) {
	r := testRelay(t)
	// One native task + two Linear tasks across two cycles.
	_, _ = r.DB.DispatchTask("p1", "dev", "user", "native task", "", "P2", nil, nil)
	seedCycleTask(t, r, "1", "p1", "cycle-A task", "In Progress", "cyc-a", "Cycle A", "2026-06-01", "2026-06-14", 3)
	seedCycleTask(t, r, "2", "p1", "cycle-B task", "Todo", "cyc-b", "Cycle B", "2026-05-01", "2026-05-14", 5)

	// All tasks (no cycle filter)
	w := doAPI(r, "GET", "/tasks/board?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	all := decodeJSONArray(t, w)
	if len(all) != 3 {
		t.Fatalf("expected 3 board tasks, got %d", len(all))
	}

	// Filter to cycle A — only the cycle-A task
	w2 := doAPI(r, "GET", "/tasks/board?project=p1&cycle=cyc-a", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	cycA := decodeJSONArray(t, w2)
	if len(cycA) != 1 {
		t.Fatalf("expected 1 task in cycle A, got %d", len(cycA))
	}
	if got := cycA[0].(map[string]any)["title"]; got != "cycle-A task" {
		t.Errorf("expected 'cycle-A task', got %v", got)
	}
}

func TestAPIGetBoardTasksExcludesCancelledAndArchived(t *testing.T) {
	r := testRelay(t)
	tk, _ := r.DB.DispatchTask("p1", "dev", "user", "keep me", "", "P2", nil, nil)
	cn, _ := r.DB.DispatchTask("p1", "dev", "user", "cancel me", "", "P2", nil, nil)
	_, _ = r.DB.CancelTask(cn.ID, "user", "p1", nil)
	_ = tk // keep alive

	w := doAPI(r, "GET", "/tasks/board?project=p1", "")
	tasks := decodeJSONArray(t, w)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 board task (cancelled excluded), got %d", len(tasks))
	}
}

func TestAPIGetBoardTasksActiveCycle(t *testing.T) {
	r := testRelay(t)
	today := time.Now().UTC()
	curStart := today.AddDate(0, 0, -3).Format("2006-01-02")
	curEnd := today.AddDate(0, 0, 3).Format("2006-01-02")
	pastStart := today.AddDate(0, 0, -30).Format("2006-01-02")
	pastEnd := today.AddDate(0, 0, -20).Format("2006-01-02")
	seedCycleTask(t, r, "1", "p1", "current task", "Todo", "cyc-cur", "Current", curStart, curEnd, 1)
	seedCycleTask(t, r, "2", "p1", "past task", "Done", "cyc-past", "Past", pastStart, pastEnd, 1)

	// cycle=active should resolve to the cycle spanning today
	w := doAPI(r, "GET", "/tasks/board?project=p1&cycle=active", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	tasks := decodeJSONArray(t, w)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task in active cycle, got %d", len(tasks))
	}
	if got := tasks[0].(map[string]any)["title"]; got != "current task" {
		t.Errorf("expected 'current task', got %v", got)
	}
}

func TestAPIGetCycles(t *testing.T) {
	r := testRelay(t)
	today := time.Now().UTC()
	curStart := today.AddDate(0, 0, -2).Format("2006-01-02")
	curEnd := today.AddDate(0, 0, 5).Format("2006-01-02")
	seedCycleTask(t, r, "1", "p1", "t1", "Todo", "cyc-cur", "Current", curStart, curEnd, 1)
	seedCycleTask(t, r, "2", "p1", "t2", "Todo", "cyc-cur", "Current", curStart, curEnd, 1)
	seedCycleTask(t, r, "3", "p1", "t3", "Done", "cyc-old", "Old", "2025-01-01", "2025-01-14", 1)

	w := doAPI(r, "GET", "/cycles?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cycles := decodeJSONArray(t, w)
	if len(cycles) != 2 {
		t.Fatalf("expected 2 cycles, got %d", len(cycles))
	}
	// Find current cycle and verify active + count
	var foundActive bool
	for _, c := range cycles {
		cm := c.(map[string]any)
		if cm["id"] == "cyc-cur" {
			if cm["active"] != true {
				t.Errorf("expected cyc-cur active=true, got %v", cm["active"])
			}
			if cm["count"].(float64) != 2 {
				t.Errorf("expected cyc-cur count=2, got %v", cm["count"])
			}
			foundActive = true
		}
		if cm["id"] == "cyc-old" && cm["active"] != false {
			t.Errorf("expected cyc-old active=false, got %v", cm["active"])
		}
	}
	if !foundActive {
		t.Error("active cycle cyc-cur not found in response")
	}
}

func TestAPIGetCyclesEmptyNative(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.DispatchTask("p1", "dev", "user", "native only", "", "P2", nil, nil)

	w := doAPI(r, "GET", "/cycles?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	cycles := decodeJSONArray(t, w)
	if len(cycles) != 0 {
		t.Errorf("expected 0 cycles in native mode, got %d", len(cycles))
	}
}

// --- Profile API Tests ---

func TestAPIGetProfiles(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.RegisterProfile("p1", "backend", "Backend Dev", "developer", "[]")

	w := doAPI(r, "GET", "/profiles?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	profiles := decodeJSONArray(t, w)
	if len(profiles) != 1 {
		t.Errorf("expected 1 profile, got %d", len(profiles))
	}
}

func TestAPIGetProfile(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.RegisterProfile("p1", "backend", "Backend Dev", "developer", "[]")

	w := doAPI(r, "GET", "/profiles/backend?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	profile := decodeJSON(t, w)
	if profile["slug"] != "backend" {
		t.Errorf("expected backend, got %v", profile["slug"])
	}
}

// --- Board API Tests ---

func TestAPIGetBoards(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.CreateBoard("p1", "Sprint 1", "sprint-1", "", "user")

	w := doAPI(r, "GET", "/boards?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	boards := decodeJSONArray(t, w)
	if len(boards) != 1 {
		t.Errorf("expected 1 board, got %d", len(boards))
	}
}

// --- Team API Tests ---

func TestAPIGetTeams(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.CreateTeam("Backend", "backend", "p1", "", "regular", nil, nil)

	w := doAPI(r, "GET", "/teams?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	teams := decodeJSONArray(t, w)
	if len(teams) != 1 {
		t.Errorf("expected 1 team, got %d", len(teams))
	}
}

// --- More Team/Org API Tests ---

func TestAPIGetOrgs(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.CreateOrg("Acme", "acme", "")

	w := doAPI(r, "GET", "/orgs", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	orgs := decodeJSONArray(t, w)
	if len(orgs) != 1 {
		t.Errorf("expected 1 org, got %d", len(orgs))
	}
}

func TestAPIGetAllTeams(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.CreateTeam("Backend", "backend", "p1", "", "regular", nil, nil)
	_, _ = r.DB.CreateTeam("Frontend", "frontend", "p2", "", "regular", nil, nil)

	w := doAPI(r, "GET", "/teams/all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	teams := decodeJSONArray(t, w)
	if len(teams) != 2 {
		t.Errorf("expected 2 teams across projects, got %d", len(teams))
	}
}

func TestAPIGetTeamMembers(t *testing.T) {
	r := testRelay(t)
	team, _ := r.DB.CreateTeam("Backend", "backend", "p1", "", "regular", nil, nil)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_ = r.DB.AddTeamMember(team.ID, "bot-a", "p1", "lead")

	w := doAPI(r, "GET", "/teams/backend/members?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// --- More Conversation API Tests ---

func TestAPIGetAllConversations(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p2", "bot-b", "qa", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.CreateConversation("p1", "conv1", "bot-a", []string{"bot-a"})
	_, _ = r.DB.CreateConversation("p2", "conv2", "bot-b", []string{"bot-b"})

	w := doAPI(r, "GET", "/conversations/all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	convs := decodeJSONArray(t, w)
	if len(convs) != 2 {
		t.Errorf("expected 2 conversations across projects, got %d", len(convs))
	}
}

// --- More Message API Tests ---

func TestAPIGetLatestMessages(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.InsertMessage("p1", "bot-a", "bot-b", "notification", "test", "recent msg", "{}", "P2", 3600, nil, nil)

	w := doAPI(r, "GET", "/messages/latest?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	msgs := decodeJSONArray(t, w)
	if len(msgs) != 1 {
		t.Errorf("expected 1 recent message, got %d", len(msgs))
	}
}

func TestAPIGetLatestMessagesAllProjects(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.InsertMessage("p1", "bot-a", "bot-b", "notification", "test", "msg1", "{}", "P2", 3600, nil, nil)

	w := doAPI(r, "GET", "/messages/latest-all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- More Task API Tests ---

func TestAPIGetLatestTasks(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _ = r.DB.DispatchTask("p1", "dev", "bot-a", "recent task", "", "P2", nil, nil)

	w := doAPI(r, "GET", "/tasks/latest?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAPIUpdateTask(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	task, _ := r.DB.DispatchTask("p1", "dev", "bot-a", "old title", "", "P2", nil, nil)

	w := doAPI(r, "PUT", "/tasks/"+task.ID, `{"project":"p1","title":"new title"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	updated := decodeJSON(t, w)
	if updated["title"] != "new title" {
		t.Errorf("expected 'new title', got %v", updated["title"])
	}
}

func TestAPIDeleteTask(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	task, _ := r.DB.DispatchTask("p1", "dev", "bot-a", "to delete", "", "P2", nil, nil)

	w := doAPI(r, "DELETE", "/tasks/"+task.ID+"?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	if data["deleted"] != true {
		t.Error("expected deleted=true")
	}
}

// --- More Board API Tests ---

func TestAPIGetAllBoards(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.CreateBoard("p1", "Sprint 1", "sprint-1", "", "user")
	_, _ = r.DB.CreateBoard("p2", "Sprint 2", "sprint-2", "", "user")

	w := doAPI(r, "GET", "/boards/all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	boards := decodeJSONArray(t, w)
	if len(boards) != 2 {
		t.Errorf("expected 2 boards, got %d", len(boards))
	}
}

// --- Memory API resolve conflict ---

func TestAPIResolveMemoryConflict(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.SetMemory("p1", "bot-a", "key1", "value-a", "[]", "project", "stated", "behavior")
	_, _ = r.DB.SetMemory("p1", "bot-b", "key1", "value-b", "[]", "project", "stated", "behavior")

	w := doAPI(r, "POST", "/memories/key1/resolve", `{"project":"p1","chosen_value":"value-b"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	if data["resolved"] != true {
		t.Error("expected resolved=true")
	}
}

// --- 404 Test ---

func TestAPINotFound(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "GET", "/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// --- Activity API Tests ---

func TestAPIGetActivity(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "GET", "/activity", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	sessions := decodeJSONArray(t, w)
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions with nil ingester, got %d", len(sessions))
	}
}

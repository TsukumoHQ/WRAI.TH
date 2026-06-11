package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	handlers := NewHandlers(database, registry, nil, nil, events)

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

	task, _ := r.DB.DispatchTask("p1", "dev", "bot-a", "task1", "", "P2", nil, nil, nil)

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
	_, _ = r.DB.DispatchTask("p1", "dev", "bot-a", "task1", "", "P2", nil, nil, nil)
	_, _ = r.DB.DispatchTask("p1", "dev", "bot-a", "task2", "", "P1", nil, nil, nil)

	w := doAPI(r, "GET", "/tasks/all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	tasks := decodeJSONArray(t, w)
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

// --- Profile API Tests ---

func TestAPIGetProfiles(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.RegisterProfile("p1", "backend", "Backend Dev", "developer", "", "[]", "[]", "[]")

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
	_, _ = r.DB.RegisterProfile("p1", "backend", "Backend Dev", "developer", "", "[]", "[]", "[]")

	w := doAPI(r, "GET", "/profiles/backend?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	profile := decodeJSON(t, w)
	if profile["slug"] != "backend" {
		t.Errorf("expected backend, got %v", profile["slug"])
	}
}

// --- Goal API Tests ---

func TestAPIGoalCRUD(t *testing.T) {
	r := testRelay(t)

	// Create
	w := doAPI(r, "POST", "/goals", `{"project":"p1","title":"Ship v2","type":"agent_goal"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	goal := decodeJSON(t, w)
	goalID := goal["id"].(string)

	// Get
	w2 := doAPI(r, "GET", "/goals/"+goalID+"?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}

	// Update
	w3 := doAPI(r, "PUT", "/goals/"+goalID, `{"project":"p1","status":"completed"}`)
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w3.Code, w3.Body.String())
	}

	// List
	w4 := doAPI(r, "GET", "/goals?project=p1", "")
	if w4.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w4.Code)
	}
	goals := decodeJSONArray(t, w4)
	if len(goals) != 1 {
		t.Errorf("expected 1 goal, got %d", len(goals))
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
	_, _ = r.DB.DispatchTask("p1", "dev", "bot-a", "recent task", "", "P2", nil, nil, nil)

	w := doAPI(r, "GET", "/tasks/latest?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAPIUpdateTask(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	task, _ := r.DB.DispatchTask("p1", "dev", "bot-a", "old title", "", "P2", nil, nil, nil)

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
	task, _ := r.DB.DispatchTask("p1", "dev", "bot-a", "to delete", "", "P2", nil, nil, nil)

	w := doAPI(r, "DELETE", "/tasks/"+task.ID+"?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	if data["deleted"] != true {
		t.Error("expected deleted=true")
	}
}

// --- More Goal API Tests ---

func TestAPIGetAllGoals(t *testing.T) {
	r := testRelay(t)
	_, _ = r.DB.CreateGoal("p1", "agent_goal", "Goal 1", "", "user", nil, nil)
	_, _ = r.DB.CreateGoal("p2", "agent_goal", "Goal 2", "", "user", nil, nil)

	w := doAPI(r, "GET", "/goals/all", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	goals := decodeJSONArray(t, w)
	if len(goals) != 2 {
		t.Errorf("expected 2 goals, got %d", len(goals))
	}
}

func TestAPIGetGoalCascade(t *testing.T) {
	r := testRelay(t)
	parent, _ := r.DB.CreateGoal("p1", "mission", "Mission", "", "user", nil, nil)
	_, _ = r.DB.CreateGoal("p1", "project_goal", "Sub-goal", "", "user", nil, &parent.ID)

	w := doAPI(r, "GET", "/goals/cascade?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
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

// ===== v0.6 OS Primitives =====

// --- Phase 1: Triggers + Webhooks + Signal Handlers ---

func TestAPITriggerCRUD(t *testing.T) {
	r := testRelay(t)

	// Create trigger
	w := doAPI(r, "POST", "/triggers", `{"project":"p1","event":"task_pending","profile_slug":"backend","cycle":"review","match_rules":"{\"priority\":\"P0\"}"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create trigger: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	trigger := decodeJSON(t, w)
	triggerID := trigger["id"].(string)
	if trigger["event"] != "task_pending" {
		t.Errorf("expected event=task_pending, got %v", trigger["event"])
	}

	// List triggers
	w2 := doAPI(r, "GET", "/triggers?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("list triggers: expected 200, got %d", w2.Code)
	}
	triggers := decodeJSONArray(t, w2)
	if len(triggers) != 1 {
		t.Errorf("expected 1 trigger, got %d", len(triggers))
	}

	// List with event filter
	w3 := doAPI(r, "GET", "/triggers?project=p1&event=task_pending", "")
	if w3.Code != http.StatusOK {
		t.Fatalf("list filtered triggers: expected 200, got %d", w3.Code)
	}
	filtered := decodeJSONArray(t, w3)
	if len(filtered) != 1 {
		t.Errorf("expected 1 filtered trigger, got %d", len(filtered))
	}

	// List with wrong event filter
	w4 := doAPI(r, "GET", "/triggers?project=p1&event=nonexistent", "")
	if w4.Code != http.StatusOK {
		t.Fatalf("list filtered triggers: expected 200, got %d", w4.Code)
	}
	empty := decodeJSONArray(t, w4)
	if len(empty) != 0 {
		t.Errorf("expected 0 triggers for nonexistent event, got %d", len(empty))
	}

	// Delete trigger
	w5 := doAPI(r, "DELETE", "/triggers/"+triggerID, "")
	if w5.Code != http.StatusOK {
		t.Fatalf("delete trigger: expected 200, got %d", w5.Code)
	}

	// Verify deleted
	w6 := doAPI(r, "GET", "/triggers?project=p1", "")
	after := decodeJSONArray(t, w6)
	if len(after) != 0 {
		t.Errorf("expected 0 triggers after delete, got %d", len(after))
	}
}

func TestAPITriggerCreateMissingFields(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/triggers", `{"project":"p1","event":"task_pending"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", w.Code)
	}
}

func TestAPITriggerHistory(t *testing.T) {
	r := testRelay(t)

	// Record some trigger fires
	r.DB.RecordTriggerFire("trigger-1", "p1", "task_pending", "child-1", nil)
	r.DB.RecordTriggerFire("trigger-1", "p1", "task_completed", "child-2", nil)

	w := doAPI(r, "GET", "/trigger-history?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("trigger history: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	history := decodeJSONArray(t, w)
	if len(history) != 2 {
		t.Errorf("expected 2 history entries, got %d", len(history))
	}
}

func TestAPITriggerHistoryWithLimit(t *testing.T) {
	r := testRelay(t)

	for i := 0; i < 5; i++ {
		r.DB.RecordTriggerFire("trigger-1", "p1", "test_event", "", nil)
	}

	w := doAPI(r, "GET", "/trigger-history?project=p1&limit=2", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	history := decodeJSONArray(t, w)
	if len(history) != 2 {
		t.Errorf("expected 2 (limited), got %d", len(history))
	}
}

func TestAPIWebhook(t *testing.T) {
	r := testRelay(t)

	// Create a trigger that matches "pr_opened" events
	_, _ = r.DB.UpsertTrigger("myproj", "pr_opened", `{}`, "reviewer", "review", "10m")

	// Fire webhook — no spawn manager so it should report "spawn_unavailable"
	w := doAPI(r, "POST", "/webhooks/myproj/pr_opened", `{"author":"alice","branch":"feature-x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	skipped := data["skipped"].([]any)
	if len(skipped) == 0 {
		t.Error("expected at least 1 skipped (no spawn manager)")
	}
	// Verify trigger history was recorded
	history := r.DB.GetTriggerHistory("myproj", 10)
	if len(history) != 1 {
		t.Errorf("expected 1 history entry from webhook, got %d", len(history))
	}
}

func TestAPIWebhookWithMatchRules(t *testing.T) {
	r := testRelay(t)

	// Trigger only fires when author=alice
	_, _ = r.DB.UpsertTrigger("proj", "pr_opened", `{"author":"alice"}`, "reviewer", "review", "10m")

	// Should match
	w := doAPI(r, "POST", "/webhooks/proj/pr_opened", `{"author":"alice"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook: expected 200, got %d", w.Code)
	}
	data := decodeJSON(t, w)
	// Will be skipped (spawn_unavailable) but NOT rules_mismatch
	skipped := data["skipped"].([]any)
	for _, s := range skipped {
		sk := s.(map[string]any)
		if sk["reason"] == "rules_mismatch" {
			t.Error("alice should match, got rules_mismatch")
		}
	}

	// Should NOT match (bob != alice)
	w2 := doAPI(r, "POST", "/webhooks/proj/pr_opened", `{"author":"bob"}`)
	data2 := decodeJSON(t, w2)
	skipped2 := data2["skipped"].([]any)
	found := false
	for _, s := range skipped2 {
		sk := s.(map[string]any)
		if sk["reason"] == "rules_mismatch" {
			found = true
		}
	}
	if !found {
		t.Error("bob should NOT match, expected rules_mismatch")
	}
}

func TestAPIWebhookBadPath(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/webhooks/", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad path, got %d", w.Code)
	}
}

func TestAPISignalHandler(t *testing.T) {
	r := testRelay(t)

	w := doAPI(r, "POST", "/signal-handlers", `{"project":"p1","signal":"interrupt","profile_slug":"oncall","cycle":"triage"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create signal handler: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	handler := decodeJSON(t, w)
	if handler["event"] != "signal:interrupt" {
		t.Errorf("expected event=signal:interrupt, got %v", handler["event"])
	}

	// Verify it appears in triggers list
	triggers := r.DB.ListTriggers("p1", "signal:interrupt")
	if len(triggers) != 1 {
		t.Errorf("expected 1 signal trigger, got %d", len(triggers))
	}
}

func TestAPISignalHandlerMissingFields(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/signal-handlers", `{"project":"p1","signal":"interrupt"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", w.Code)
	}
}

// --- Phase 2: Poll Triggers ---

func TestAPIPollTriggerCRUD(t *testing.T) {
	r := testRelay(t)

	// Create
	w := doAPI(r, "POST", "/poll-triggers", `{
		"project":"p1",
		"name":"ci-status",
		"url":"https://api.example.com/status",
		"condition_path":"status",
		"condition_op":"eq",
		"condition_value":"green",
		"poll_interval":"5m",
		"fire_event":"ci_green",
		"cooldown_seconds":300
	}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create poll trigger: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	pt := decodeJSON(t, w)
	ptID := pt["id"].(string)
	if pt["name"] != "ci-status" {
		t.Errorf("expected name=ci-status, got %v", pt["name"])
	}
	if pt["condition_op"] != "eq" {
		t.Errorf("expected condition_op=eq, got %v", pt["condition_op"])
	}

	// List
	w2 := doAPI(r, "GET", "/poll-triggers?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("list poll triggers: expected 200, got %d", w2.Code)
	}
	pts := decodeJSONArray(t, w2)
	if len(pts) != 1 {
		t.Errorf("expected 1 poll trigger, got %d", len(pts))
	}

	// Delete
	w3 := doAPI(r, "DELETE", "/poll-triggers/"+ptID, "")
	if w3.Code != http.StatusOK {
		t.Fatalf("delete poll trigger: expected 200, got %d: %s", w3.Code, w3.Body.String())
	}

	// Verify deleted
	w4 := doAPI(r, "GET", "/poll-triggers?project=p1", "")
	after := decodeJSONArray(t, w4)
	if len(after) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(after))
	}
}

func TestAPIPollTriggerUpsert(t *testing.T) {
	r := testRelay(t)

	// Create
	doAPI(r, "POST", "/poll-triggers", `{
		"project":"p1","name":"ci","url":"https://a.com","condition_path":"s","condition_op":"eq","condition_value":"ok","poll_interval":"1m","fire_event":"ev"
	}`)

	// Upsert same name — should update, not duplicate
	w := doAPI(r, "POST", "/poll-triggers", `{
		"project":"p1","name":"ci","url":"https://b.com","condition_path":"s","condition_op":"neq","condition_value":"fail","poll_interval":"2m","fire_event":"ev2"
	}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("upsert: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	pt := decodeJSON(t, w)
	if pt["url"] != "https://b.com" {
		t.Errorf("expected updated url, got %v", pt["url"])
	}

	// Should still be only 1
	w2 := doAPI(r, "GET", "/poll-triggers?project=p1", "")
	pts := decodeJSONArray(t, w2)
	if len(pts) != 1 {
		t.Errorf("expected 1 after upsert, got %d", len(pts))
	}
}

func TestAPIPollTriggerCreateMissingFields(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/poll-triggers", `{"project":"p1","name":"ci"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPIPollTriggerTest(t *testing.T) {
	r := testRelay(t)

	// Create a poll trigger pointing to a non-existent URL
	w := doAPI(r, "POST", "/poll-triggers", `{
		"project":"p1","name":"test-poll",
		"url":"http://127.0.0.1:1/nonexistent",
		"condition_path":"status","condition_op":"eq","condition_value":"ok",
		"poll_interval":"1m","fire_event":"test"
	}`)
	pt := decodeJSON(t, w)
	ptID := pt["id"].(string)

	// Test it — should fail (connection refused)
	w2 := doAPI(r, "POST", "/poll-triggers/"+ptID+"/test", "")
	if w2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unreachable URL, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestAPIPollTriggerTestNotFound(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/poll-triggers/nonexistent-id/test", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for nonexistent trigger, got %d", w.Code)
	}
}

// --- Phase 3: Skills ---

func TestAPISkillCRUD(t *testing.T) {
	r := testRelay(t)

	// Create
	w := doAPI(r, "POST", "/skills", `{"project":"p1","name":"database","description":"Database management","tags":"[\"sql\",\"migrations\"]"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create skill: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	skill := decodeJSON(t, w)
	if skill["name"] != "database" {
		t.Errorf("expected name=database, got %v", skill["name"])
	}

	// List
	w2 := doAPI(r, "GET", "/skills?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("list skills: expected 200, got %d", w2.Code)
	}
	skills := decodeJSONArray(t, w2)
	if len(skills) != 1 {
		t.Errorf("expected 1 skill, got %d", len(skills))
	}
}

func TestAPISkillUpsert(t *testing.T) {
	r := testRelay(t)

	doAPI(r, "POST", "/skills", `{"project":"p1","name":"database","description":"v1"}`)
	w := doAPI(r, "POST", "/skills", `{"project":"p1","name":"database","description":"v2"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("upsert skill: expected 201, got %d", w.Code)
	}
	skill := decodeJSON(t, w)
	if skill["description"] != "v2" {
		t.Errorf("expected updated description, got %v", skill["description"])
	}

	// Should still be 1
	w2 := doAPI(r, "GET", "/skills?project=p1", "")
	skills := decodeJSONArray(t, w2)
	if len(skills) != 1 {
		t.Errorf("expected 1 after upsert, got %d", len(skills))
	}
}

func TestAPISkillCreateMissingName(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/skills", `{"project":"p1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPISkillProfiles(t *testing.T) {
	r := testRelay(t)

	// Create skill and profile, link them
	skill, _ := r.DB.UpsertSkill("p1", "database", "DB mgmt", "[]")
	profile, _ := r.DB.RegisterProfile("p1", "dba", "DBA", "database admin", "", "[]", "[]", "[]")
	_ = r.DB.LinkProfileSkill(profile.ID, skill.ID, "expert")

	w := doAPI(r, "GET", "/skills/database/profiles?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("skill profiles: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	profiles := data["profiles"].([]any)
	if len(profiles) != 1 {
		t.Errorf("expected 1 linked profile, got %d", len(profiles))
	}
	p := profiles[0].(map[string]any)
	if p["slug"] != "dba" {
		t.Errorf("expected slug=dba, got %v", p["slug"])
	}
	if p["proficiency"] != "expert" {
		t.Errorf("expected proficiency=expert, got %v", p["proficiency"])
	}
}

func TestAPISkillProfilesEmpty(t *testing.T) {
	r := testRelay(t)

	w := doAPI(r, "GET", "/skills/nonexistent/profiles?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	data := decodeJSON(t, w)
	profiles := data["profiles"].([]any)
	if len(profiles) != 0 {
		t.Errorf("expected 0 profiles, got %d", len(profiles))
	}
}

// --- Phase 4: Quotas ---

func TestAPIQuotaCRUD(t *testing.T) {
	r := testRelay(t)

	// Set quota
	w := doAPI(r, "PUT", "/quotas/bot-a", `{"project":"p1","max_messages_per_hour":100,"max_tasks_per_hour":50,"max_spawns_per_hour":10}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set quota: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	if data["status"] != "updated" {
		t.Errorf("expected status=updated, got %v", data["status"])
	}

	// Get agent quota
	w2 := doAPI(r, "GET", "/quotas/bot-a?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("get quota: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	quota := decodeJSON(t, w2)
	if quota["max_messages_per_hour"].(float64) != 100 {
		t.Errorf("expected max_messages_per_hour=100, got %v", quota["max_messages_per_hour"])
	}
	if quota["max_tasks_per_hour"].(float64) != 50 {
		t.Errorf("expected max_tasks_per_hour=50, got %v", quota["max_tasks_per_hour"])
	}

	// List quotas
	w3 := doAPI(r, "GET", "/quotas?project=p1", "")
	if w3.Code != http.StatusOK {
		t.Fatalf("list quotas: expected 200, got %d", w3.Code)
	}
	quotas := decodeJSONArray(t, w3)
	if len(quotas) != 1 {
		t.Errorf("expected 1 quota, got %d", len(quotas))
	}
}

func TestAPIQuotaUsageTracking(t *testing.T) {
	r := testRelay(t)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	// Set quota
	_ = r.DB.SetAgentQuota("p1", "bot-a", 0, 100, 50, 0)

	// Get usage — should be 0
	w := doAPI(r, "GET", "/quotas/bot-a?project=p1", "")
	data := decodeJSON(t, w)
	if data["messages_used_1h"].(float64) != 0 {
		t.Errorf("expected 0 messages used, got %v", data["messages_used_1h"])
	}
}

func TestAPIQuotaUpdate(t *testing.T) {
	r := testRelay(t)

	// Set initial quota
	doAPI(r, "PUT", "/quotas/bot-a", `{"project":"p1","max_messages_per_hour":100}`)

	// Update quota
	w := doAPI(r, "PUT", "/quotas/bot-a", `{"project":"p1","max_messages_per_hour":200,"max_tasks_per_hour":75}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update quota: expected 200, got %d", w.Code)
	}

	// Verify
	w2 := doAPI(r, "GET", "/quotas/bot-a?project=p1", "")
	quota := decodeJSON(t, w2)
	if quota["max_messages_per_hour"].(float64) != 200 {
		t.Errorf("expected 200, got %v", quota["max_messages_per_hour"])
	}
}

func TestAPIQuotaNoQuotaSet(t *testing.T) {
	r := testRelay(t)

	// Get quota for agent with no quota — should return zero values
	w := doAPI(r, "GET", "/quotas/unknown-agent?project=p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	data := decodeJSON(t, w)
	if data["max_messages_per_hour"].(float64) != 0 {
		t.Errorf("expected 0, got %v", data["max_messages_per_hour"])
	}
}

// --- Phase 5: Discover ---

func TestAPIDiscover(t *testing.T) {
	r := testRelay(t)

	// Setup: skill + profile + link + active agent
	skill, _ := r.DB.UpsertSkill("p1", "testing", "QA testing", "[]")
	profile, _ := r.DB.RegisterProfile("p1", "qa-lead", "QA Lead", "tester", "", "[]", "[]", "[]")
	_ = r.DB.LinkProfileSkill(profile.ID, skill.ID, "expert")
	slug := "qa-lead"
	_, _, _ = r.DB.RegisterAgent("p1", "qa-bot", "tester", "", nil, &slug, false, nil, "[]", 0, db.RegisterOptions{})

	w := doAPI(r, "GET", "/discover?project=p1&skill=testing", "")
	if w.Code != http.StatusOK {
		t.Fatalf("discover: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeJSON(t, w)
	agents := data["agents"].([]any)
	profiles := data["profiles"].([]any)
	if len(agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(agents))
	}
	if len(profiles) != 1 {
		t.Errorf("expected 1 profile, got %d", len(profiles))
	}
}

func TestAPIDiscoverNoSkill(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "GET", "/discover?project=p1", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without skill param, got %d", w.Code)
	}
}

func TestAPIDiscoverEmpty(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "GET", "/discover?project=p1&skill=nonexistent", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	data := decodeJSON(t, w)
	agents := data["agents"].([]any)
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}

// --- Phase 6: Elevations ---

func TestAPIElevationCRUD(t *testing.T) {
	r := testRelay(t)

	// Grant elevation
	w := doAPI(r, "POST", "/elevations", `{"project":"p1","agent":"bot-a","role":"admin","granted_by":"lead","reason":"emergency","duration":"1h"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("grant elevation: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	elev := decodeJSON(t, w)
	elevID := elev["id"].(string)
	if elev["elevated_role"] != "admin" {
		t.Errorf("expected role=admin, got %v", elev["elevated_role"])
	}
	if elev["agent_name"] != "bot-a" {
		t.Errorf("expected agent=bot-a, got %v", elev["agent_name"])
	}

	// List elevations
	w2 := doAPI(r, "GET", "/elevations?project=p1", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("list elevations: expected 200, got %d", w2.Code)
	}
	elevs := decodeJSONArray(t, w2)
	if len(elevs) != 1 {
		t.Errorf("expected 1 elevation, got %d", len(elevs))
	}

	// Revoke
	w3 := doAPI(r, "DELETE", "/elevations/"+elevID, "")
	if w3.Code != http.StatusOK {
		t.Fatalf("revoke elevation: expected 200, got %d: %s", w3.Code, w3.Body.String())
	}
	revoked := decodeJSON(t, w3)
	if revoked["status"] != "revoked" {
		t.Errorf("expected status=revoked, got %v", revoked["status"])
	}

	// Verify revoked — list should be empty
	w4 := doAPI(r, "GET", "/elevations?project=p1", "")
	after := decodeJSONArray(t, w4)
	if len(after) != 0 {
		t.Errorf("expected 0 after revoke, got %d", len(after))
	}
}

func TestAPIElevationMissingFields(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/elevations", `{"project":"p1","agent":"bot-a"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", w.Code)
	}
}

func TestAPIElevationBadDuration(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/elevations", `{"project":"p1","agent":"bot-a","role":"admin","granted_by":"lead","duration":"not-a-duration"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad duration, got %d", w.Code)
	}
}

func TestAPIElevationDefaultDuration(t *testing.T) {
	r := testRelay(t)
	w := doAPI(r, "POST", "/elevations", `{"project":"p1","agent":"bot-a","role":"admin","granted_by":"lead"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	elev := decodeJSON(t, w)
	if elev["expires_at"] == "" {
		t.Error("expected expires_at to be set with default 1h duration")
	}
}

func TestAPIElevationCanMessage(t *testing.T) {
	r := testRelay(t)

	// Setup: teams configured, bot-a NOT in admin team
	_, _ = r.DB.CreateTeam("devs", "devs", "p1", "", "regular", nil, nil)
	_, _, _ = r.DB.RegisterAgent("p1", "bot-a", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})
	_, _, _ = r.DB.RegisterAgent("p1", "bot-b", "dev", "", nil, nil, false, nil, "[]", 0, db.RegisterOptions{})

	// Without elevation: should NOT be able to message bot-b (different team)
	allowed, _ := r.DB.CanMessage("p1", "bot-a", "bot-b")
	if allowed {
		t.Error("expected NOT allowed without elevation")
	}

	// Grant admin elevation
	_, _ = r.DB.GrantElevation("p1", "bot-a", "admin", "lead", "test", 1*60*60*1000*1000*1000) // 1h as Duration

	// With elevation: should be able to message bot-b
	allowed2, _ := r.DB.CanMessage("p1", "bot-a", "bot-b")
	if !allowed2 {
		t.Error("expected allowed WITH admin elevation")
	}
}

// --- Cross-Phase Integration ---

func TestAPIWebhookFiresAndRecordsHistory(t *testing.T) {
	r := testRelay(t)

	// Create trigger for "deploy" event
	_, _ = r.DB.UpsertTrigger("proj", "deploy", `{}`, "deployer", "deploy-cycle", "10m")

	// Fire via webhook
	doAPI(r, "POST", "/webhooks/proj/deploy", `{"env":"production"}`)

	// Check history
	history := r.DB.GetTriggerHistory("proj", 10)
	if len(history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(history))
	}
	if history[0].Event != "deploy" {
		t.Errorf("expected event=deploy, got %s", history[0].Event)
	}
}

func TestAPIQuotaEnforcementDB(t *testing.T) {
	r := testRelay(t)

	// Set very low quota
	_ = r.DB.SetAgentQuota("p1", "bot-a", 0, 1, 1, 0)

	// First message should be allowed
	allowed, _, _ := r.DB.CheckQuota("p1", "bot-a", "messages")
	if !allowed {
		t.Error("first message should be allowed (0 used)")
	}

	// Insert a message to count against quota
	_, _ = r.DB.InsertMessage("p1", "bot-a", "bot-b", "notification", "", "hello", "{}", "P2", 3600, nil, nil)

	// Second should be denied
	allowed2, used, limit := r.DB.CheckQuota("p1", "bot-a", "messages")
	if allowed2 {
		t.Errorf("second message should be denied (used=%d, limit=%d)", used, limit)
	}
}

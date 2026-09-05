package relay

import (
	"net/http"
	"testing"
)

// S6a (audit b983684b §1): POST /api/projects creates a project via the
// existing name primitives.
func TestAPICreateProject(t *testing.T) {
	r := testRelay(t)

	w := doAPI(r, http.MethodPost, "/projects", `{"name":"my-proj"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["created"]; got != true {
		t.Errorf("create: want created=true, got %v", got)
	}
	proj, err := r.DB.GetProject("my-proj")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if proj == nil {
		t.Fatal("project not persisted")
	}
}

// S6a: an underscore spelling folds to the canonical hyphen name (NormalizeProject).
func TestAPICreateProjectNormalizes(t *testing.T) {
	r := testRelay(t)

	w := doAPI(r, http.MethodPost, "/projects", `{"name":"My_Proj"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	proj, err := r.DB.GetProject("my-proj")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if proj == nil {
		t.Fatal("normalized project 'my-proj' not persisted")
	}
}

// S6a: existing refusals (empty name, reserved "default") surface as a 400 with
// the standard error envelope — not a silent create.
func TestAPICreateProjectRefusals(t *testing.T) {
	r := testRelay(t)

	cases := []struct {
		name string
		body string
	}{
		{"empty", `{"name":""}`},
		{"whitespace", `{"name":"   "}`},
		{"reserved-default", `{"name":"default"}`},
		{"reserved-default-cased", `{"name":"DEFAULT"}`},
		{"path-shaped", `{"name":".agentd"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doAPI(r, http.MethodPost, "/projects", c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: want 400, got %d (%s)", c.name, w.Code, w.Body.String())
			}
			if decodeJSON(t, w)["error"] == nil {
				t.Errorf("%s: want error envelope, got %s", c.name, w.Body.String())
			}
		})
	}
}

// S6a: a duplicate project surfaces as a 409 error, not a silent OK.
func TestAPICreateProjectDuplicate(t *testing.T) {
	r := testRelay(t)

	w := doAPI(r, http.MethodPost, "/projects", `{"name":"dup-proj"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("first create: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	w = doAPI(r, http.MethodPost, "/projects", `{"name":"dup-proj"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate: want 409, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["error"]; got != "project already exists" {
		t.Errorf("duplicate: want error 'project already exists', got %v", got)
	}
}

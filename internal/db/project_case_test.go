package db

import "testing"

// TestProjectNameCanonicalization verifies the projects registry is
// case-insensitive and underscore/hyphen-insensitive at the DB layer: mixed
// spellings resolve to one canonical row (no case-split dupes, no lost
// lookups). Ticket/e2f5ad14 #3.
func TestProjectNameCanonicalization(t *testing.T) {
	d := testDB(t)

	// Case-insensitive: two spellings collapse to one row.
	d.EnsureProject("Foo")
	d.EnsureProject("foo")
	d.EnsureProject("FOO")

	got, err := d.GetProject("fOo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got == nil {
		t.Fatal("expected 'Foo' to resolve to a project via any casing")
	}
	if got.Name != "foo" {
		t.Fatalf("expected canonical name 'foo', got %q", got.Name)
	}

	// Only one registry row exists despite three spellings.
	infos, err := d.ListProjectsWithInfo()
	if err != nil {
		t.Fatalf("ListProjectsWithInfo: %v", err)
	}
	fooCount := 0
	for _, p := range infos {
		if p.Name == "foo" {
			fooCount++
		}
		if p.Name == "Foo" || p.Name == "FOO" {
			t.Fatalf("non-canonical project row leaked: %q", p.Name)
		}
	}
	if fooCount != 1 {
		t.Fatalf("expected exactly 1 'foo' registry row, got %d", fooCount)
	}

	// Underscores fold to hyphens: 'a_b' and 'a-b' are one project.
	d.EnsureProject("Synergix_Prod")
	got2, _ := d.GetProject("synergix-prod")
	if got2 == nil || got2.Name != "synergix-prod" {
		t.Fatalf("expected 'Synergix_Prod' to resolve to 'synergix-prod', got %+v", got2)
	}

	// Delete resolves through any casing too.
	if err := d.DeleteProject("SYNERGIX-PROD"); err != nil {
		t.Fatalf("DeleteProject via uppercase: %v", err)
	}
	if g, _ := d.GetProject("synergix-prod"); g != nil {
		t.Fatal("expected 'synergix-prod' deleted via uppercase spelling")
	}

	// Internal "_"-prefixed pseudo-projects are exempt (left verbatim).
	if canonicalProject("_relay") != "_relay" {
		t.Fatalf("internal '_relay' must be left untouched, got %q", canonicalProject("_relay"))
	}
}

package db

import (
	"path/filepath"
	"testing"
	"time"
)

// Real transcript tokens and the legacy bytes/4 estimate must coexist in the
// same aggregate without double-counting: a row uses its real counts when
// present, else its bytes estimate.
func TestTokenUsageRealAndEstimateCoexist(t *testing.T) {
	d, err := NewTestDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer func() { _ = d.Close() }()

	now := time.Now().UTC().Format(time.RFC3339)
	if err := d.InsertTokenUsageBatch([]TokenRecord{
		{Project: "p", Agent: "a", Bytes: 400, CreatedAt: now},            // legacy → 100 tokens (400/4)
		{Project: "p", Agent: "a", Input: 60, Output: 40, CreatedAt: now}, // real → 100 tokens
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := d.GetTokenUsageByAgent("p", "1970-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 agent row, got %d", len(res))
	}
	if res[0].Tokens != 200 {
		t.Errorf("expected 200 tokens (100 estimate + 100 real), got %d", res[0].Tokens)
	}
}

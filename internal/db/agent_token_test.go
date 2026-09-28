package db

import "testing"

// TestMintAgentTokenStoresHashOnly (S3b 05525713): the clear token is never
// stored — only its sha256 — and re-minting invalidates the previous token.
func TestMintAgentTokenStoresHashOnly(t *testing.T) {
	d := testDB(t)
	regAgent(t, d, "p1", "bot-a")
	if d.AgentHasToken("p1", "bot-a") {
		t.Fatal("fresh agent must have no token")
	}
	tok, err := d.MintAgentToken("p1", "bot-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	var stored string
	if err := d.conn.QueryRow(`SELECT token_hash FROM agents WHERE project = 'p1' AND name = 'bot-a'`).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if stored == tok || stored != HashAgentToken(tok) {
		t.Fatalf("token_hash must be the sha256 of the token, never the token (stored %q)", stored)
	}
	if p, n, ok := d.AgentByToken(tok); !ok || p != "p1" || n != "bot-a" {
		t.Fatalf("AgentByToken: got %q %q %v", p, n, ok)
	}
	tok2, err := d.MintAgentToken("p1", "bot-a")
	if err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	if _, _, ok := d.AgentByToken(tok); ok || d.AgentTokenMatches("p1", "bot-a", tok) {
		t.Fatal("re-mint must invalidate the previous token")
	}
	if !d.AgentTokenMatches("p1", "bot-a", tok2) {
		t.Fatal("new token must match")
	}
	if _, err := d.MintAgentToken("p1", "ghost"); err == nil {
		t.Fatal("minting for an unregistered agent must fail")
	}
}

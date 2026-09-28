package db

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// Per-agent relay tokens (S3b 05525713, F-04/F-27). register_agent mints a
// random token, the relay keeps only its sha256, and a request carrying it
// (X-Agent-Token) is bound to that agent: it can act as itself only.

// HashAgentToken is the stored form of a token: sha256, hex.
func HashAgentToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MintAgentToken generates a fresh token for (project, name), stores its hash
// (replacing — and so invalidating — any previous token) and returns the clear
// token. It is the only place the clear token exists; the caller returns it
// once and never persists or logs it.
func (d *DB) MintAgentToken(project, name string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint agent token: %w", err)
	}
	token := "art_" + hex.EncodeToString(raw[:])
	res, err := d.writerExec(`UPDATE agents SET token_hash = ? WHERE project = ? AND name = ?`,
		HashAgentToken(token), project, name)
	if err != nil {
		return "", fmt.Errorf("mint agent token: %w", err)
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return "", fmt.Errorf("mint agent token: agent %q not registered in project %q", name, project)
	}
	return token, nil
}

// AgentTokenMatches reports whether token is the current token of (project,
// name). Constant-time on the hash; false when no token was ever minted.
func (d *DB) AgentTokenMatches(project, name, token string) bool {
	if token == "" {
		return false
	}
	var stored sql.NullString
	if err := d.ro().QueryRow(`SELECT token_hash FROM agents WHERE project = ? AND name = ?`, project, name).Scan(&stored); err != nil || !stored.Valid {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored.String), []byte(HashAgentToken(token))) == 1
}

// AgentByToken resolves a token to the agent it was minted for. ok=false for
// an unknown, rotated or empty token.
func (d *DB) AgentByToken(token string) (project, name string, ok bool) {
	if token == "" {
		return "", "", false
	}
	err := d.ro().QueryRow(`SELECT project, name FROM agents WHERE token_hash = ?`, HashAgentToken(token)).Scan(&project, &name)
	if err != nil {
		return "", "", false
	}
	return project, name, true
}

// AgentHasToken reports whether (project, name) already has a minted token.
func (d *DB) AgentHasToken(project, name string) bool {
	var stored sql.NullString
	err := d.ro().QueryRow(`SELECT token_hash FROM agents WHERE project = ? AND name = ?`, project, name).Scan(&stored)
	return err == nil && stored.Valid && stored.String != ""
}

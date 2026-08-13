package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxBody is the request body cap applied when RELAY_MAX_BODY is unset.
// 1 MiB comfortably exceeds any legitimate tool call (messages are capped at
// max_context_bytes) while blocking memory-exhaustion via oversized payloads.
const DefaultMaxBody int64 = 1 << 20

// Config holds server security settings loaded from environment variables.
// MaxBody defaults to DefaultMaxBody; the rest are opt-in (zero values preserve
// backward-compatible behavior).
type Config struct {
	APIKey      string   // RELAY_API_KEY: shared secret for Bearer auth
	CORSOrigins []string // RELAY_CORS_ORIGINS: allowed origins (comma-separated)
	MaxBody     int64    // RELAY_MAX_BODY: max request body in bytes (default 1 MiB; 0 disables)
	RateLimit   int      // RELAY_RATE_LIMIT: requests/minute per IP (opt-in; 0 = off)

	// Identity enforcement is no longer opt-in: every mutating tool call must
	// resolve a real agent registered in a real project (guardIdentity), and the
	// former RELAY_REQUIRE_REGISTERED env var is retired along with the
	// anonymous/default fallbacks it used to guard against.

	// LinearMode toggles Linear-SSOT mirror mode. Default false = degraded/native
	// mode (tasks live in the relay DB, kanban is writable). Surfaced via
	// /api/health and /api/settings so the web UI can detect the mode.
	// Set with RELAY_LINEAR_MODE=1 (or true). The Linear connector only spins up
	// (goroutines, webhook route, write-back) when LinearMode is true AND
	// LinearAPIKey is non-empty; otherwise behavior is byte-identical to native.
	LinearMode bool // RELAY_LINEAR_MODE

	// --- Linear connector (single workspace, personal API key) ---
	// Read from env only; never logged. The connector is inert unless LinearMode
	// is on and LinearAPIKey is set (see internal/connector/linear).
	LinearAPIKey        string        // LINEAR_API_KEY: GraphQL auth (personal key)
	LinearWebhookSecret string        // LINEAR_WEBHOOK_SECRET: HMAC secret for inbound webhooks
	LinearTeamKey       string        // LINEAR_TEAM_KEY: team key (e.g. SYN); reconcile + state scope
	LinearReconcileIval time.Duration // RELAY_LINEAR_RECONCILE_INTERVAL: active-cycle poll (default 5m)

	// --- Federation (relay-to-relay message forwarding) ---
	// FederationPeers lists trusted remote relays this instance can forward DMs
	// to and accept DMs from. Loaded from RELAY_FEDERATION_PEERS as a JSON array;
	// each peer carries a Label (local alias, used in `agent@label` addressing), a
	// base URL, a shared Token (peer-specific credential, NOT the global API key),
	// and the target Project on that peer. Empty = federation off (byte-identical
	// to today). Each peer's Token authenticates its inbound POSTs to this relay's
	// /api/federation/inbound route independently of RELAY_API_KEY.
	FederationPeers []FederationPeer // RELAY_FEDERATION_PEERS (JSON array)

	// Version is the build tag (from main.Version). Surfaced in /api/health
	// and MCP server info. Set by the caller before relay.New.
	Version string
}

// FederationPeer is one trusted remote relay. Label is a local alias (the "@peer"
// in addressing); Token is a per-peer shared secret; Project is the namespace on
// the peer that forwarded messages land in (default "default").
type FederationPeer struct {
	Label   string `json:"label"`
	URL     string `json:"url"`
	Token   string `json:"token"`
	Project string `json:"project"`
}

// LinearActive reports whether the Linear connector should run: mirror mode on
// plus an API key present. Without both, the no-op connector is used and the
// webhook route 404s — behavior identical to native mode.
func (c Config) LinearActive() bool {
	return c.LinearMode && c.LinearAPIKey != ""
}

// Load reads configuration from environment variables with safe defaults.
func Load() Config {
	cfg := Config{
		APIKey: os.Getenv("RELAY_API_KEY"),
	}

	if v := os.Getenv("RELAY_CORS_ORIGINS"); v != "" {
		for _, origin := range strings.Split(v, ",") {
			origin = strings.TrimSpace(origin)
			if origin != "" {
				cfg.CORSOrigins = append(cfg.CORSOrigins, origin)
			}
		}
	}

	cfg.MaxBody = DefaultMaxBody
	if v := os.Getenv("RELAY_MAX_BODY"); v != "" {
		// Explicit 0 opts out of the cap (unlimited); negatives are ignored.
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.MaxBody = n
		}
	}

	if v := os.Getenv("RELAY_RATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RateLimit = n
		}
	}

	if v := strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_LINEAR_MODE"))); v == "1" || v == "true" || v == "yes" {
		cfg.LinearMode = true
	}

	// Federation peers: JSON array in RELAY_FEDERATION_PEERS. A malformed value or
	// entries missing label/url/token are skipped (fail-open to "no such peer"),
	// never fatal — a bad env var must not stop the relay from booting.
	if v := strings.TrimSpace(os.Getenv("RELAY_FEDERATION_PEERS")); v != "" {
		var peers []FederationPeer
		if err := json.Unmarshal([]byte(v), &peers); err == nil {
			for _, p := range peers {
				p.Label = strings.ToLower(strings.TrimSpace(p.Label))
				p.URL = strings.TrimSpace(p.URL)
				p.Token = strings.TrimSpace(p.Token)
				p.Project = strings.TrimSpace(p.Project)
				if p.Label == "" || p.URL == "" || p.Token == "" {
					continue
				}
				cfg.FederationPeers = append(cfg.FederationPeers, p)
			}
		}
	}

	cfg.LinearAPIKey = os.Getenv("LINEAR_API_KEY")
	cfg.LinearWebhookSecret = os.Getenv("LINEAR_WEBHOOK_SECRET")
	cfg.LinearTeamKey = os.Getenv("LINEAR_TEAM_KEY")
	cfg.LinearReconcileIval = 5 * time.Minute
	if v := os.Getenv("RELAY_LINEAR_RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.LinearReconcileIval = d
		}
	}

	return cfg
}

package config

import "testing"

func TestMaxBodyDefault(t *testing.T) {
	t.Setenv("RELAY_MAX_BODY", "")
	if got := Load().MaxBody; got != DefaultMaxBody {
		t.Errorf("default MaxBody = %d, want %d", got, DefaultMaxBody)
	}
}

func TestMaxBodyOverride(t *testing.T) {
	t.Setenv("RELAY_MAX_BODY", "4096")
	if got := Load().MaxBody; got != 4096 {
		t.Errorf("MaxBody = %d, want 4096", got)
	}
}

func TestMaxBodyExplicitZeroDisables(t *testing.T) {
	t.Setenv("RELAY_MAX_BODY", "0")
	if got := Load().MaxBody; got != 0 {
		t.Errorf("MaxBody = %d, want 0 (unlimited)", got)
	}
}

func TestMaxBodyNegativeIgnored(t *testing.T) {
	t.Setenv("RELAY_MAX_BODY", "-5")
	if got := Load().MaxBody; got != DefaultMaxBody {
		t.Errorf("MaxBody = %d, want default %d on negative input", got, DefaultMaxBody)
	}
}

func TestRequireRegisteredOptIn(t *testing.T) {
	t.Setenv("RELAY_REQUIRE_REGISTERED", "")
	if Load().RequireRegistered {
		t.Error("RequireRegistered should default to false")
	}
	for _, v := range []string{"1", "true", "yes", "TRUE"} {
		t.Setenv("RELAY_REQUIRE_REGISTERED", v)
		if !Load().RequireRegistered {
			t.Errorf("RequireRegistered should be true for %q", v)
		}
	}
	t.Setenv("RELAY_REQUIRE_REGISTERED", "0")
	if Load().RequireRegistered {
		t.Error("RequireRegistered should be false for \"0\"")
	}
}

func TestRateLimitOptIn(t *testing.T) {
	t.Setenv("RELAY_RATE_LIMIT", "")
	if got := Load().RateLimit; got != 0 {
		t.Errorf("default RateLimit = %d, want 0 (off)", got)
	}
	t.Setenv("RELAY_RATE_LIMIT", "600")
	if got := Load().RateLimit; got != 600 {
		t.Errorf("RateLimit = %d, want 600", got)
	}
}

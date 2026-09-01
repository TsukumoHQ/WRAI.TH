package relay

import (
	"testing"
	"time"
)

// TestResolveReviewerTTL asserts RELAY_REVIEWER_TTL_DAYS overrides the default
// and that an invalid or non-positive value falls back to DefaultReviewerTTL.
func TestResolveReviewerTTL(t *testing.T) {
	cases := []struct {
		env  string
		set  bool
		want time.Duration
	}{
		{set: false, want: DefaultReviewerTTL},
		{env: "3", set: true, want: 3 * 24 * time.Hour},
		{env: "1", set: true, want: 24 * time.Hour},
		{env: "0", set: true, want: DefaultReviewerTTL},
		{env: "-1", set: true, want: DefaultReviewerTTL},
		{env: "x", set: true, want: DefaultReviewerTTL},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv("RELAY_REVIEWER_TTL_DAYS", c.env)
		} else {
			t.Setenv("RELAY_REVIEWER_TTL_DAYS", "")
		}
		if got := resolveReviewerTTL(); got != c.want {
			t.Errorf("RELAY_REVIEWER_TTL_DAYS=%q: got %s, want %s", c.env, got, c.want)
		}
	}
}

// TestResolveBackupKeep asserts RELAY_BACKUP_KEEP overrides the default and that
// an invalid or non-positive value falls back to DefaultBackupKeep (never 0).
func TestResolveBackupKeep(t *testing.T) {
	cases := []struct {
		env  string
		set  bool
		want int
	}{
		{set: false, want: DefaultBackupKeep},
		{env: "5", set: true, want: 5},
		{env: "1", set: true, want: 1},
		{env: "0", set: true, want: DefaultBackupKeep},
		{env: "-2", set: true, want: DefaultBackupKeep},
		{env: "abc", set: true, want: DefaultBackupKeep},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv("RELAY_BACKUP_KEEP", c.env)
		} else {
			t.Setenv("RELAY_BACKUP_KEEP", "")
		}
		if got := resolveBackupKeep(); got != c.want {
			t.Errorf("RELAY_BACKUP_KEEP=%q: got %d, want %d", c.env, got, c.want)
		}
	}
}

package db

import (
	"testing"

	"agent-relay/internal/models"
)

// TestSenderEligibility pins the T2 verdict table: the single helper every send /
// ack / is_eligible path consults, so all three always agree.
func TestSenderEligibility(t *testing.T) {
	cases := []struct {
		name         string
		agent        *models.Agent
		wantEligible bool
		wantReason   string
	}{
		{"nil is unregistered", nil, false, "unregistered"},
		{"active is eligible", &models.Agent{Status: "active"}, true, "active"},
		{"sleeping is eligible", &models.Agent{Status: "sleeping"}, true, "sleeping"},
		{"inactive is refused", &models.Agent{Status: "inactive"}, false, "inactive"},
		{"deleted is refused", &models.Agent{Status: "deleted"}, false, "deleted"},
		// A service identity is eligible regardless of status — a monitoring/QA
		// daemon must post feedback even when it (and every worker) looks dead.
		{"service is eligible even when inactive", &models.Agent{Status: "inactive", IsService: true}, true, "service"},
		{"service is eligible even when deleted", &models.Agent{Status: "deleted", IsService: true}, true, "service"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotEligible, gotReason := SenderEligibility(c.agent)
			if gotEligible != c.wantEligible || gotReason != c.wantReason {
				t.Fatalf("SenderEligibility = (%v, %q), want (%v, %q)", gotEligible, gotReason, c.wantEligible, c.wantReason)
			}
		})
	}
}

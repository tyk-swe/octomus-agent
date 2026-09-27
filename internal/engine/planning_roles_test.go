package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

// Each discovery agent takes its focus from discoveryScopes by index, so no
// agent count the configuration accepts may exceed the scopes. A larger count
// is refused before any session is admitted instead of panicking inside a
// role goroutine.
func TestDiscoveryScopesCoverEveryAcceptedAgentCount(t *testing.T) {
	cfg := testConfig(t.TempDir())
	scopes := uint64(len(discoveryScopes))
	accepted := 0
	for agents := uint64(0); agents <= 2*scopes; agents++ {
		cfg.DiscoveryAgents = agents
		for name, validate := range map[string]func(config.Config) error{
			"Validate(false)": func(c config.Config) error { return c.Validate(false) },
			"Validate(true)":  func(c config.Config) error { return c.Validate(true) },
			"ValidateAudit":   config.Config.ValidateAudit,
		} {
			if validate(cfg) != nil {
				continue
			}
			if name == "Validate(false)" {
				accepted++
			}
			if agents > scopes {
				t.Fatalf("%s accepts %d discovery agents; only %d scopes exist", name, agents, scopes)
			}
		}
	}
	if accepted == 0 {
		t.Fatal("no discovery agent count validated, so the bound was not checked")
	}

	app := New(testStore(t), t.TempDir())
	t.Cleanup(app.Shutdown)
	cfg.DiscoveryAgents = scopes + 1
	cycle := model.Cycle{ID: model.ID(), Mode: model.CycleModeAudit, Grounding: &model.Grounding{Revision: "revision"}}
	want := fmt.Sprintf("Discovery supports at most %d agents", scopes)
	if err := app.discover(context.Background(), cfg, &cycle, "ground", "{}"); err == nil || err.Error() != want {
		t.Fatalf("discover with %d agents = %v; want %q", cfg.DiscoveryAgents, err, want)
	}
	if len(cycle.Sessions) != 0 || len(cycle.Proposals) != 0 {
		t.Fatalf("refused discovery still ran: sessions=%d proposals=%d", len(cycle.Sessions), len(cycle.Proposals))
	}
}

// Every reviewer slot has its own adversarial brief. The runner fixtures
// recognize a reviewer turn by its "Adversarial proposal review" prefix.
func TestEveryReviewerSlotHasAnIndependentFocus(t *testing.T) {
	slots := model.ReviewerSlots()
	if len(reviewFocus) != len(slots) {
		t.Fatalf("review focus covers %d slots; reviewer slots are %v", len(reviewFocus), slots)
	}
	seen := map[string]string{}
	for _, slot := range slots {
		focus, ok := reviewFocus[slot]
		if !ok || !strings.HasPrefix(focus, "Adversarial proposal review ") {
			t.Fatalf("reviewer slot %s has focus %q", slot, focus)
		}
		if other, duplicate := seen[focus]; duplicate {
			t.Fatalf("reviewer slots %s and %s share one focus", other, slot)
		}
		seen[focus] = slot
	}
}

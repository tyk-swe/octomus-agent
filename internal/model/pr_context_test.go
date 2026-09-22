package model

import (
	"encoding/json"
	"testing"
)

// TestLegacyGroundingLoadsWithEmptyExternalContext ports
// legacy_grounding_loads_with_empty_external_context (tests/pr_context.rs): a
// grounding saved before external PR context existed still loads, with empty
// context and incomplete coverage.
func TestLegacyGroundingLoadsWithEmptyExternalContext(t *testing.T) {
	var g Grounding
	legacy := `{"revision":"rev","prs":[],"history":{},"maintenance_due":false,"maintenance_targets":[]}`
	if err := json.Unmarshal([]byte(legacy), &g); err != nil {
		t.Fatalf("legacy grounding rejected: %v", err)
	}
	if len(g.ExternalPRs) != 0 {
		t.Fatalf("external_prs = %+v; want empty", g.ExternalPRs)
	}
	if g.PRCoverage.Complete {
		t.Fatal("legacy coverage claims completeness")
	}
	if g.PRCoverage.ObservedAt != nil {
		t.Fatalf("legacy coverage observed_at = %q; want none", *g.PRCoverage.ObservedAt)
	}
	if g.PRCoverage.TotalExternal != 0 {
		t.Fatalf("legacy coverage total_external = %d; want 0", g.PRCoverage.TotalExternal)
	}
}

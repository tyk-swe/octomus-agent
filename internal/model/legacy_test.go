package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/jsoncompat"
)

// Port of tests/core.rs legacy_cycles_default_to_execution_without_rewriting_evidence.
func TestLegacyCyclesDefaultToExecutionWithoutRewritingEvidence(t *testing.T) {
	old := `{"id":"legacy","number":7,"status":"completed","started_at":"2026-09-08T00:00:00Z","completed_at":"2026-09-08T00:05:00Z","grounding":null,"proposals":[],"assessments":[],"sessions":[],"error":null}`
	var cycle Cycle
	if err := json.Unmarshal([]byte(old), &cycle); err != nil {
		t.Fatal(err)
	}
	if cycle.Mode != CycleModeExecution {
		t.Fatalf("legacy cycle mode: %v", cycle.Mode)
	}
	data, err := jsoncompat.Marshal(&cycle)
	if err != nil {
		t.Fatal(err)
	}
	var upgraded map[string]any
	if err := json.Unmarshal(data, &upgraded); err != nil {
		t.Fatal(err)
	}
	mode, ok := upgraded["mode"]
	if !ok || mode != "execution" {
		t.Fatalf("upgraded mode: %v (present %v)", mode, ok)
	}
	delete(upgraded, "mode")
	var original map[string]any
	if err := json.Unmarshal([]byte(old), &original); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(upgraded, original) {
		t.Fatalf("legacy evidence rewritten\n got: %s\nwant: %s", data, old)
	}
}

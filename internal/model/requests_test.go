package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperatorRequestStrictDecode(t *testing.T) {
	record := `{"id":"abc","repository":"Fixture/Project","title":"T","description":"D","target":"main","status":"pending","created_at":"2026-01-01T00:00:00+00:00","updated_at":"2026-01-01T00:00:00+00:00","assessment":null,"task_id":null}`
	var request OperatorRequest
	if err := json.Unmarshal([]byte(record), &request); err != nil {
		t.Fatalf("record refused: %v", err)
	}
	if request.Status != RequestPending || request.ProposalID() != "request-abc" {
		t.Fatalf("%+v", request)
	}
	saved, err := json.Marshal(request)
	if err != nil || string(saved) != record {
		t.Fatalf("round trip:\n%s\nwant\n%s", saved, record)
	}
	// Saved records tolerate fields a newer binary added; the strict API body is covered in httpapi.
	var assessment RequestAssessment
	if err := json.Unmarshal([]byte(`{"cycle_id":"c","mode":"audit","decision":"deferred","reason":"r","at":"t","extra":1}`), &assessment); err != nil {
		t.Fatalf("forward-compatible assessment decode: %v", err)
	}
	var status RequestStatus
	if err := json.Unmarshal([]byte(`"bogus"`), &status); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown status: %v", err)
	}
}

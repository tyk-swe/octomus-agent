package model

import "testing"

func TestVerificationSelectionFollowsRecordedOrder(t *testing.T) {
	task := Task{Verification: []Verification{
		{Command: "test", Revision: "reviewed", Success: true, CreatedAt: "2026-10-01T00:00:00Z"},
		{Command: "unrelated", Revision: "reviewed", Success: true},
		{Command: "test", Revision: "reviewed", Success: false, CreatedAt: "2026-09-01T00:00:00Z"},
	}}
	history := task.VerificationFor("test")
	if history.Recorded != 2 || history.Latest == nil || history.Latest.Success {
		t.Fatalf("newer saved failure did not supersede the older pass: %+v", history)
	}
	task.Verification = append(task.Verification, Verification{Command: "test", Revision: "other", Success: true})
	history = task.VerificationFor("test")
	if history.Recorded != 3 || history.Latest == nil || history.Latest.Revision != "other" {
		t.Fatalf("wrong-revision evidence was skipped: %+v", history)
	}
	if missing := task.VerificationFor("missing"); missing.Latest != nil || missing.Recorded != 0 {
		t.Fatalf("missing evidence selected a result: %+v", missing)
	}
}

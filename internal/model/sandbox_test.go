package model_test

import (
	"fmt"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func run(oom bool, allowed, denied map[string]uint64) *model.SandboxRecord {
	return &model.SandboxRecord{ImageID: "sha256:image", Runs: 1, OOM: oom, Egress: model.SandboxEgress{Allowed: allowed, Denied: denied}}
}

func TestMergeSandboxCountsRunsHostsAndKeepsAnOOM(t *testing.T) {
	t.Parallel()
	merged := model.MergeSandbox(nil, run(true, map[string]uint64{"api.openai.com:443": 2}, map[string]uint64{"example.com:443": 1}))
	unreachable := run(false, map[string]uint64{"api.openai.com:443": 3}, map[string]uint64{})
	unreachable.Egress.Failed = map[string]uint64{"registry.npmjs.org:443": 2}
	merged = model.MergeSandbox(merged, unreachable)
	merged = model.MergeSandbox(merged, nil)
	if merged.Runs != 2 || !merged.OOM || merged.Egress.Allowed["api.openai.com:443"] != 5 || merged.Egress.Denied["example.com:443"] != 1 ||
		len(merged.Egress.Denied) != 1 || merged.Egress.Failed["registry.npmjs.org:443"] != 2 {
		t.Fatalf("merged = %+v", merged)
	}
	many := map[string]uint64{}
	for i := range model.SandboxHostLimit + 10 {
		many[fmt.Sprintf("host-%03d.example:443", i)] = 1
	}
	bounded := model.MergeSandbox(nil, run(false, nil, many))
	if len(bounded.Egress.Denied) != model.SandboxHostLimit+1 || bounded.Egress.Denied["other"] != 10 {
		t.Fatalf("bounded denied hosts = %d, other = %d", len(bounded.Egress.Denied), bounded.Egress.Denied["other"])
	}
}

func TestMergeSandboxKeepsMixedIdentity(t *testing.T) {
	t.Parallel()
	imaged := func(id, runtime string) *model.SandboxRecord {
		return &model.SandboxRecord{ImageID: id, Runtime: runtime, Runs: 1}
	}
	merged := model.MergeSandbox(nil, imaged("sha256:a", "runc"))
	merged = model.MergeSandbox(merged, imaged("sha256:b", "runc"))
	if merged.ImageID != "mixed" || merged.Runtime != "" {
		t.Fatalf("two images merged = %+v", merged)
	}
	merged = model.MergeSandbox(merged, imaged("sha256:a", "runc"))
	if merged.ImageID != "mixed" || merged.Runtime != "" {
		t.Fatalf("mixed record reverted to one image = %+v", merged)
	}
	same := model.MergeSandbox(nil, imaged("sha256:a", "runc"))
	same = model.MergeSandbox(same, imaged("sha256:a", "runc"))
	if same.ImageID != "sha256:a" || same.Runtime != "runc" {
		t.Fatalf("identical runs = %+v", same)
	}
	same = model.MergeSandbox(same, imaged("sha256:a", "runsc"))
	if same.ImageID != "mixed" || same.Runtime != "" {
		t.Fatalf("runtime change = %+v", same)
	}
}

func TestSessionsWithoutSandboxRecordsStillLoad(t *testing.T) {
	t.Parallel()
	var session model.Session
	legacy := `{"id":"s1","role":"executor","route":{"backend":"codex","model":"m","effort":"low"},"status":"completed","started_at":"2026-09-01T00:00:00Z","summary":"done"}`
	if err := wirejson.DecodeRecord([]byte(legacy), &session); err != nil || session.Sandbox != nil {
		t.Fatalf("legacy session = %+v, %v", session, err)
	}
	session.Sandbox = run(false, map[string]uint64{"api.openai.com:443": 1}, nil)
	data, err := wirejson.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var again model.Session
	if err := wirejson.DecodeRecord(data, &again); err != nil || again.Sandbox.Egress.Allowed["api.openai.com:443"] != 1 || again.Sandbox.Egress.Denied == nil {
		t.Fatalf("round trip = %+v, %v", again.Sandbox, err)
	}
}

func TestSandboxRecordsWithoutEgressFailuresStillLoad(t *testing.T) {
	t.Parallel()
	var record model.SandboxRecord
	saved := `{"image_id":"sha256:image","runtime":"","runs":1,"oom":false,"egress":{"allowed":{},"denied":{"example.com:443":2}}}`
	if err := wirejson.DecodeRecord([]byte(saved), &record); err != nil || record.Egress.Failed == nil || len(record.Egress.Failed) != 0 ||
		record.Egress.Denied["example.com:443"] != 2 {
		t.Fatalf("record saved before egress failures = %+v, %v", record, err)
	}
	merged := model.MergeSandbox(&record, &model.SandboxRecord{ImageID: "sha256:image", Runs: 1,
		Egress: model.SandboxEgress{Failed: map[string]uint64{"api.openai.com:443": 1}}})
	data, err := wirejson.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	var again model.SandboxRecord
	if err := wirejson.DecodeRecord(data, &again); err != nil || again.Egress.Failed["api.openai.com:443"] != 1 || again.Runs != 2 {
		t.Fatalf("round trip = %+v, %v", again, err)
	}
}

func TestIncompleteSandboxEvidenceIsKeptThroughMerges(t *testing.T) {
	t.Parallel()
	var record model.SandboxRecord
	saved := `{"image_id":"sha256:image","runtime":"","runs":1,"oom":false,"egress":{"allowed":{},"denied":{},"failed":{}}}`
	if err := wirejson.DecodeRecord([]byte(saved), &record); err != nil || record.Incomplete {
		t.Fatalf("record saved before lost evidence was marked = %+v, %v", record, err)
	}
	merged := model.MergeSandbox(&record, &model.SandboxRecord{ImageID: "sha256:image", Runs: 1, Incomplete: true})
	merged = model.MergeSandbox(merged, run(false, nil, nil))
	if !merged.Incomplete || merged.Runs != 3 {
		t.Fatalf("a run whose evidence was lost no longer marks the record = %+v", merged)
	}
}

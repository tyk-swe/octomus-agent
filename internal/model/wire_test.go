package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestReviewJSONContract(t *testing.T) {
	data, err := json.Marshal(Review{Completed: true, Summary: "Full diff reviewed"})
	if err != nil || string(data) != `{"completed":true,"summary":"Full diff reviewed","findings":[]}` {
		t.Fatalf("review JSON = %s, %v", data, err)
	}
	for _, raw := range []string{
		`{"completed":true,"summary":"ok","findings":[],"unknown":1}`,
		`{"completed":true,"completed":false,"summary":"ok","findings":[]}`,
		`{"completed":null,"summary":"ok","findings":[]}`,
	} {
		var review Review
		if err := json.Unmarshal([]byte(raw), &review); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestStatusJSONUsesCurrentNames(t *testing.T) {
	var status Status
	if err := json.Unmarshal([]byte(`"published"`), &status); err != nil || status != StatusPublished {
		t.Fatalf("published status = %v, %v", status, err)
	}
	if err := json.Unmarshal([]byte(`{"published":null}`), &status); err == nil {
		t.Fatal("accepted object enum")
	}
}

func TestCycleLifecycleIsOmittedOnlyWhileEmpty(t *testing.T) {
	archived, discarded := "archived-at", "discarded-at"
	for _, tc := range []struct {
		lifecycle WorkspaceLifecycle
		want      string
	}{
		{WorkspaceLifecycle{}, ""},
		{WorkspaceLifecycle{ArchivedAt: &archived}, `"lifecycle":{"archived_at":"archived-at","discarded_at":null}`},
		{WorkspaceLifecycle{DiscardedAt: &discarded}, `"lifecycle":{"archived_at":null,"discarded_at":"discarded-at"}`},
	} {
		data, err := json.Marshal(Cycle{Lifecycle: tc.lifecycle})
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == "" && strings.Contains(string(data), `"lifecycle"`) {
			t.Fatalf("empty lifecycle emitted: %s", data)
		}
		if tc.want != "" && !strings.Contains(string(data), tc.want) {
			t.Fatalf("lifecycle %+v = %s; want %s", tc.lifecycle, data, tc.want)
		}
		var decoded Cycle
		if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded.Lifecycle, tc.lifecycle) {
			t.Fatalf("lifecycle round trip = %+v, %v", decoded.Lifecycle, err)
		}
	}
}

// Records decode from a zero value, so a reused variable never keeps optional
// fields the JSON leaves out.
func TestDecodingIntoAReusedRecordResetsAbsentFields(t *testing.T) {
	data, err := json.Marshal(Cycle{ID: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"run_id"`) || strings.Contains(string(data), `"lifecycle"`) || strings.Contains(string(data), `"repository"`) {
		t.Fatalf("optional cycle fields emitted: %s", data)
	}
	runID, archived := "stale-run", "stale-archive"
	cycle := Cycle{ID: "stale", RunID: &runID, Repository: "stale/repository", DecisionMemory: []any{"stale"}, Lifecycle: WorkspaceLifecycle{ArchivedAt: &archived}}
	if err := json.Unmarshal(data, &cycle); err != nil {
		t.Fatal(err)
	}
	if cycle.ID != "fresh" || cycle.RunID != nil || cycle.Repository != "" || len(cycle.DecisionMemory) != 0 || cycle.Lifecycle != (WorkspaceLifecycle{}) {
		t.Fatalf("reused cycle kept stale fields: %+v", cycle)
	}
}

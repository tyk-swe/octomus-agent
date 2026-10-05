// The golden v0.1.0 database served through the API: every saved record reads back.

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

const (
	goldenCompletedCycle = "4306e9ae-0833-47e5-b569-14b7b1d7d4f8"
	goldenFailedCycle    = "0346c423-2759-4071-bd68-a87ed546ebcb"
	goldenFirstTask      = "e166cfa0-816b-4173-9d30-e22addfa1c6a"
	goldenSecondTask     = "b0ed2705-4eb3-4d41-bc9f-d3a55e9e9556"
	goldenThirdTask      = "d1351dc0-f45b-45f9-9069-5f6a9866c3dd"
)

func TestGoldenStateReadsBack(t *testing.T) {
	dir := t.TempDir()
	testutil.GoldenState(t, "0.1.0", filepath.Join(dir, "state.db"))
	state := openStore(t, dir)
	app := engine.New(state, dir)
	router := Router(app, token, "", "test")

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := call(t, router, "GET", "/api"+path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
		return response
	}
	object := func(path string) map[string]any {
		t.Helper()
		return decode(t, get(path))
	}
	list := func(path string) []any {
		t.Helper()
		var value []any
		if err := json.Unmarshal(get(path).Body.Bytes(), &value); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return value
	}

	view := object("/state")
	// The golden's repository path no longer exists, so the saved configuration
	// cannot validate; the API must still serve the recorded state.
	if view["status"] != "paused" || view["configured"] != false || view["cycle_active"] != false {
		t.Fatalf("state: %v %v %v", view["status"], view["configured"], view["cycle_active"])
	}
	if view["repository"] != "fixture/project" {
		t.Fatalf("repository: %v", view["repository"])
	}
	counts, ok := view["counts"].(map[string]any)
	if !ok || counts["published"] != float64(3) {
		t.Fatalf("counts: %v", view["counts"])
	}
	if len(view["tasks"].([]any)) != 3 || len(view["cycles"].([]any)) != 2 || len(view["prs"].([]any)) != 1 {
		t.Fatalf("tasks/cycles/prs: %v %v %v", len(view["tasks"].([]any)), len(view["cycles"].([]any)), len(view["prs"].([]any)))
	}
	if view["merged_prs"] != float64(0) {
		t.Fatalf("merged_prs: %v", view["merged_prs"])
	}

	for _, id := range []string{goldenFirstTask, goldenSecondTask, goldenThirdTask} {
		task := object("/tasks/" + id)
		if task["id"] != id || task["status"] != "published" || task["cycle_id"] != goldenCompletedCycle || task["branch"] != "octomus/existing" || task["pr_number"] != float64(42) {
			t.Fatalf("task %s: %v", id, task)
		}
		if task["allowed_actions"] == nil {
			t.Fatalf("task %s missing allowed_actions", id)
		}
	}
	first := object("/tasks/" + goldenFirstTask)
	if first["lifecycle"].(map[string]any)["archived_at"] == nil {
		t.Fatalf("first task must be archived: %v", first["lifecycle"])
	}
	for _, id := range []string{goldenSecondTask, goldenThirdTask} {
		if object("/tasks/" + id)["lifecycle"].(map[string]any)["archived_at"] != nil {
			t.Fatalf("task %s must not be archived", id)
		}
	}

	cycles := object("/cycles")
	if len(cycles["items"].([]any)) != 2 {
		t.Fatalf("cycles: %v", cycles)
	}
	completed := object("/cycles/" + goldenCompletedCycle)
	if completed["mode"] != "execution" || completed["number"] != float64(1) || completed["status"] != "completed" || len(completed["proposals"].([]any)) != 3 {
		t.Fatalf("completed cycle: %v", completed)
	}
	failed := object("/cycles/" + goldenFailedCycle)
	if failed["status"] != "failed" || failed["number"] != float64(2) || failed["error"] == nil {
		t.Fatalf("failed cycle: %v", failed)
	}

	evidence := object("/cycles/" + goldenCompletedCycle + "/evidence")
	if evidence["kind"] != "recorded_review_check_evidence" || evidence["schema_version"] != float64(1) {
		t.Fatalf("evidence: %v", evidence)
	}
	ec := evidence["cycle"].(map[string]any)
	if ec["id"] != goldenCompletedCycle || ec["status"] != "completed" {
		t.Fatalf("evidence cycle: %v", ec)
	}
	planning := ec["planning"].(map[string]any)
	if planning["proposal_count"] != float64(3) || planning["planning_finished"] != true {
		t.Fatalf("planning: %v", planning)
	}
	linked := 0
	for _, proposal := range evidence["proposals"].([]any) {
		linked += len(proposal.(map[string]any)["linked_tasks"].([]any))
	}
	if linked != 3 {
		t.Fatalf("linked tasks: %d", linked)
	}
	failedEvidence := object("/cycles/" + goldenFailedCycle + "/evidence")
	fc := failedEvidence["cycle"].(map[string]any)
	if fc["status"] != "failed" || fc["planning"].(map[string]any)["error_recorded"] != true {
		t.Fatalf("failed evidence: %v", fc)
	}

	if len(object("/proposals")["items"].([]any)) != 6 {
		t.Fatal("expected six proposals across the two cycles")
	}
	prs := object("/prs")["items"].([]any)
	if len(prs) != 1 || prs[0].(map[string]any)["pr"].(map[string]any)["number"] != float64(42) {
		t.Fatalf("prs: %v", prs)
	}
	events := list("/events")
	const wantUpgrades = 1 // One final event for the whole v0.1.0 upgrade chain.
	upgrades := 0
	for _, event := range events {
		if event.(map[string]any)["kind"] == "upgrade" {
			upgrades++
		}
	}
	if upgrades != wantUpgrades || len(events) != 103+wantUpgrades {
		t.Fatalf("%d events, %d upgrade events; want %d and %d", len(events), upgrades, 103+wantUpgrades, wantUpgrades)
	}
	settings := object("/config")
	if settings["config"].(map[string]any)["github_repo"] != "fixture/project" || len(settings["revision"].(string)) != 64 {
		t.Fatalf("config: %v", settings["revision"])
	}
}

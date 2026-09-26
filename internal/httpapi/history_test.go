package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

// History pages accept any unsigned limit: the store clamps it to 1..100, so an
// oversized limit is the page cap rather than a wrapped negative number, and
// only a malformed value is a request error.
func TestHistoryLimitClampsOversizedValuesAndRejectsMalformedOnes(t *testing.T) {
	app, state := testApp(t)
	cfg := config.Default()
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		task := queuedTask(cfg)
		task.ID = id
		if err := state.Put("task", id, task); err != nil {
			t.Fatal(err)
		}
	}
	router := Router(app, token, "", "test")
	for _, check := range []struct {
		limit string
		items int
	}{
		{"18446744073709551615", 3},
		{"9223372036854775808", 3},
		{"1000", 3},
		{"2", 2},
		{"0", 1},
	} {
		response := call(t, router, "GET", "/api/tasks?limit="+check.limit, "")
		if response.Code != http.StatusOK {
			t.Fatalf("limit=%s: %d %s", check.limit, response.Code, response.Body.String())
		}
		if items, _ := decode(t, response)["items"].([]any); len(items) != check.items {
			t.Fatalf("limit=%s: %d items, want %d", check.limit, len(items), check.items)
		}
	}
	for _, limit := range []string{"abc", "-1", "18446744073709551616"} {
		response := call(t, router, "GET", "/api/tasks?limit="+limit, "")
		if response.Code != http.StatusBadRequest ||
			response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
			!strings.HasPrefix(response.Body.String(), "Invalid query string") {
			t.Fatalf("limit=%s: %d %q %s", limit, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
}

// Each history route pages only its own record kind.
func TestHistoryRoutesPageTheirOwnKind(t *testing.T) {
	app, state := testApp(t)
	task := queuedTask(config.Default())
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: "cycle-history", Number: 3, Status: "completed",
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{},
		Sessions: []model.Session{}, Repository: "fixture/project",
	}
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	pr := map[string]any{"repository": "fixture/project", "pr": map[string]any{"number": 42, "title": "Fixture PR", "state": "open"}}
	if err := state.Put("pr", "fixture/project:42", pr); err != nil {
		t.Fatal(err)
	}
	router := Router(app, token, "", "test")
	only := func(path string) map[string]any {
		t.Helper()
		response := call(t, router, "GET", path, "")
		items, _ := decode(t, response)["items"].([]any)
		if response.Code != http.StatusOK || len(items) != 1 {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		item, _ := items[0].(map[string]any)
		return item
	}
	if item := only("/api/tasks"); item["id"] != task.ID {
		t.Fatalf("tasks: %v", item)
	}
	if item := only("/api/cycles"); item["id"] != cycle.ID || item["number"] != float64(3) {
		t.Fatalf("cycles: %v", item)
	}
	if item := only("/api/prs"); item["repository"] != "fixture/project" || item["pr"].(map[string]any)["number"] != float64(42) {
		t.Fatalf("prs: %v", item)
	}
}

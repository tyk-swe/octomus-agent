package httpapi

// Ports of tests/evidence.rs cycle routes: evidence_route_requires_auth_and_
// reports_unknown_cycles (:639) and existing_cycle_action_routes_are_preserved
// (:677).
import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func cycleRecord(id string) model.Cycle {
	return model.Cycle{
		Mode: model.CycleModeExecution, ID: id, Number: 1, Status: model.CycleCompleted,
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{},
		Sessions: []model.Session{}, Repository: "fixture/project",
	}
}

// The evidence export is authenticated like every other API read: no token and
// a wrong token both get 401, an unknown cycle gets 404 with the reference
// wording, and a known cycle gets the export document.
func TestCycleEvidenceRouteRequiresAuthAndSeparatesUnknownCycles(t *testing.T) {
	app, state := testApp(t)
	if err := state.Put("cycle", "cycle-a", cycleRecord("cycle-a")); err != nil {
		t.Fatal(err)
	}
	router := Router(app, token, "", "test")

	if response := request(t, router, "GET", "/api/cycles/cycle-a/evidence", "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", response.Code)
	}
	req := httptest.NewRequest("GET", "/api/cycles/cycle-a/evidence", nil)
	req.Header.Set("Authorization", "Bearer not-the-operator-token-000000000000")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", recorder.Code)
	}

	response := call(t, router, "GET", "/api/cycles/cycle-a/evidence", "")
	if response.Code != http.StatusOK {
		t.Fatalf("known cycle: %d %s", response.Code, response.Body.String())
	}
	if body := decode(t, response); body["cycle"] == nil || body["generated_at"] == nil {
		t.Fatalf("evidence body: %v", body)
	}

	response = call(t, router, "GET", "/api/cycles/cycle-missing/evidence", "")
	if response.Code != http.StatusNotFound || decode(t, response)["error"] != "Cycle not found" {
		t.Fatalf("unknown cycle: %d %s", response.Code, response.Body.String())
	}
}

// Archiving a cycle persists lifecycle.archived_at and the detail route keeps
// serving the archived record; an unrecognized action still reports 404.
func TestCycleDetailStaysReadableAfterArchivePersistsLifecycle(t *testing.T) {
	app, state := testApp(t)
	if err := state.Put("cycle", "cycle-a", cycleRecord("cycle-a")); err != nil {
		t.Fatal(err)
	}
	router := Router(app, token, "", "test")

	response := call(t, router, "POST", "/api/cycles/cycle-a/archive", "{}")
	if response.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", response.Code, response.Body.String())
	}
	archived, err := store.Get[model.Cycle](state, "cycle", "cycle-a")
	if err != nil || archived == nil || archived.Lifecycle.ArchivedAt == nil {
		t.Fatalf("archive did not persist lifecycle.archived_at: %+v, %v", archived, err)
	}

	response = call(t, router, "GET", "/api/cycles/cycle-a", "")
	if response.Code != http.StatusOK {
		t.Fatalf("detail after archive: %d %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	lifecycle, _ := body["lifecycle"].(map[string]any)
	if body["id"] != "cycle-a" || lifecycle["archived_at"] == nil {
		t.Fatalf("detail body lost the archive evidence: %v", body)
	}
	if response := call(t, router, "POST", "/api/cycles/cycle-a/bogus", "{}"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", response.Code)
	}
}

package httpapi

// Cycle routes preserve authentication, detail and action status behavior.
import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func cycleRecord(id string) model.Cycle {
	return model.Cycle{
		Mode: model.CycleModeExecution, ID: id, Number: 1, Status: model.CycleCompleted,
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{},
		Sessions: []model.Session{}, Repository: "fixture/project",
	}
}

// The evidence export is authenticated like every other API read: no token and
// a wrong token both get 401, an unknown cycle gets 404 with the expected
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

// A cycle discard admitted through the API holds no scheduler gate while its
// planning directory is removed: pause answers through the boundary, the
// claimed cycle conflicts a duplicate discard with 409, and the original
// request completes the durable mark once removal finishes. The injected
// removal barrier makes the ordering deterministic — the bounded waits detect
// the pre-fix gate-holding deadlock rather than measuring timing.
func TestCycleDiscardOverHTTPLeavesControlsResponsive(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	id := model.ID()
	cycleDir := filepath.Join(dir, "cycles", id)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	app := engine.New(state, dir, engine.WithWorkspaceRemoval(func(root, path string) error {
		if path == cycleDir {
			once.Do(func() { close(entered) })
			<-release
		}
		return workspace.RemoveOwnedDir(root, path)
	}))
	t.Cleanup(app.Shutdown)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	cycle := cycleRecord(id)
	cycle.Lifecycle.ArchivedAt = stringPointer(model.Now())
	if err := state.Put("cycle", id, cycle); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cycleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cycleDir, "planning.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	router := Router(app, token, "", "test")

	discarded := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		discarded <- call(t, router, "POST", "/api/cycles/"+id+"/discard", "{}")
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("discard did not reach the held removal")
	}

	if response := call(t, router, "POST", "/api/control/pause", "{}"); response.Code != http.StatusOK {
		t.Fatalf("pause during held cycle removal: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/cycles/"+id+"/discard", "{}"); response.Code != http.StatusConflict {
		t.Fatalf("duplicate discard on the claimed cycle: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/cycles/"+id+"/archive", "{}"); response.Code != http.StatusConflict {
		t.Fatalf("archive on the claimed cycle: %d %s", response.Code, response.Body.String())
	}

	releaseOnce.Do(func() { close(release) })
	var response *httptest.ResponseRecorder
	select {
	case response = <-discarded:
	case <-time.After(30 * time.Second):
		t.Fatal("discard did not finish after the held removal released")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("discard: %d %s", response.Code, response.Body.String())
	}
	saved, err := store.Get[model.Cycle](state, "cycle", id)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("discarded record: %+v, %v", saved, err)
	}
	if _, err := os.Stat(cycleDir); !os.IsNotExist(err) {
		t.Fatalf("cycle directory still present: %v", err)
	}
}

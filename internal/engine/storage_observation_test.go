package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestIncompleteStorageObservationPreservesLastCompleteSnapshot(t *testing.T) {
	t.Parallel()
	for _, subtree := range []string{"tasks", "cycles", "other"} {
		t.Run(subtree, func(t *testing.T) {
			state := testStore(t)
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			cfg := testConfig(t.TempDir())
			if err := app.measureStorage(cfg); err != nil {
				t.Fatal(err)
			}
			before, found, err := state.GetRaw("settings", "storage")
			if err != nil || !found {
				t.Fatalf("initial storage = %s, %t, %v", before, found, err)
			}
			deep := filepath.Join(app.DataDir, subtree)
			if err := os.MkdirAll(deep, 0o755); err != nil {
				t.Fatal(err)
			}
			nestDirectories(t, deep, 2200)
			if size, err := measuredBytes(deep); size != 0 || !errors.Is(err, errStorageIncomplete) {
				t.Fatalf("incomplete bytes = %d, %v", size, err)
			}
			if err := app.measureStorage(cfg); !errors.Is(err, errStorageIncomplete) {
				t.Fatalf("incomplete observation = %v", err)
			}
			after, found, err := state.GetRaw("settings", "storage")
			if err != nil || !found || string(after) != string(before) {
				t.Fatalf("partial observation replaced the last complete snapshot: before=%s after=%s found=%t err=%v", before, after, found, err)
			}
		})
	}
}

func TestIncompleteRunnerStorageIsNotReportedAsMeasured(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	cfg := testConfig(t.TempDir())
	deep, healthy := t.TempDir(), t.TempDir()
	nestDirectories(t, deep, 2200)
	if err := os.WriteFile(filepath.Join(healthy, "transcript"), []byte("known"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.RunnerStoragePaths = map[string]string{"codex": deep, "opencode": healthy}
	if err := app.measureStorage(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get[storageUsage](state, "settings", "storage")
	if err != nil || saved == nil {
		t.Fatalf("storage = %+v, %v", saved, err)
	}
	runners := saved.RunnerTranscripts
	if entry := runners.Runners["codex"]; entry.Status != "error" || entry.Bytes != nil {
		t.Fatalf("incomplete runner reported measured: %+v", entry)
	}
	if entry := runners.Runners["opencode"]; entry.Status != "measured" || entry.Bytes == nil || *entry.Bytes != 5 {
		t.Fatalf("healthy runner lost its measurement: %+v", entry)
	}
	if runners.Status != "partial" || runners.Bytes == nil || *runners.Bytes != 5 {
		t.Fatalf("aggregate runner storage = %+v", runners)
	}
}

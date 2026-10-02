package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestReadOnlyExportsResolveDataDirectoryBeforeParentSegments(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	target := filepath.Join(root, "target", "state")
	decoy := filepath.Join(root, "state")
	for _, path := range []string{target, decoy, filepath.Join(root, "target", "child")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "target", "child"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	before := map[string][]byte{}
	for directory, id := range map[string]string{target: "target-cycle", decoy: "decoy-cycle"} {
		path := filepath.Join(directory, stateDBName)
		state, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		cycle := model.Cycle{
			ID: id, Number: 1, Status: model.CycleCompleted,
			StartedAt: "2026-09-12T00:00:00Z", Repository: "fixture/project",
			Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		}
		if err := state.Put("cycle", id, cycle); err != nil {
			state.Close()
			t.Fatal(err)
		}
		if err := state.Close(); err != nil {
			t.Fatal(err)
		}
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}

	// The OS resolves link/.. inside target, but filepath.Join would remove
	// those components first and silently select the decoy database instead.
	for _, directory := range []string{"link/../state", root + "/link/../state"} {
		for _, command := range [][]string{{"--usage-report"}, {"--export-run", "target-cycle"}} {
			t.Run(directory+" "+command[0], func(t *testing.T) {
				args := append([]string{"--data-dir", directory}, command...)
				var stdout, stderr bytes.Buffer
				if code := run(args, func(string) (string, bool) { return "", false }, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
					t.Fatalf("code=%d stderr=%q", code, stderr.String())
				}
				var exported struct {
					Cycles []struct{ ID string } `json:"cycles"`
					Cycle  struct{ ID string }   `json:"cycle"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &exported); err != nil {
					t.Fatal(err)
				}
				id := exported.Cycle.ID
				if command[0] == "--usage-report" {
					if len(exported.Cycles) != 1 {
						t.Fatalf("exported %d cycles, want one", len(exported.Cycles))
					}
					id = exported.Cycles[0].ID
				}
				if id != "target-cycle" {
					t.Fatalf("exported cycle %q from the wrong data directory", id)
				}
			})
		}
	}
	for path, original := range before {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, original) {
			t.Fatalf("read-only export changed %s: %v", path, err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "service.lock")); !os.IsNotExist(err) {
			t.Fatalf("read-only export created a service lock beside %s: %v", path, err)
		}
	}
}

func TestReadOnlyExportsDoNotCreateUnresolvedDataDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "target", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target", "child"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{root + "/link/../missing", filepath.Join(root, "dangling")} {
		for _, command := range [][]string{{"--usage-report"}, {"--export-run", "fixture-cycle"}} {
			var stdout, stderr bytes.Buffer
			args := append([]string{"--data-dir", directory}, command...)
			if code := run(args, func(string) (string, bool) { return "", false }, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !bytes.Contains(stderr.Bytes(), []byte("state database")) {
				t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
			}
		}
	}
	for _, directory := range []string{filepath.Join(root, "target", "missing"), filepath.Join(root, "missing"), filepath.Join(root, "missing-target")} {
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			t.Fatalf("read-only exports created %s: %v", directory, err)
		}
	}
}

package engine

// Verification commands: a fresh clone of exactly the reviewed revision, mutation is failed evidence, and the saved
// output is scrubbed across both streams even where the capture limit cut a secret in half.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// verificationFixture is a reviewing task whose workspace is a separate repository, with an App that runs no runner.
func verificationFixture(t *testing.T, commands []string) (*App, model.Task, string) {
	t.Helper()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = commands })
	ws := filepath.Join(f.root, "verify", "workspace")
	git(t, f.root, "init", "--initial-branch=main", "--separate-git-dir", filepath.Join(f.root, "verify", "repo.git"), ws)
	git(t, ws, "config", "user.name", "Fixture")
	git(t, ws, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(ws, "impl.txt"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "impl.txt")
	git(t, ws, "commit", "-m", "Fixture")
	revision := git(t, ws, "rev-parse", "HEAD")
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusReviewing
	task.Workspace = ws
	task.ComparisonBase = revision
	putTask(t, f, task)
	app := New(f.state, f.dataDir)
	cleanupApp(t, app)
	return app, task, revision
}

func TestVerificationMutationFails(t *testing.T) {
	t.Parallel()
	for _, commands := range [][]string{
		{"printf 1 > impl.txt", "test \"$(cat impl.txt)\" = 1", "git checkout -- impl.txt"},
		{"git -c user.name=x -c user.email=x@example.com commit --allow-empty -m moved", "true"},
	} {
		app, task, revision := verificationFixture(t, commands)
		_, err := app.verifyRevision(context.Background(), &task, revision)
		if err == nil || model.BlockedReasonFromError(err) != model.BlockedWorkspaceInvalid {
			t.Fatalf("commands %v: err = %v, want workspace_invalid", commands, err)
		}
		saved, getErr := store.Get[model.Task](app.Store, "task", task.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if len(saved.Verification) != 1 || saved.Verification[0].Success {
			t.Fatalf("commands %v: verification = %+v, want one failed record", commands, saved.Verification)
		}
		if record := saved.Verification[0]; record.Command != commands[0] || !strings.Contains(record.Output, "changed during this verification command") {
			t.Fatalf("commands %v: record = %+v; want the mutating command with mutation evidence", commands, record)
		}
	}
}

func TestVerificationUsesReviewedCommit(t *testing.T) {
	t.Parallel()
	app, task, revision := verificationFixture(t, []string{
		"test \"$(cat impl.txt)\" = 0", "test ! -e planted.txt", "test ! -d node_modules", "echo built > coverage.out"})
	if err := os.WriteFile(filepath.Join(filepath.Dir(task.Workspace), "repo.git", "info", "exclude"), []byte("planted.txt\nnode_modules/\ncoverage.out\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"planted.txt": "left by a session\n", "node_modules/fake/index.js": "exit(0)\n"} {
		path := filepath.Join(task.Workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	failures, err := app.verifyRevision(context.Background(), &task, revision)
	if err != nil || len(failures) != 0 {
		t.Fatalf("verification = %v, %v; ignored files in the task work tree must not reach the checkout", failures, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(task.Workspace), verificationDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification checkout was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(task.Workspace, "planted.txt")); err != nil {
		t.Fatal("the task work tree itself must be left as it was")
	}
}

const crossStreamToken = "opaque-verification-cross-stream-token"

const crossStreamCommand = `printf 'Authorization: Bearer '; printf '%s%s\nDIAGNOSTIC-END\n' 'opaque-verification-' 'cross-stream-token' >&2; exit 3`

// captureRedactionChild re-runs the calling test in a child process whose environment carries a synthetic secret,
// installed before the scrubber's first call and independent of parallel engine tests.
func captureRedactionChild(t *testing.T, env ...string) bool {
	t.Helper()
	const child = "OCTOMUS_CAPTURE_REDACTION_CHILD"
	if os.Getenv(child) != t.Name() {
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
		cmd.Env = append(os.Environ(), child+"="+t.Name(), "CAPTURE_TEST_API_KEY=s3cr3tValue-0123456789")
		cmd.Env = append(cmd.Env, env...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("saved-evidence regression: %v\n%s", err, output)
		}
		return false
	}
	return true
}

func TestEvidenceRedactsAdjacentSecrets(t *testing.T) {
	t.Parallel()
	if !captureRedactionChild(t) {
		return
	}
	const emit = `printf 's3cr3tValue-'; printf '0123456789\nDIAGNOSTIC-END\n' >&2; exit 3`
	checkOutput := func(t *testing.T, output string, diagnostic bool) {
		t.Helper()
		if strings.Contains(output, "s3cr3tValue-") || strings.Contains(output, "0123456789") {
			t.Fatalf("saved evidence retained an adjacent secret fragment: %q", output)
		}
		for _, want := range []string{"[redacted]", "[stderr]", "exit status: 3"} {
			if !strings.Contains(output, want) {
				t.Errorf("saved evidence lost %q", want)
			}
		}
		if strings.Contains(output, "DIAGNOSTIC-END") != diagnostic {
			t.Error("saved evidence changed its retained-fragment display policy")
		}
	}
	for _, boundary := range []struct {
		name, emit   string
		baselineTail bool
	}{
		{"whole", emit, true},
		{"stderr head cut", `printf 's3cr3tValue-'; { printf '0123456789'; head -c 400000 /dev/zero | tr '\000' x; printf '\nDIAGNOSTIC-END\n'; } >&2; exit 3`, false},
		{"stdout tail cut", `{ head -c 400000 /dev/zero | tr '\000' x; printf 's3cr3tValue-'; }; printf '0123456789\nDIAGNOSTIC-END\n' >&2; exit 3`, true},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct{ name, before string }{
				{"short", "printf 'STDOUT-HEAD\\n'; "},
				{"display limited", "seq -f 'progress line %g' 2000; "},
				{"capture limited", "seq -f 'progress line %g' 40000; "},
			} {
				t.Run("verification/"+tc.name, func(t *testing.T) {
					t.Parallel()
					app, task, revision := verificationFixture(t, []string{tc.before + boundary.emit})
					if _, err := app.verifyRevision(context.Background(), &task, revision); err != nil {
						t.Fatal(err)
					}
					saved := loadTask(t, app.Store, task.ID)
					if len(saved.Verification) != 1 || saved.Verification[0].Success {
						t.Fatal("expected saved verification failure")
					}
					checkOutput(t, saved.Verification[0].Output, true)
				})
			}
			t.Run("baseline", func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				app := New(f.state, f.dataDir)
				cleanupApp(t, app)
				check := makeCheck(f.cfg, model.BaselineStatusRunning)
				check.Config.VerificationCommands = []string{"printf 'STDOUT-HEAD\\n'; " + boundary.emit}
				if status, err := app.executeBaseline(context.Background(), &check); err != nil || status != model.BaselineStatusFailed {
					t.Fatalf("baseline = %v, %v", status, err)
				}
				saved, err := store.Get[model.BaselineCheck](app.Store, "baseline", check.ID)
				if err != nil || saved == nil || len(saved.Commands) != 1 || saved.Commands[0].Success {
					t.Fatalf("expected saved baseline failure: %+v, %v", saved, err)
				}
				checkOutput(t, saved.Commands[0].Output, boundary.baselineTail)
			})
		})
	}
}

func TestEvidenceRedactsCrossStream(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before string }{
		{"short", "printf 'STDOUT-HEAD\\n'; "},
		{"display limited", "seq -f 'progress line %g' 2000; "},
		{"capture limited", "seq -f 'progress line %g' 40000; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			command := tc.before + crossStreamCommand
			app, task, revision := verificationFixture(t, []string{command})
			failures, err := app.verifyRevision(context.Background(), &task, revision)
			if err != nil {
				t.Fatal(err)
			}
			saved := loadTask(t, app.Store, task.ID)
			if len(saved.Verification) != 1 {
				t.Fatalf("verification records = %d; want one", len(saved.Verification))
			}
			result := saved.Verification[0]
			if result.Success || result.Revision != revision || !utf8.ValidString(result.Output) || len(result.Output) > outputLimit {
				t.Fatalf("incorrect verification result: %+v", result)
			}
			if strings.Contains(result.Output, crossStreamToken) {
				t.Fatalf("saved verification exposed the cross-stream credential: %q", result.Output)
			}
			for _, want := range []string{"Authorization: [redacted]", "\n[stderr]\nDIAGNOSTIC-END", "exit status: 3"} {
				if !strings.Contains(result.Output, want) {
					t.Errorf("saved verification lost %q", want)
				}
			}
			if len(failures) != 1 || failures[0] != command+": "+result.Output || strings.Contains(failures[0], crossStreamToken) {
				t.Fatal("repair evidence must use the saved, scrubbed output")
			}
		})
	}
}

package engine

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const crossStreamToken = "opaque-verification-cross-stream-token"

const crossStreamCommand = `printf 'Authorization: Bearer '; printf '%s%s\nDIAGNOSTIC-END\n' 'opaque-verification-' 'cross-stream-token' >&2; exit 3`

func captureRedactionChild(t *testing.T, env ...string) bool {
	t.Helper()
	// Run with the existing process package's synthetic secret installed before
	// the first scrubber call, independently of parallel engine tests.
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

func TestSavedCommandEvidenceRedactsAdjacentStreamSecrets(t *testing.T) {
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
			for _, tc := range []struct{ name, before string }{
				{"short", "printf 'STDOUT-HEAD\\n'; "},
				{"display limited", "seq -f 'progress line %g' 2000; "},
				{"capture limited", "seq -f 'progress line %g' 40000; "},
			} {
				t.Run("verification/"+tc.name, func(t *testing.T) {
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
				fixture := newPlanningFixture(t)
				app := New(fixture.state, fixture.dataDir)
				t.Cleanup(app.Shutdown)
				check := makeCheck(fixture.cfg, model.BaselineStatusRunning)
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

func TestSavedVerificationRedactsCredentialsAcrossStreams(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before string }{
		{"short", "printf 'STDOUT-HEAD\\n'; "},
		{"display limited", "seq -f 'progress line %g' 2000; "},
		{"capture limited", "seq -f 'progress line %g' 40000; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if result.Success || result.Revision != revision || !utf8.ValidString(result.Output) || len(result.Output) > verificationOutputLimit {
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

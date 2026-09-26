package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func git(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}

func baselineApp(t *testing.T) (*App, config.Config) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit := exec.Command("git", "init", "-b", "main")
	gitInit.Dir = repo
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s", out)
	}
	gitCfg := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	gitCfg("config", "user.name", "Fixture")
	gitCfg("config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCfg("add", ".")
	gitCfg("commit", "-m", "initial")
	cfg := config.Default()
	cfg.Repository = repo
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"true"}
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	return New(state, data), cfg
}

func makeCheck(cfg config.Config, status model.BaselineStatus) model.BaselineCheck {
	return model.BaselineCheck{
		ID: model.ID(), Status: status, Config: cfg.Clone(),
		StartedAt: model.Now(), Commands: []model.BaselineCommand{},
	}
}

func TestBaselineValidationAcceptsUnroutedModelsButRequiresRepositoryAndCommands(t *testing.T) {
	_, cfg := baselineApp(t)
	if err := cfg.Validate(true); err == nil {
		t.Fatal("unrouted models must fail full validation")
	}
	if err := cfg.ValidateBaseline(); err != nil {
		t.Fatalf("baseline validation: %v", err)
	}
	noCommands := cfg.Clone()
	noCommands.VerificationCommands = []string{}
	if err := noCommands.ValidateBaseline(); err == nil {
		t.Fatal("empty verification commands must fail")
	}
	noRepo := cfg.Clone()
	noRepo.Repository = "relative/path"
	if err := noRepo.ValidateBaseline(); err == nil {
		t.Fatal("relative repository must fail")
	}
	noGitHub := cfg.Clone()
	noGitHub.GitHubRepo = "not-an-owner/name/pair"
	if err := noGitHub.ValidateBaseline(); err == nil {
		t.Fatal("malformed github_repo must fail")
	}
	if err := cfg.ValidateAudit(); err == nil {
		t.Fatal("unrouted models must fail audit validation")
	}
}

// TestStartBaselineRejectsStaleRevisionBeforeWork verifies admission compares
// the caller's expected revision with the live canonical fingerprint before a
// check record, clone directory or worker exists.
func TestStartBaselineRejectsStaleRevisionBeforeWork(t *testing.T) {
	app, cfg := baselineApp(t)
	fingerprint, err := BaselineFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("0", len(fingerprint))
	if _, err := app.StartBaseline(stale); err == nil {
		t.Fatal("stale revision accepted")
	} else {
		var conflict *BaselineConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("conflict kind: %v", err)
		}
	}
	if running, err := app.Store.RunningBaselines(); err != nil || len(running) != 0 {
		t.Fatalf("rejected start persisted work: %d %v", len(running), err)
	}
	if latest, err := app.Store.LatestBaseline(); err != nil || latest != nil {
		t.Fatal("stale start created a baseline record")
	}
	if entries, err := os.ReadDir(filepath.Join(app.DataDir, "baselines")); err == nil && len(entries) != 0 {
		t.Fatalf("clone directory created: %v", entries)
	}
}

func TestBaselineFingerprintTracksTheCanonicalConfig(t *testing.T) {
	_, cfg := baselineApp(t)
	fingerprint, err := BaselineFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(fingerprint) != 64 {
		t.Fatalf("fingerprint length %d", len(fingerprint))
	}
	again, _ := BaselineFingerprint(cfg)
	if again != fingerprint {
		t.Fatal("fingerprint is not stable")
	}
	changed := cfg.Clone()
	changed.VerificationCommands = append(changed.VerificationCommands, "echo ok")
	other, _ := BaselineFingerprint(changed)
	if other == fingerprint {
		t.Fatal("fingerprint must track configuration changes")
	}
}

func TestBaselineOutputBoundsAreUTF8Safe(t *testing.T) {
	const marker = "\n[output truncated]"
	if output, truncated := boundedOutput("short", 16*1024, false); truncated || output != "short" {
		t.Fatalf("short output: %q %v", output, truncated)
	}
	if output, truncated := boundedOutput("anything", 0, false); !truncated || output != "" {
		t.Fatalf("zero limit: %q %v", output, truncated)
	}
	if output, truncated := boundedOutput("", 0, false); truncated || output != "" {
		t.Fatalf("empty at zero limit: %q %v", output, truncated)
	}
	if output, truncated := boundedOutput("anything", 0, true); !truncated || output != "" {
		t.Fatalf("diagnostic at zero limit: %q %v", output, truncated)
	}
	if output, truncated := boundedOutput(strings.Repeat("x", 100), 1, false); !truncated || len(output) != 1 {
		t.Fatalf("limit one: %q %v", output, truncated)
	}
	for _, limit := range []int{len(marker) - 1, len(marker)} {
		if output, truncated := boundedOutput(strings.Repeat("x", 100*1024), limit, false); !truncated || len(output) > limit {
			t.Fatalf("limit %d: %d %v", limit, len(output), truncated)
		}
	}
	long := strings.Repeat("x", 20*1024)
	if output, truncated := boundedOutput(long, 16*1024, false); !truncated || len(output) > 16*1024 || !strings.HasSuffix(output, "[output truncated]") {
		t.Fatalf("16KiB bound: %d %v", len(output), truncated)
	}
	wide := strings.Repeat("𐐀", 16*1024)
	if output, truncated := boundedOutput(wide, 16*1024, false); !truncated || len(output) > 16*1024 {
		t.Fatalf("utf8 bound: %d %v", len(output), truncated)
	}
	if output, truncated := boundedOutput("small", 16*1024, true); !truncated || !strings.HasSuffix(output, "[output truncated]") {
		t.Fatalf("diagnostic flag: %q %v", output, truncated)
	}
	remaining := 1024 * 1024
	total := 0
	for i := 0; i < 100; i++ {
		limit := remaining
		if limit > 16*1024 {
			limit = 16 * 1024
		}
		output, truncated := boundedOutput(long, limit, false)
		if !truncated && output != "" {
			t.Fatalf("aggregate round %d: %q", i, output[:min(40, len(output))])
		}
		if len(output) > limit {
			t.Fatalf("aggregate round %d over limit", i)
		}
		remaining -= len(output)
		if remaining < 0 {
			remaining = 0
		}
		total += len(output)
	}
	if total > 1024*1024 || remaining != 0 {
		t.Fatalf("aggregate: total %d remaining %d", total, remaining)
	}
}

func TestBaselineCommandOutputPreservesRealCaptureTruncation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	captured, captureErr := process.Capture(ctx, "bash", []string{"-c", "yes '𐐀' | head -c 20000"}, dir, 10, process.CaptureDiagnostic)
	text, diagnosticTruncated, success := commandOutput(captured, captureErr)
	if !success || diagnosticTruncated {
		t.Fatalf("success=%v diagnostic=%v", success, diagnosticTruncated)
	}
	if output, truncated := boundedOutput(store.Redact(text), 16*1024, diagnosticTruncated); !truncated || len(output) > 16*1024 {
		t.Fatalf("multibyte bound: %d %v", len(output), truncated)
	}
	captured, captureErr = process.Capture(ctx, "bash", []string{"-c", "yes 'x' | head -c 300000; exit 3"}, dir, 10, process.CaptureDiagnostic)
	text, diagnosticTruncated, success = commandOutput(captured, captureErr)
	if success || !diagnosticTruncated || !strings.Contains(text, "exit status: 3") {
		t.Fatalf("failed capture: %v %v %.60s", success, diagnosticTruncated, text)
	}
	if output, truncated := boundedOutput(store.Redact(text), 16*1024, diagnosticTruncated); !truncated || len(output) > 16*1024 {
		t.Fatalf("failure bound: %d %v", len(output), truncated)
	}
	captured, captureErr = process.Capture(ctx, "bash", []string{"-c", "echo out; echo err >&2; exit 1"}, dir, 10, process.CaptureDiagnostic)
	text, diagnosticTruncated, success = commandOutput(captured, captureErr)
	if success || diagnosticTruncated {
		t.Fatalf("stderr capture: %v %v", success, diagnosticTruncated)
	}
	if !strings.Contains(text, "out") || !strings.Contains(text, "[stderr]") || !strings.Contains(text, "err") {
		t.Fatalf("stderr shape: %.80s", text)
	}
}

func TestBaselineOutputFlagsShorteningBelowTheCaptureLimit(t *testing.T) {
	_, cfg := baselineApp(t)
	ctx := context.Background()
	revision := git(t, cfg.Repository, "rev-parse", "HEAD")
	// 20 KiB of ASCII output plus a failure status line: above the 16 KiB
	// per-command cap but far below capture's diagnostic limit. The persisted
	// evidence must flag the shortened tail, not record it as complete.
	outcome := runCheckCommand(ctx, cfg, cfg.Repository, "yes x | head -c 20480; exit 3", revision)
	text, diagnosticTruncated, success := commandOutput(outcome.captured, outcome.capture)
	if success || diagnosticTruncated {
		t.Fatalf("outcome: %v %v", success, diagnosticTruncated)
	}
	if !strings.Contains(text, "exit status: 3") {
		t.Fatalf("status line missing: %.60s", text)
	}
	// The exact composition executeBaseline applies per command: the 16 KiB
	// per-command bound inside the 1 MiB aggregate budget.
	output, truncated := boundedOutput(store.RedactSecrets(text), 16*1024, diagnosticTruncated)
	if !truncated {
		t.Fatal("shortened output must be flagged")
	}
	if !strings.HasSuffix(output, "[output truncated]") || len(output) > 16*1024 {
		t.Fatalf("bounded: %d %q", len(output), output[len(output)-30:])
	}
	// Secret scrubbing still applies within the kept bytes.
	redacted, _ := boundedOutput(store.RedactSecrets("token ghp_abcdefghijklmnop"), 16*1024, false)
	if !strings.Contains(redacted, "[redacted]") || strings.Contains(redacted, "ghp_") {
		t.Fatalf("redaction: %q", redacted)
	}
}

func TestBaselineObservationNeverRegressesToAnOlderRevision(t *testing.T) {
	app, cfg := baselineApp(t)
	older := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	newer := time.Now().UTC().Format(time.RFC3339)
	if err := app.observeDefaultBranch(cfg, strings.Repeat("a", 40), newer); err != nil {
		t.Fatal(err)
	}
	if err := app.observeDefaultBranch(cfg, strings.Repeat("b", 40), older); err != nil {
		t.Fatal(err)
	}
	app.runtimeMu.Lock()
	observation := *app.runtime.defaultObservation
	app.runtimeMu.Unlock()
	if observation.Revision != strings.Repeat("a", 40) {
		t.Fatalf("regressed to %s", observation.Revision)
	}
	other := cfg.Clone()
	other.DefaultBranch = "other"
	if err := app.observeDefaultBranch(other, strings.Repeat("c", 40), newer); err == nil {
		t.Fatal("a changed remote identity must reject the observation")
	}
	app.runtimeMu.Lock()
	observation = *app.runtime.defaultObservation
	app.runtimeMu.Unlock()
	if observation.Revision != strings.Repeat("a", 40) {
		t.Fatalf("rejected observation overwrote: %s", observation.Revision)
	}
}

func setObservation(app *App, observation *model.DefaultBranchObservation) {
	app.runtimeMu.Lock()
	app.runtime.defaultObservation = observation
	app.runtimeMu.Unlock()
}

func TestBaselineViewReportsConfigMatchAndRevisionStalenessSeparately(t *testing.T) {
	app, cfg := baselineApp(t)
	fingerprint, err := BaselineFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	completed := model.Now()
	check := makeCheck(cfg, model.BaselineStatusPassed)
	check.ConfigFingerprint = fingerprint
	check.Revision = &revision
	check.CompletedAt = &completed
	check.WorkspaceRemoved = true
	if err := app.Store.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.Put("settings", "baseline_latest", check.ID); err != nil {
		t.Fatal(err)
	}
	view, err := app.BaselineView(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := view["check"].(*model.BaselineCheck); !ok || got.ID != check.ID {
		t.Fatalf("view check: %v", view["check"])
	}
	if view["config_matches"] != true || view["revision_status"] != "unknown" {
		t.Fatalf("view: %v", view)
	}
	if obs, _ := view["default_observation"].(*model.DefaultBranchObservation); obs != nil {
		t.Fatalf("observation should be empty: %v", obs)
	}
	observation := &model.DefaultBranchObservation{
		Repository: cfg.GitHubRepo, DefaultBranch: cfg.DefaultBranch,
		Revision: revision, ObservedAt: model.Now(),
	}
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "matches_last_observation" {
		t.Fatalf("fresh match: %v", status["revision_status"])
	}
	observation.Revision = strings.Repeat("b", 40)
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "stale" {
		t.Fatalf("stale: %v", status["revision_status"])
	}
	observation.ObservedAt = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "unknown" {
		t.Fatalf("future observation: %v", status["revision_status"])
	}
	observation.ObservedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "unknown" {
		t.Fatalf("expired observation: %v", status["revision_status"])
	}
	// An observation older than one housekeeping interval stays comparable
	// until the next, possibly slower, pass has had time to replace it.
	observation.Revision = revision
	observation.ObservedAt = time.Now().UTC().Add(-(observeInterval + time.Minute)).Format(time.RFC3339)
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "matches_last_observation" {
		t.Fatalf("observation within its lifetime: %v", status["revision_status"])
	}
	observation.ObservedAt = time.Now().UTC().Add(-(observationLifetime + time.Minute)).Format(time.RFC3339)
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "unknown" {
		t.Fatalf("observation past its lifetime: %v", status["revision_status"])
	}
	observation.Revision = strings.Repeat("b", 40)
	observation.ObservedAt = model.Now()
	observation.DefaultBranch = "other"
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "unknown" {
		t.Fatalf("other branch: %v", status["revision_status"])
	}
	observation.DefaultBranch = cfg.DefaultBranch
	setObservation(app, observation)
	if status, _ := app.BaselineView(nil); status["revision_status"] != "stale" {
		t.Fatalf("same target stale: %v", status["revision_status"])
	}
	changed := cfg.Clone()
	changed.DefaultBranch = "moved"
	if err := app.Store.Put("settings", "config", changed); err != nil {
		t.Fatal(err)
	}
	view, err = app.BaselineView(nil)
	if err != nil {
		t.Fatal(err)
	}
	if view["config_matches"] != false || view["revision_status"] != "unknown" {
		t.Fatalf("changed config: %v %v", view["config_matches"], view["revision_status"])
	}
	noCommands := changed.Clone()
	noCommands.VerificationCommands = []string{}
	if err := app.Store.Put("settings", "config", noCommands); err != nil {
		t.Fatal(err)
	}
	view, err = app.BaselineView(nil)
	if err != nil {
		t.Fatal(err)
	}
	if view["eligible"] != false {
		t.Fatal("invalid config must report ineligible")
	}
	reason, _ := view["reason"].(string)
	if !strings.Contains(reason, "verification") {
		t.Fatalf("reason %q must name the verification problem", reason)
	}
}

func TestBaselineCleanupRemovesTheOwnedCloneAndRefusesSymlinks(t *testing.T) {
	app, cfg := baselineApp(t)
	check := makeCheck(cfg, model.BaselineStatusFailed)
	completed := model.Now()
	check.CompletedAt = &completed
	// Production only ever cleans a persisted record; the cleanup finalization
	// re-reads it, so the check must exist in the store first.
	if err := app.Store.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}
	workspaceDir := filepath.Join(app.DataDir, "baselines", check.ID, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "artifact"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.CleanupBaseline(&check); err != nil {
		t.Fatal(err)
	}
	if !check.WorkspaceRemoved || check.CleanupError != nil {
		t.Fatalf("cleanup: removed=%v error=%v", check.WorkspaceRemoved, check.CleanupError)
	}
	if _, err := os.Stat(filepath.Join(app.DataDir, "baselines", check.ID)); !os.IsNotExist(err) {
		t.Fatal("owned clone still exists")
	}
	saved, err := store.Get[model.BaselineCheck](app.Store, "baseline", check.ID)
	if err != nil || saved == nil || !saved.WorkspaceRemoved {
		t.Fatalf("saved cleanup: %v %v", saved, err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := makeCheck(cfg, model.BaselineStatusFailed)
	bad.CompletedAt = &completed
	if err := app.Store.Put("baseline", bad.ID, bad); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(app.DataDir, "baselines", bad.ID)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := app.CleanupBaseline(&bad); err != nil {
		t.Fatal(err)
	}
	if bad.WorkspaceRemoved || bad.CleanupError == nil {
		t.Fatalf("symlink cleanup: removed=%v error=%v", bad.WorkspaceRemoved, bad.CleanupError)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("symlink cleanup deleted the outside directory")
	}
	invalid := makeCheck(cfg, model.BaselineStatusFailed)
	invalid.ID = "../etc"
	invalid.CompletedAt = &completed
	if err := app.CleanupBaseline(&invalid); err == nil {
		t.Fatal("invalid identity must refuse cleanup")
	}
}

func TestRecoverBaselinesFinalizesRunningRecordsAndPreservesCancelIntent(t *testing.T) {
	app, cfg := baselineApp(t)
	interrupted := makeCheck(cfg, model.BaselineStatusRunning)
	cancelled := makeCheck(cfg, model.BaselineStatusRunning)
	finished := makeCheck(cfg, model.BaselineStatusPassed)
	for _, check := range []model.BaselineCheck{interrupted, cancelled, finished} {
		if err := app.Store.Put("baseline", check.ID, check); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Store.Put("baseline_cancel", cancelled.ID, model.Now()); err != nil {
		t.Fatal(err)
	}
	if err := app.recoverBaselines(); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get[model.BaselineCheck](app.Store, "baseline", interrupted.ID)
	if err != nil || got == nil {
		t.Fatal("interrupted check missing")
	}
	if got.Status != model.BaselineStatusInterrupted || got.CompletedAt == nil || got.Error == nil {
		t.Fatalf("interrupted: %+v", got)
	}
	got, err = store.Get[model.BaselineCheck](app.Store, "baseline", cancelled.ID)
	if err != nil || got == nil || got.Status != model.BaselineStatusCancelled {
		t.Fatalf("cancelled: %+v", got)
	}
	got, err = store.Get[model.BaselineCheck](app.Store, "baseline", finished.ID)
	if err != nil || got == nil || got.Status != model.BaselineStatusPassed {
		t.Fatalf("finished: %+v", got)
	}
	candidates, err := app.Store.BaselineCleanupCandidates()
	if err != nil || len(candidates) != 3 {
		t.Fatalf("cleanup candidates: %d %v", len(candidates), err)
	}
	running, err := app.Store.RunningBaselines()
	if err != nil || len(running) != 0 {
		t.Fatalf("running after recovery: %d", len(running))
	}
}

package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func baselineApp(t *testing.T) (*App, config.Config) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Fixture")
	git(t, repo, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	cfg := config.Default()
	cfg.Repository = repo
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"true"}
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	state := openStore(t, data)
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
	t.Parallel()
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

func TestStartBaselineRejectsStaleRevisionBeforeWork(t *testing.T) {
	t.Parallel()
	app, cfg := baselineApp(t)
	fingerprint, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("0", len(fingerprint))
	if _, err := app.StartBaseline(stale); err == nil {
		t.Fatal("stale revision accepted")
	} else if !IsActionConflict(err) {
		t.Fatalf("conflict kind: %v", err)
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

func setObservation(app *App, observation *model.DefaultBranchObservation) {
	app.runtimeMu.Lock()
	app.runtime.defaultObservation = observation
	app.runtimeMu.Unlock()
}

func TestBaselineViewReportsConfigMatchAndRevisionStalenessSeparately(t *testing.T) {
	t.Parallel()
	app, cfg := baselineApp(t)
	fingerprint, err := cfg.Fingerprint()
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
	view, err = app.BaselineView(nil)
	if err != nil {
		t.Fatal(err)
	}
	if view["revision_status"] != "stale" {
		t.Fatalf("same target stale: %v", view["revision_status"])
	}
	if obs, _ := view["default_observation"].(*model.DefaultBranchObservation); obs == nil || obs.Revision != observation.Revision {
		t.Fatalf("observation of the live target was not shown: %v", view["default_observation"])
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
	if obs, _ := view["default_observation"].(*model.DefaultBranchObservation); obs != nil {
		t.Fatalf("observation of another target was shown as the live one: %v", obs)
	}
	if encoded, err := json.Marshal(view["default_observation"]); err != nil || string(encoded) != "null" {
		t.Fatalf("hidden observation must encode as null: %s, %v", encoded, err)
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

package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func (a *App) StateView() (map[string]any, error) {
	control, err := a.Control()
	if err != nil {
		return nil, err
	}
	snapshot, err := a.Store.Dashboard()
	if err != nil {
		return nil, err
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	planningCapacity, err := a.Store.PlanningCapacity(time.Now())
	if err != nil {
		return nil, err
	}
	prCapacity, err := a.prCapacity(cfg)
	if err != nil {
		return nil, err
	}
	latest, err := a.Store.LatestBaseline()
	if err != nil {
		return nil, err
	}
	notifications, err := a.Store.NotificationHealth()
	if err != nil {
		return nil, err
	}
	a.runtimeMu.Lock()
	var cycleMode *model.CycleMode
	if a.runtime.cycle != nil {
		cycleMode = new(a.runtime.cycle.mode)
	} else if a.runtime.preflight != nil {
		cycleMode = new(*a.runtime.preflight)
	}
	activeTasks := len(a.runtime.tasks)
	cycleActive := a.runtime.cycle != nil
	baselineActive := a.runtime.baseline != nil
	recoveryError := a.runtime.activeRecoveryError
	a.runtimeMu.Unlock()
	if !cycleActive {
		running, err := a.Store.RunningCycles("")
		if err != nil {
			return nil, err
		}
		if len(running) > 0 {
			cycleActive = true
			if cycleMode == nil {
				cycleMode = new(running[0].Mode)
			}
		}
	}
	status := "idle"
	switch {
	case recoveryError != nil:
		status = "unhealthy"
	case cycleMode != nil && *cycleMode == model.CycleModeAudit:
		status = "auditing"
	case control.Paused:
		status = "paused"
	case control.Error != nil:
		status = "unhealthy"
	case cycleActive || activeTasks > 0:
		status = "running"
	}
	view, err := wirejson.GenericMap(snapshot)
	if err != nil {
		return nil, err
	}
	controlJSON, err := wirejson.GenericMap(control)
	if err != nil {
		return nil, err
	}
	var baselineView any
	if latest != nil {
		summary := a.baselineSummary(latest, cfg)
		maps.Copy(summary, map[string]any{
			"id":           latest.ID,
			"status":       latest.Status.String(),
			"started_at":   latest.StartedAt,
			"completed_at": latest.CompletedAt,
			"error":        latest.Error,
		})
		baselineView = summary
	}
	storage, err := store.Get[any](a.Store, "settings", "storage")
	if err != nil {
		return nil, err
	}
	maps.Copy(view, map[string]any{
		"status":            status,
		"recovery_error":    recoveryError,
		"control":           controlJSON,
		"repository":        cfg.GitHubRepo,
		"configured":        cfg.Validate(true) == nil,
		"audit_configured":  cfg.ValidateAudit() == nil,
		"active_cycle_mode": cycleMode,
		"active_tasks":      activeTasks,
		"cycle_active":      cycleActive,
		"baseline_active":   baselineActive,
		"baseline":          baselineView,
		"notifications":     notifications,
		"session_limit":     cfg.MaxSessionsPerDay,
		"storage_limit":     cfg.MaxWorkspaceBytes,
		"storage":           storage,
		"planning_capacity": planningCapacity,
		"pr_capacity":       prCapacity,
		"sandbox":           a.SandboxPosture(),
	})
	return view, nil
}

// baselineSummary is what the state and baseline views both report about a check against the live configuration.
func (a *App) baselineSummary(check *model.BaselineCheck, live config.Config) map[string]any {
	return map[string]any{
		"config_revision": check.ConfigFingerprint,
		"config_matches":  baselineConfigMatches(check, live),
		"revision_status": a.baselineRevisionStatus(check, live),
	}
}

const baselineCaveat = "A baseline check verifies the saved commands on a clone made at its start time; it does not prove later host, tool or remote health and is not publication evidence."

func (a *App) BaselineView(id *string) (map[string]any, error) {
	var check *model.BaselineCheck
	var err error
	if id != nil {
		check, err = store.Get[model.BaselineCheck](a.Store, "baseline", *id)
	} else {
		check, err = a.Store.LatestBaseline()
	}
	if err != nil {
		return nil, err
	}
	reason, err := a.baselineBlocker()
	if err != nil {
		return nil, err
	}
	live, err := a.Config()
	if err != nil {
		return nil, err
	}
	if reason == nil {
		if err := live.ValidateBaseline(); err != nil {
			reason = new(redact.Error(err))
		}
	}
	a.runtimeMu.Lock()
	observation := a.runtime.defaultObservation
	a.runtimeMu.Unlock()
	if observation != nil && (observation.Revision == "" || !observation.Describes(live)) {
		observation = nil
	}
	var reasonValue any
	if reason != nil {
		reasonValue = *reason
	}
	view := map[string]any{
		"check":               check,
		"eligible":            reason == nil,
		"reason":              reasonValue,
		"config_matches":      nil,
		"config_revision":     nil,
		"revision_status":     "unknown",
		"default_observation": observation,
		"caveat":              baselineCaveat,
	}
	if check != nil {
		maps.Copy(view, a.baselineSummary(check, live))
	}
	return view, nil
}

func (a *App) Doctor(cfg config.Config, mode model.CycleMode) (map[string]any, []string, error) {
	if err := a.admitDiagnostic(); err != nil {
		return nil, nil, err
	}
	defer a.wg.Done()
	// The containment self-test needs no configuration, so it runs first: a broken sandbox is reported even when the
	// configuration or repository check fails too.
	errs := []string{}
	sandboxResult := map[string]any{"mode": a.sandbox.Mode().String()}
	if a.sandbox.Mode() == sandbox.ModeDocker {
		selfTest, err := a.SelfTest(a.ctx)
		if err != nil {
			return nil, nil, err
		}
		if failure := selfTest.failure(); failure != nil {
			errs = append(errs, failure.Error())
		}
		sandboxResult["self_test"] = selfTest
	}
	withSelfTest := func(err error) error {
		if len(errs) == 0 {
			return err
		}
		return fmt.Errorf("%s; %w", strings.Join(errs, "; "), err)
	}
	if mode == model.CycleModeAudit {
		if err := cfg.ValidateAudit(); err != nil {
			return nil, nil, withSelfTest(err)
		}
	} else {
		if err := cfg.Validate(true); err != nil {
			return nil, nil, withSelfTest(err)
		}
	}
	if err := gitops.ValidateRemote(a.ctx, cfg); err != nil {
		return nil, nil, withSelfTest(err)
	}
	routes := cfg.RoutesFor(mode == model.CycleModeAudit)
	seen := map[config.Backend]bool{}
	backends := []config.Backend{}
	for _, named := range routes {
		if !seen[named.Route.Backend] {
			seen[named.Route.Backend] = true
			backends = append(backends, named.Route.Backend)
		}
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i] < backends[j] })
	diagnostics := []runner.Diagnostics{}
	models := []runner.Model{}
	warnings := []string{}
	for _, backend := range backends {
		checkErr := a.withScratchRunner(backend, cfg, func(client runner.Adapter, scratch string) error {
			diagnostic, err := client.Diagnose(scratch)
			if err != nil {
				return err
			}
			if diagnostic.Warning != nil && *diagnostic.Warning != "" {
				warnings = append(warnings, *diagnostic.Warning)
			}
			catalog, err := client.Models(scratch)
			if err != nil {
				return err
			}
			for _, named := range routes {
				if named.Route.Backend != backend {
					continue
				}
				if err := runner.ValidateRoute(named.Route, catalog); err != nil {
					errs = append(errs, named.Name+": "+err.Error())
				}
			}
			models = append(models, catalog...)
			diagnostics = append(diagnostics, diagnostic)
			return nil
		})
		if checkErr != nil {
			errs = append(errs, backend.Display()+": "+checkErr.Error())
		}
	}
	if len(errs) > 0 {
		return nil, warnings, errors.New(strings.Join(errs, "; "))
	}
	modeName := "all"
	if mode == model.CycleModeAudit {
		modeName = "planning"
	}
	message := "Repository, GitHub authentication, and " + modeName + " model routes are available."
	if len(warnings) > 0 {
		message += " Warning: " + strings.Join(warnings, " ")
	}
	result := map[string]any{
		"ok": true, "mode": mode, "models": models, "backends": diagnostics,
		"warnings": warnings, "message": message, "sandbox": sandboxResult,
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Backend == config.BackendCodex {
			result["codex_version"] = diagnostic.Version
			result["tested_codex_version"] = runner.CodexVersion
			break
		}
	}
	return result, warnings, nil
}

func (a *App) ModelCatalog(backend config.Backend, binary string) ([]runner.Model, error) {
	if err := a.admitDiagnostic(); err != nil {
		return nil, err
	}
	defer a.wg.Done()
	if err := config.ValidateBinary(binary); err != nil {
		return nil, err
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	switch backend {
	case config.BackendCodex:
		cfg.CodexBinary = binary
	case config.BackendOpencode:
		cfg.OpencodeBinary = binary
	default:
		return nil, errors.New("Invalid backend")
	}
	var catalog []runner.Model
	err = a.withScratchRunner(backend, cfg, func(client runner.Adapter, scratch string) error {
		var err error
		catalog, err = client.Models(scratch)
		return err
	})
	return catalog, err
}

// withScratchRunner connects one runner from an empty owned root, hands it to fn and joins its cleanup, so a
// sandboxed runner started for a diagnostic can see no repository, workspace or state.
func (a *App) withScratchRunner(backend config.Backend, cfg config.Config, fn func(client runner.Adapter, scratch string) error) (err error) {
	scratch, discard, err := a.scratchWorkspace()
	if err != nil {
		return err
	}
	defer discard()
	client, err := a.connect("system")(a.ctx, backend, cfg, scratch)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("Runner cleanup failed: %s", redact.Error(closeErr)))
		}
	}()
	return fn(client, scratch)
}

// Diagnostics own runner children and scratch roots even though they do not start
// model turns. Join their cleanup before shutdown can close the service store.
func (a *App) admitDiagnostic() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	if err := a.ctx.Err(); err != nil {
		return err
	}
	a.wg.Add(1)
	return nil
}

// SandboxPosture is what the dashboard shows about isolation: the mode, the broker's live report, the egress
// allowlists, the pinned repository and the latest self-test.
type SandboxPosture struct {
	Mode             string              `json:"mode"`
	Healthy          bool                `json:"healthy"`
	Error            *string             `json:"error"`
	Broker           *wire.BrokerInfo    `json:"broker"`
	Egress           map[string][]string `json:"egress"`
	PinnedRepository *string             `json:"pinned_repository"`
	SelfTest         *SandboxSelfTest    `json:"self_test"`
}

func (a *App) SandboxPosture() SandboxPosture {
	posture := SandboxPosture{Mode: a.sandbox.Mode().String(), Healthy: a.sandbox.Mode() == sandbox.ModeDocker, Egress: a.deployment.Egress}
	if a.deployment.pinned() {
		posture.PinnedRepository = new(a.deployment.GitHubRepo)
	}
	if remote, ok := a.sandbox.(*sandbox.Remote); ok {
		ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
		info, err := remote.Info(ctx)
		cancel()
		if err != nil {
			posture.Healthy, posture.Error = false, new(redact.Error(err))
		} else {
			posture.Broker = &info
		}
	}
	if selfTest, err := store.Get[SandboxSelfTest](a.Store, "settings", selfTestRecord); err == nil && selfTest != nil {
		// A saved proof only counts while it names the broker's current image and runtime; anything else predates
		// a posture change and needs a fresh self-test.
		if posture.Broker != nil && selfTest.ImageID == posture.Broker.ImageID && selfTest.Runtime == posture.Broker.Runtime {
			posture.SelfTest = selfTest
		}
	}
	return posture
}

// SandboxSelfTest is the latest containment probe, kept for the dashboard.
type SandboxSelfTest struct {
	At      string               `json:"at"`
	Passed  bool                 `json:"passed"`
	Checks  []sandbox.ProbeCheck `json:"checks"`
	Kernel  string               `json:"kernel"`
	ImageID string               `json:"image_id"`
	Runtime string               `json:"runtime,omitempty"`
	Error   *string              `json:"error"`
}

const selfTestRecord = "sandbox_self_test"

// SelfTest runs the containment probe in a real sandbox and records what it observed. It proves the boundary
// rather than describing it: every check is made from inside.
func (a *App) SelfTest(ctx context.Context) (SandboxSelfTest, error) {
	record := SandboxSelfTest{At: model.Now(), Checks: []sandbox.ProbeCheck{}}
	if err := a.admitDiagnostic(); err != nil {
		return record, err
	}
	defer a.wg.Done()
	// A direct HTTP request owns its caller context, but the service still owns
	// the child and must cancel and join it before shutdown closes the store.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	defer stop()
	defer cancel()
	cancellation := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// AfterFunc propagates service cancellation asynchronously.
		return a.ctx.Err()
	}
	if err := cancellation(); err != nil {
		return record, err
	}
	remote, ok := a.sandbox.(*sandbox.Remote)
	if !ok {
		return record, conflictError("The sandbox is off; there is no containment to test")
	}
	if info, err := remote.Info(ctx); err == nil {
		record.ImageID, record.Runtime = info.ImageID, info.Runtime
	}
	report, err := sandbox.Probe(ctx, remote)
	if cancelErr := cancellation(); cancelErr != nil {
		// A probe canceled by its caller or shutdown observed nothing about containment; the last result stands.
		return record, cancelErr
	}
	if err != nil {
		record.Error = new(redact.Error(err))
	} else {
		record.Checks, record.Kernel, record.Passed = report.Checks, report.Kernel, report.Passed()
		if report.Sandbox != nil {
			// The probe's own request can move the broker to an image rebuilt under the same tag since the info above
			// was read: the proof belongs to the image it ran on.
			record.ImageID, record.Runtime = report.Sandbox.ImageID, report.Sandbox.Runtime
		}
	}
	if err := cancellation(); err != nil {
		return record, err
	}
	if err := a.Store.Put("settings", selfTestRecord, record); err != nil {
		return record, err
	}
	return record, nil
}

func (r SandboxSelfTest) failure() error {
	if r.Error != nil {
		return fmt.Errorf("Sandbox self-test could not run: %s", *r.Error)
	}
	if r.Passed {
		return nil
	}
	failed := []string{}
	for _, check := range r.Checks {
		if !check.Passed {
			failed = append(failed, check.Label+" ("+check.Detail+")")
		}
	}
	return fmt.Errorf("Sandbox self-test failed: %s", strings.Join(failed, "; "))
}

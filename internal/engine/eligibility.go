package engine

import (
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

// Capabilities describe current UI eligibility, not authorization. Controls
// revalidate the same runtime blockers under the admission gate on arrival.
type Capability struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

type Capabilities struct {
	Pause     Capability `json:"pause"`
	Resume    Capability `json:"resume"`
	Cycle     Capability `json:"cycle"`
	Audit     Capability `json:"audit"`
	Configure Capability `json:"configure"`
}

type operationActivity struct {
	idle     bool
	audit    bool
	baseline bool
}

func (a *App) operationActivity() operationActivity {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	return operationActivity{idle: a.runtime.idle(), audit: a.runtime.auditActive(), baseline: a.runtime.baseline != nil}
}

func controlBlocker(action string, control model.Control, activity operationActivity) error {
	refused := (action == "audit" || action == "cycle") && (!control.Paused || !activity.idle) ||
		(action == "resume" || action == "cycle") && activity.audit ||
		action == "resume" && activity.baseline
	switch {
	case !refused:
		return nil
	case activity.baseline:
		return codedConflict(ActiveWorkConflict, "Wait for the baseline check to finish.")
	case action == "audit":
		return codedConflict(ActiveWorkConflict, "Audits require paused operation with no active work. Pause the service and wait for active work to finish.")
	case action == "cycle":
		return codedConflict(ActiveWorkConflict, "Run once requires paused operation with no active work. Pause the service and wait for active work to finish.")
	default:
		return codedConflict(ActiveWorkConflict, "Wait for the audit to finish before starting continuous operation.")
	}
}

func capabilities(control model.Control, cfg config.Config, activity operationActivity, recovery *string, capacity model.PlanningCapacity) Capabilities {
	operation := func(action string) Capability {
		if action != "pause" && recovery != nil {
			return Capability{Reason: errRecoveryBlocked.Error()}
		}
		if err := controlBlocker(action, control, activity); err != nil {
			return Capability{Reason: err.Error()}
		}
		// Pausing an audit is an accepted API no-op; the dashboard avoids offering
		// it while already paused. Recovery remains interruptible.
		if action == "pause" && activity.audit && recovery == nil {
			return Capability{Reason: "Wait for the audit to finish."}
		}
		if action == "pause" {
			return Capability{Allowed: true}
		}
		var err error
		if action == "audit" {
			err = cfg.ValidateAudit()
		} else {
			err = cfg.Validate(true)
		}
		if err != nil {
			return Capability{Reason: err.Error()}
		}
		if action == "cycle" || action == "audit" {
			if err := capacity.EnsureAvailable(); err != nil {
				return Capability{Reason: err.Error()}
			}
		}
		return Capability{Allowed: true}
	}
	configure := Capability{Allowed: control.Paused && activity.idle}
	if !configure.Allowed {
		configure.Reason = "Pause and wait for active work to finish before changing configuration."
	}
	return Capabilities{Pause: operation("pause"), Resume: operation("resume"), Cycle: operation("cycle"), Audit: operation("audit"), Configure: configure}
}

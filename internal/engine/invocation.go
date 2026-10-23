package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

type invocation struct {
	cycleID   string
	task      *model.Task
	role      string
	route     config.Route
	workspace string
	// session is the durable thread slot for persistent execution/repair.
	// A nil slot denotes a fresh review or isolated planning turn.
	session   **string
	prompt    string
	schema    schemas.Schema
	reserved  bool
	prepare   func() error
	judge     func(session, answer string) (string, error)
	completed func()
}

// entity names the record an invocation belongs to: the task it serves, or the cycle a session-less role runs for.
func (inv invocation) entity() string {
	if inv.task != nil {
		return inv.task.ID
	}
	return inv.cycleID
}

func (a *App) invoke(ctx context.Context, clients *runner.Runners, inv invocation) (answer string, err error) {
	if inv.task == nil {
		defer func() {
			// A failed start still owns a runner whose exit can explain the failure.
			if closeErr := clients.Close(); closeErr != nil && !errors.Is(err, closeErr) {
				err = errors.Join(err, closeErr)
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("Operation cancelled: %w", err)
	}

	var resume *string
	if inv.session != nil && *inv.session != nil {
		if inv.reserved {
			return "", fmt.Errorf("A resumed %s turn cannot use a reserved admission", inv.role)
		}
		identity := **inv.session
		resume = &identity
		if _, err := findSession(inv.task, identity, inv.role); err != nil {
			return "", err
		}
	}
	if !inv.reserved {
		if err := a.admit(ctx, inv.cycleID, inv.task, inv.role, inv.route); err != nil {
			return "", err
		}
	}
	if inv.prepare != nil {
		if err := inv.prepare(); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("Operation cancelled: %w", err)
	}
	session, resumed, err := a.startInvocation(ctx, clients, inv, resume)
	if err != nil {
		return "", err
	}

	if inv.task == nil {
		record := model.NewSession(session, inv.role, inv.route)
		_ = a.store.Event(inv.cycleID, model.EventSessionStarted, fmt.Sprintf("%s: %s · %s", inv.role, session, inv.route))
		// Planning roles run in their own goroutines, outside the task supervisor. Convert a turn panic before the
		// session is finalized so the failed evidence is retained and the ordinary cycle failure path can join
		// siblings and release ownership.
		var answer, summary string
		var turnErr error
		func() {
			defer func() {
				if panicked := recover(); panicked != nil {
					answer, summary = "", ""
					turnErr = errors.Join(fmt.Errorf("Planning role %s panicked: %v", inv.role, panicked), clients.Release())
				}
			}()
			answer, summary, turnErr = a.turn(ctx, clients, inv, session)
		}()
		record.Sandbox = model.MergeSandbox(record.Sandbox, clients.TakeEvidence())
		if inv.task == nil {
			if closeErr := clients.Close(); turnErr == nil && closeErr != nil {
				turnErr = closeErr
			}
		}
		switch {
		case turnErr == nil:
			record.MarkCompleted(redact.Text(summary))
		case a.ctx.Err() != nil:
			record.MarkInterrupted()
			answer = ""
		default:
			record.MarkFailed(redact.Error(turnErr))
			answer = ""
		}
		if err := a.store.AppendCycleSession(inv.cycleID, record); err != nil {
			return answer, errors.Join(turnErr, err)
		}
		return answer, turnErr
	}

	task := inv.task
	if !resumed {
		record := model.NewSession(session, inv.role, inv.route)
		record.FirstTurnStarted = new(false)
		task.Sessions = append(task.Sessions, record)
		if inv.session != nil {
			*inv.session = &session
		}
	} else {
		record, err := findSession(task, session, inv.role)
		if err != nil {
			return "", err
		}
		record.MarkRunning()
		// A successful resume establishes that the runner retained this thread.
		record.FirstTurnStarted = new(true)
	}
	if err := a.checkpointTask(ctx, task); err != nil {
		return "", err
	}
	answer, summary, err := a.turn(ctx, clients, inv, session)
	// The sandbox record stays with the session whether or not the turn succeeded; the caller saves the task.
	if record, recordErr := findSession(task, session, inv.role); recordErr == nil {
		record.Sandbox = model.MergeSandbox(record.Sandbox, clients.TakeEvidence())
	}
	if err != nil {
		return "", err
	}
	record, err := findSession(task, session, inv.role)
	if err != nil {
		return "", err
	}
	if inv.completed != nil {
		// Keep successful-turn accounting in the first completed-session
		// checkpoint, including the supervisor's retry if this save fails.
		inv.completed()
	}
	record.MarkCompleted(redact.Text(summary))
	return answer, a.checkpointTask(ctx, task)
}

// A missing, never-started thread has no runner state to resume. Retain its failed
// evidence, clear only its active identity, and reserve a new start after cleanup.
func (a *App) startInvocation(ctx context.Context, clients *runner.Runners, inv invocation, resume *string) (string, bool, error) {
	session, err := clients.Start(inv.route, inv.workspace, resume)
	if err == nil {
		return session, resume != nil, nil
	}
	released := clients.Release()
	if resume == nil || inv.task == nil {
		// A runner that started and then failed to establish its session still ran a sandbox, and no session record
		// exists to carry its record yet, so keep it as evidence rather than losing it with the release.
		a.keepSandboxEvidence(inv.entity(), inv.role+" session start", clients.TakeEvidence())
		return "", false, errors.Join(err, released)
	}
	record, recordErr := findSession(inv.task, *resume, inv.role)
	if recordErr != nil {
		a.keepSandboxEvidence(inv.entity(), inv.role+" session start", clients.TakeEvidence())
		return "", false, errors.Join(err, released, recordErr)
	}
	record.Sandbox = model.MergeSandbox(record.Sandbox, clients.TakeEvidence())
	if !errors.Is(err, runner.ErrSessionMissing) || inv.route.Backend != config.BackendCodex ||
		record.FirstTurnStarted == nil || *record.FirstTurnStarted || record.Status == model.SessionCompleted || inv.session == nil || released != nil || ctx.Err() != nil {
		return "", false, errors.Join(err, released)
	}
	record.MarkFailed(redact.Text(strings.TrimSpace(record.Summary + "\nNever-started session is missing; a fresh session is required: " + err.Error())))
	*inv.session = nil
	if saveErr := a.checkpointTask(ctx, inv.task); saveErr != nil {
		return "", false, errors.Join(err, saveErr)
	}
	if err := a.admit(ctx, inv.cycleID, inv.task, inv.role, inv.route); err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	session, err = clients.Start(inv.route, inv.workspace, nil)
	if err != nil {
		cleanup := clients.Release()
		// The replacement child ran too; its record belongs beside the failure like the one it replaced.
		a.keepSandboxEvidence(inv.entity(), inv.role+" session start", clients.TakeEvidence())
		return "", false, errors.Join(err, cleanup)
	}
	return session, false, nil
}

func (a *App) turn(ctx context.Context, clients *runner.Runners, inv invocation, session string) (answer, summary string, err error) {
	var started func() error
	if inv.task != nil {
		started = func() error {
			record, err := findSession(inv.task, session, inv.role)
			if err != nil {
				return err
			}
			if record.FirstTurnStarted != nil && *record.FirstTurnStarted {
				return nil
			}
			record.FirstTurnStarted = new(true)
			return a.checkpointTask(ctx, inv.task)
		}
	}
	answer, err = clients.Turn(session, inv.route, inv.workspace, inv.prompt, inv.schema, started)
	// Nothing a runner started may outlive its turn: the judge and the orchestrator's git read the work tree next.
	released := clients.Release()
	if err = errors.Join(err, released); err != nil {
		return "", "", err
	}
	if inv.judge == nil {
		return answer, answer, nil
	}
	if inv.task != nil {
		record, err := findSession(inv.task, session, inv.role)
		if err != nil {
			return "", "", err
		}
		record.Summary = redact.Text(answer)
		if err := a.checkpointTask(ctx, inv.task); err != nil {
			return "", "", err
		}
	}
	summary, err = inv.judge(session, answer)
	if err != nil {
		return "", "", err
	}
	return answer, summary, nil
}

func (a *App) admit(ctx context.Context, cycleID string, task *model.Task, role string, route config.Route) error {
	owner := filepath.Join("cycles", cycleID)
	var taskID *string
	if task != nil {
		taskID = &task.ID
		owner = filepath.Join("tasks", task.ID)
	}
	size, err := a.measureLocked(ctx, owner)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Operation cancelled: %w", err)
	}
	return a.store.ReserveSession(size, store.NewAdmission(cycleID, taskID, role, route))
}

func (a *App) measureLocked(ctx context.Context, owner string) (uint64, error) {
	a.fsLock.Lock()
	defer a.fsLock.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("Operation cancelled: %w", err)
	}
	// Storage is a pre-turn snapshot, not a disk reservation. Exclude trusted filesystem changes only for the scan;
	// the store independently serializes budget reservations and must not stall unrelated setup/status/cleanup.
	return a.measure(ctx, owner)
}

// ownedRoots are the data directory's parents of owned roots, each <parent>/<id>, where sandboxes write.
var ownedRoots = []string{"tasks", "cycles", "baselines", scratchDir}

// ownerDepth is how many leading components of a path below the data directory name its owner: <parent>/<id>.
const ownerDepth = 2

func (a *App) nestedRoots() []string {
	a.nestedMu.RLock()
	defer a.nestedMu.RUnlock()
	return a.nested
}

// measure measures the data directory for an admission on behalf of owner, an owned root relative to it. A subtree
// the walk could not measure, unreadable or too costly to traverse, holds unknown bytes: it puts its own owner over the
// limit, and puts everyone over it when it lies outside any owned root. Another owner's unmeasured subtree does not
// stop this admission; that owner can admit nothing more until its retained work is resolved.
func (a *App) measure(ctx context.Context, owner string) (uint64, error) {
	usage, err := workspace.Measure(ctx, a.dataDir, ownerDepth, a.nestedRoots()...)
	if err != nil {
		return 0, err
	}
	for _, rel := range usage.Unmeasured {
		parent, _, owned := strings.Cut(rel, string(filepath.Separator))
		if !owned || !slices.Contains(ownedRoots, parent) {
			// No sandbox writes here, so this is the host's own: lost+found at a filesystem's root, for example.
			return 0, fmt.Errorf("Storage under %s in the data directory could not be measured; it is unreadable, nested too deeply, or too costly to traverse. Make it readable to the service or move it out of the data directory: %w",
				redact.Text(rel), model.BlockedStorageLimit)
		}
		if rel == owner {
			return 0, fmt.Errorf("Workspace storage under %s could not be measured; it is unreadable, nested too deeply, or too costly to traverse. Resolve that retained work: %w",
				redact.Text(rel), model.BlockedStorageLimit)
		}
	}
	return usage.Bytes, nil
}

func findSession(task *model.Task, thread, role string) (*model.Session, error) {
	for i := range task.Sessions {
		if task.Sessions[i].ID == thread && task.Sessions[i].Role == role {
			return &task.Sessions[i], nil
		}
	}
	return nil, fmt.Errorf("Task %s is missing its %s session record (%s): %w", task.ID, role, thread, model.BlockedWorkspaceInvalid)
}

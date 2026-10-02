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
	cycleID     string
	task        *model.Task
	role        string
	route       config.Route
	workspace   string
	resume      *string
	keep        func(session string)
	prompt      string
	schema      schemas.Schema
	reserved    bool
	prepare     func() error
	judge       func(session, answer string) (string, error)
	ownsClients bool
}

func (a *App) invoke(ctx context.Context, clients *runner.Runners, inv invocation) (answer string, err error) {
	if inv.ownsClients {
		defer func() { _ = clients.Close() }()
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("Operation cancelled: %w", err)
	}

	var resume *string
	if inv.resume != nil {
		if inv.reserved {
			return "", fmt.Errorf("A resumed %s turn cannot use a reserved admission", inv.role)
		}
		identity := *inv.resume
		resume = &identity
		if _, err := sessionMut(inv.task, identity, inv.role); err != nil {
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
	session, err := clients.Start(inv.route, inv.workspace, resume)
	if err != nil {
		return "", err
	}

	if inv.task == nil {
		record := model.NewSession(session, inv.role, inv.route)
		_ = a.Store.Event(inv.cycleID, "session_started", fmt.Sprintf("%s: %s · %s", inv.role, session, inv.route))
		answer, summary, turnErr := a.turn(clients, inv, session)
		record.Sandbox = model.MergeSandbox(record.Sandbox, clients.TakeEvidence())
		if inv.ownsClients {
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
		if err := a.Store.AppendCycleSession(inv.cycleID, record); err != nil {
			return answer, errors.Join(turnErr, err)
		}
		return answer, turnErr
	}

	task := inv.task
	if resume == nil {
		task.Sessions = append(task.Sessions, model.NewSession(session, inv.role, inv.route))
		if inv.keep != nil {
			inv.keep(session)
		}
	} else {
		record, err := sessionMut(task, session, inv.role)
		if err != nil {
			return "", err
		}
		record.MarkRunning()
	}
	if err := a.saveTask(task); err != nil {
		return "", err
	}
	answer, summary, err := a.turn(clients, inv, session)
	// The sandbox record stays with the session whether or not the turn succeeded; the caller saves the task.
	if record, recordErr := sessionMut(task, session, inv.role); recordErr == nil {
		record.Sandbox = model.MergeSandbox(record.Sandbox, clients.TakeEvidence())
	}
	if err != nil {
		return "", err
	}
	record, err := sessionMut(task, session, inv.role)
	if err != nil {
		return "", err
	}
	record.MarkCompleted(redact.Text(summary))
	return answer, a.saveTask(task)
}

func (a *App) turn(clients *runner.Runners, inv invocation, session string) (answer, summary string, err error) {
	answer, err = clients.Turn(session, inv.route, inv.workspace, inv.prompt, inv.schema)
	// Nothing a runner started may outlive its turn: the judge and the orchestrator's git read the work tree next.
	released := clients.Release()
	if err = errors.Join(err, released); err != nil {
		return "", "", err
	}
	if inv.judge == nil {
		return answer, answer, nil
	}
	if inv.task != nil {
		record, err := sessionMut(inv.task, session, inv.role)
		if err != nil {
			return "", "", err
		}
		record.Summary = redact.Text(answer)
		if err := a.saveTask(inv.task); err != nil {
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
	a.planningStorage.Lock()
	defer a.planningStorage.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Operation cancelled: %w", err)
	}
	owner := filepath.Join("cycles", cycleID)
	var taskID *string
	if task != nil {
		taskID = &task.ID
		owner = filepath.Join("tasks", task.ID)
	}
	size, err := a.measureFor(owner)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Operation cancelled: %w", err)
	}
	return a.Store.ReserveSession(size, store.NewAdmission(cycleID, taskID, role, route))
}

// ownedRoots are the data directory's parents of owned roots, each <parent>/<id>, where sandboxes write.
var ownedRoots = []string{"tasks", "cycles", "baselines", scratchDir}

// ownerDepth is how many leading components of a path below the data directory name its owner: <parent>/<id>.
const ownerDepth = 2

// measureFor measures the data directory for an admission on behalf of owner, an owned root relative to it. A subtree
// the walk could not measure, unreadable or too costly to traverse, holds unknown bytes: it puts its own owner over the
// limit, and puts everyone over it when it lies outside any owned root. Another owner's unmeasured subtree does not
// stop this admission; that owner can admit nothing more until its retained work is resolved.
func (a *App) measureFor(owner string) (uint64, error) {
	usage, err := workspace.Measure(a.DataDir, ownerDepth)
	if err != nil {
		return 0, err
	}
	for _, rel := range usage.Unmeasured {
		parent, _, owned := strings.Cut(rel, string(filepath.Separator))
		if !owned || !slices.Contains(ownedRoots, parent) {
			// No sandbox writes here, so this is the host's own: lost+found at a filesystem's root, for example.
			return 0, fmt.Errorf("Storage under %s in the data directory could not be measured; it is unreadable, nested too deeply, or too costly to traverse. Make it readable to the service or move it out of the data directory: %w",
				redact.Text(rel), model.BlockedReasonStorageLimit)
		}
		if rel == owner {
			return 0, fmt.Errorf("Workspace storage under %s could not be measured; it is unreadable, nested too deeply, or too costly to traverse. Resolve that retained work: %w",
				redact.Text(rel), model.BlockedReasonStorageLimit)
		}
	}
	return usage.Bytes, nil
}

func sessionMut(task *model.Task, thread, role string) (*model.Session, error) {
	for i := range task.Sessions {
		if task.Sessions[i].ID == thread && task.Sessions[i].Role == role {
			return &task.Sessions[i], nil
		}
	}
	return nil, fmt.Errorf("Task %s is missing its %s session record (%s): %w", task.ID, role, thread, model.BlockedReasonWorkspaceInvalid)
}

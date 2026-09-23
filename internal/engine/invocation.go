// invocation.go is the role invocation module: the one place an agent turn is
// run. Planning roles, the executor, each fresh reviewer and each repair turn
// all pass through invoke, which owns storage measurement and the daily
// admission, session start or resume, the session record lifecycle, the turn
// itself and redaction. Callers build prompts and interpret answers; they
// never reserve admissions, resume threads or mark session records.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// invocation describes one agent turn for a role.
//
// Where the session record lives follows the owner. A task-owned record is
// saved on the task as soon as the session starts and completed in place. If
// the turn or the judge fails, the record stays running: the task supervisor
// fails it with the task's terminal reason, because only it knows whether a
// deadline or a cancellation ended the turn. A cycle-owned record is appended
// to the cycle once, when the turn ends, as completed or failed.
type invocation struct {
	// cycleID owns the admission. task, when set, is the task within that
	// cycle that owns the turn and holds its session record; nil leaves the
	// record on the cycle.
	cycleID string
	task    *model.Task
	// role labels both the admission and the session record.
	role      string
	route     config.Route
	workspace string
	// resume is the role's recorded persistent thread (task.ExecutionSession,
	// task.RepairSession), resumed before the turn; nil starts a fresh
	// session. keep, when set, records a fresh session as the role's
	// persistent thread so later turns resume it. Roles without a persistent
	// thread leave both unset.
	resume *string
	keep   func(session string)
	prompt string
	schema schemas.Schema
	// reserved means a fresh session's first turn was already admitted
	// (executor initialization). A resumed turn always reserves its own
	// admission, so invoke rejects reserved together with resume.
	reserved bool
	// prepare, when set, runs after the admission and before the session
	// starts; a planning role clones its workspace here.
	prepare func() error
	// judge, when set, inspects the raw answer before the record completes. It
	// returns the summary to save, which is redacted here, or an error that
	// fails the turn. A task-owned record saves the redacted answer as its
	// provisional summary first, so a rejected answer stays inspectable.
	judge func(session, answer string) (string, error)
	// ownsClients closes the client scope when the turn ends, before the
	// record is finalized; a close failure fails an otherwise good turn.
	ownsClients bool
}

// invoke runs one role turn and returns the session identity and the raw
// answer, or the classified error that ended the turn.
func (a *App) invoke(ctx context.Context, clients *runner.Runners, inv invocation) (session, answer string, err error) {
	closed := false
	closeClients := func() error {
		if !inv.ownsClients || closed {
			return nil
		}
		closed = true
		return clients.Close()
	}
	defer func() { _ = closeClients() }()

	var resume *string
	if inv.resume != nil {
		if inv.reserved {
			return "", "", fmt.Errorf("A resumed %s turn cannot use a reserved admission", inv.role)
		}
		identity := *inv.resume
		resume = &identity
		// A resumed thread must still have its record before any admission.
		if _, err := sessionMut(inv.task, identity, inv.role); err != nil {
			return "", "", err
		}
	}
	if !inv.reserved {
		if err := a.admit(inv.cycleID, inv.task, inv.role, inv.route); err != nil {
			return "", "", err
		}
	}
	if inv.prepare != nil {
		if err := inv.prepare(); err != nil {
			return "", "", err
		}
	}
	session, err = clients.Start(inv.route, inv.workspace, resume)
	if err != nil {
		return "", "", err
	}

	if inv.task == nil {
		record := model.NewSession(session, inv.role, inv.route)
		_ = a.Store.Event(inv.cycleID, "session_started", fmt.Sprintf("%s: %s · %s", inv.role, session, inv.route))
		answer, summary, turnErr := a.turn(clients, inv, session)
		if closeErr := closeClients(); turnErr == nil && closeErr != nil {
			turnErr = closeErr
		}
		if turnErr == nil {
			record.MarkCompleted(store.Redact(summary))
		} else {
			record.MarkFailed(store.ErrorMessage(turnErr))
			answer = ""
		}
		if err := a.Store.AppendCycleSession(inv.cycleID, record); err != nil {
			return session, answer, errors.Join(turnErr, err)
		}
		return session, answer, turnErr
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
			return session, "", err
		}
		record.MarkRunning()
	}
	if err := a.saveTask(task); err != nil {
		return session, "", err
	}
	answer, summary, err := a.turn(clients, inv, session)
	if err != nil {
		return session, "", err
	}
	record, err := sessionMut(task, session, inv.role)
	if err != nil {
		return session, "", err
	}
	record.MarkCompleted(store.Redact(summary))
	return session, answer, a.saveTask(task)
}

// turn runs the prompt on a started session and applies the judge, returning
// the raw answer and the unredacted summary to record.
func (a *App) turn(clients *runner.Runners, inv invocation, session string) (answer, summary string, err error) {
	answer, err = clients.Turn(session, inv.route, inv.workspace, inv.prompt, inv.schema)
	if err != nil {
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
		record.Summary = store.Redact(answer)
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

// admit measures managed storage and reserves one daily admission for a turn,
// strictly before its session starts. Executor initialization reserves the
// first executor turn here and then invokes it as reserved.
func (a *App) admit(cycleID string, task *model.Task, role string, route config.Route) error {
	size, err := workspace.DirectorySize(a.DataDir)
	if err != nil {
		return err
	}
	var taskID *string
	if task != nil {
		taskID = &task.ID
	}
	return a.Store.ReserveSession(size, store.NewAdmission(cycleID, taskID, role, route))
}

func sessionMut(task *model.Task, thread, role string) (*model.Session, error) {
	for i := range task.Sessions {
		if task.Sessions[i].ID == thread && task.Sessions[i].Role == role {
			return &task.Sessions[i], nil
		}
	}
	return nil, fmt.Errorf("Task %s is missing its %s session record (%s): %w", task.ID, role, thread, model.BlockedReasonWorkspaceInvalid)
}

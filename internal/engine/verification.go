package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func (a *App) verifyRevision(ctx context.Context, task *model.Task, revision string) ([]string, error) {
	cfg := task.ExecutionConfig()
	ws := task.Workspace
	verificationErrors := []string{}
	if err := a.transition(task, model.StatusVerifying); err != nil {
		return nil, err
	}
	if err := ensureWorkspaceAt(ctx, cfg, ws, revision); err != nil {
		return nil, err
	}
	checkout, discard, err := a.verificationCheckout(ctx, task, revision)
	if err != nil {
		return nil, err
	}
	defer discard()
	for i, command := range cfg.VerificationCommands {
		outcome := runCheckCommand(ctx, a.sandbox, cfg, checkout, command, revision, i == 0)
		if ctx.Err() != nil {
			return nil, process.ErrCancelled
		}
		if outcome.sandboxFailed() {
			a.keepSandboxEvidence(task.ID, command, outcome.sandbox)
			return nil, &sandboxUnavailable{sandboxFailure(command, outcome.capture)}
		}
		failed := outcome.failed()
		note := ""
		switch {
		case outcome.intactErr != nil:
			note = "\n" + boundedTail(redact.Secrets(outcome.intactErr.Error()), verificationNoteLimit)
		case !outcome.intact:
			note = "\nWorkspace or HEAD changed during this verification command"
		}
		output := outcome.evidenceText(verificationOutputLimit-len(note)) + note
		task.Verification = append(task.Verification, model.Verification{
			Command: command, Success: outcome.intactErr == nil && outcome.intact && !failed, Output: output, Revision: revision, CreatedAt: model.Now(),
			Sandbox: outcome.sandbox,
		})
		if err := a.saveTask(task); err != nil {
			return nil, err
		}
		if outcome.intactErr != nil {
			return nil, fmt.Errorf("Workspace state check failed during verification: %w", outcome.intactErr)
		}
		if !outcome.intact {
			return nil, model.BlockedReasonWorkspaceInvalid
		}
		if failed {
			verificationErrors = append(verificationErrors, command+": "+output)
		}
	}
	return verificationErrors, nil
}

// verificationDir holds each verification run's pristine checkout inside the task's root.
const verificationDir = "verify"

// verificationCheckout clones the reviewed revision fresh for one verification run and returns a function that removes
// it. Anything a session left in the task work tree beyond the reviewed commit cannot influence the result.
func (a *App) verificationCheckout(ctx context.Context, task *model.Task, revision string) (string, func(), error) {
	taskRoot := filepath.Dir(task.Workspace)
	root := filepath.Join(taskRoot, verificationDir)
	if err := workspace.RemoveOwnedDir(taskRoot, root); err != nil {
		return "", nil, fmt.Errorf("Removing a previous verification checkout: %w", err)
	}
	checkout := filepath.Join(root, "workspace")
	if err := gitops.CloneReviewed(ctx, task.ExecutionConfig(), task.Workspace, checkout, revision); err != nil {
		_ = workspace.RemoveOwnedDir(taskRoot, root)
		return "", nil, fmt.Errorf("Preparing the verification checkout: %w", err)
	}
	return checkout, func() {
		if err := workspace.RemoveOwnedDir(taskRoot, root); err != nil {
			_ = a.Store.Event(task.ID, "cleanup_error", "verification checkout: "+redact.Error(err))
		}
	}, nil
}

type checkOutcome struct {
	captured  *process.ProcessOutput
	capture   error
	intact    bool
	intactErr error
	sandbox   *model.SandboxRecord
}

func (o checkOutcome) failed() bool {
	return o.capture != nil || !o.captured.Status.Success()
}

// sandboxFailed reports that the sandbox, not the command, failed: it refused the command, lost it or could not confirm
// how it ended. The command then has no result of its own, so it is neither a verification failure nor a pass.
func (o checkOutcome) sandboxFailed() bool {
	return o.capture != nil && sandbox.Infrastructure(o.capture)
}

func sandboxFailure(command string, err error) error {
	return fmt.Errorf("The sandbox could not run verification command %s: %w", debugString(command), err)
}

// keepSandboxEvidence keeps the broker's record of a sandbox whose command has no result of its own, as a session
// keeps it for a failed turn. The broker reports one when the command ran and the sandbox failed after it, for example
// removing its container: the image, runtime, OOM and egress of untrusted code that did run. It becomes an event on
// entity beside the failure, never the command's verification.
func (a *App) keepSandboxEvidence(entity, command string, record *model.SandboxRecord) {
	if record == nil {
		return
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	_ = a.Store.Event(entity, "sandbox_evidence", debugString(command)+": "+string(data))
}

// sandboxUnavailable blocks a task as runner_unavailable, which a retry clears, when the sandbox failed one of its
// verification commands. Nothing is recorded as that command's verification and no repair round is spent on it.
type sandboxUnavailable struct{ err error }

func (e *sandboxUnavailable) Error() string { return e.err.Error() }
func (e *sandboxUnavailable) Unwrap() []error {
	return []error{model.BlockedReasonRunnerUnavailable, e.err}
}

const (
	verificationOutputLimit = 16 * 1024
	verificationNoteLimit   = 4096
	outputTruncatedMarker   = "[output truncated]"
)

func (o checkOutcome) evidenceText(limit int) string {
	if o.capture != nil {
		return boundedTail(redact.Secrets(o.capture.Error()), limit)
	}
	stdoutCapture, stderrCapture := o.captured.SafeCaptures()
	clean := func(stream process.SafeCapture) string {
		text := strings.TrimSpace(stream.Head)
		if !stream.Truncated {
			return text
		}
		text += "\n" + outputTruncatedMarker
		if tail := strings.TrimSpace(stream.Tail); tail != "" {
			text += "\n" + tail
		}
		return text
	}
	status := ""
	if !o.captured.Status.Success() {
		status = "\n" + o.captured.Status.String()
	}
	stdout := clean(stdoutCapture)
	stderr := ""
	if text := clean(stderrCapture); text != "" {
		const separator = "\n[stderr]\n"
		budget := max(limit/2, limit-len(separator)-len(stdout)-len(status))
		stderr = separator + boundedTail(text, budget)
	}
	return boundedTail(stdout, limit-len(stderr)-len(status)) + stderr + status
}

func boundedTail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const prefix = outputTruncatedMarker + "\n"
	start := len(text) - max(limit-len(prefix), 0)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return prefix + text[start:]
}

func runCheckCommand(ctx context.Context, box sandbox.Backend, cfg config.Config, ws, command, revision string, fresh bool) checkOutcome {
	captured, evidence, captureErr := sandbox.Verify(ctx, box, ws, command, cfg.CommandTimeoutSeconds, fresh)
	outcome := checkOutcome{captured: captured, capture: captureErr, sandbox: evidence}
	if ctx.Err() == nil {
		outcome.intact, outcome.intactErr = gitops.At(ctx, cfg, ws, revision)
	}
	return outcome
}

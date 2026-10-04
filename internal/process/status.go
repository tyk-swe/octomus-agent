package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

type Status struct {
	state *os.ProcessState
	exit  *Exit
}

// Exit is how a sandboxed child ended as its container runtime reports it: the init process's exit code, whether the
// memory limit killed it, and whether Octomus itself stopped it.
type Exit struct {
	Code   int
	OOM    bool
	Killed bool
	// Reason explains a kill that was not the caller's own, such as a sandbox time limit.
	Reason string
}

// ErrKilled marks a sandboxed child that Octomus stopped on purpose, the counterpart of a host group's SIGKILL.
var ErrKilled = errors.New("stopped by Octomus")

func hostStatus(state *os.ProcessState) Status { return Status{state: state} }

func ExitStatus(exit Exit) Status { return Status{exit: &exit} }

func (s Status) Success() bool {
	if s.exit != nil {
		return s.exit.Code == 0 && !s.exit.OOM && !s.exit.Killed
	}
	return s.state != nil && s.state.Success()
}

func (s Status) Code() (int, bool) {
	if s.exit != nil {
		return s.exit.Code, !s.exit.Killed
	}
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok {
		return ws.ExitStatus(), ws.Exited()
	}
	code := s.state.ExitCode()
	return code, code >= 0
}

// Err reports an unsuccessful exit the way exec does for host children, so owners can tell a deliberate kill apart.
func (s Status) Err() error {
	switch {
	case s.Success():
		return nil
	case s.exit != nil && s.exit.Killed && s.exit.Reason == "":
		return ErrKilled
	case s.exit != nil:
		return errors.New(s.String())
	case s.state == nil:
		return errors.New(s.String())
	}
	return &exec.ExitError{ProcessState: s.state}
}

func signalString(signal int) string {
	if name := unix.SignalName(syscall.Signal(signal)); name != "" {
		return " (" + name + ")"
	}
	return ""
}

func (s Status) String() string {
	if s.exit != nil {
		switch {
		case s.exit.OOM:
			return fmt.Sprintf("exit status: %d (sandbox memory limit exceeded)", s.exit.Code)
		case s.exit.Killed && s.exit.Reason != "":
			return s.exit.Reason
		case s.exit.Killed:
			return "stopped by Octomus"
		}
		return fmt.Sprintf("exit status: %d", s.exit.Code)
	}
	if s.state == nil {
		return "unrecognised wait status: 0 0x0"
	}
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok {
		switch {
		case ws.Exited():
			return fmt.Sprintf("exit status: %d", ws.ExitStatus())
		case ws.Signaled():
			sig := int(ws.Signal())
			if ws.CoreDump() {
				return fmt.Sprintf("signal: %d%s (core dumped)", sig, signalString(sig))
			}
			return fmt.Sprintf("signal: %d%s", sig, signalString(sig))
		case ws.Stopped():
			sig := int(ws.StopSignal())
			return fmt.Sprintf("stopped (not terminated) by signal: %d%s", sig, signalString(sig))
		case ws.Continued():
			return "continued (WIFCONTINUED)"
		default:
			raw := int(ws)
			return fmt.Sprintf("unrecognised wait status: %d %#x", raw, raw)
		}
	}
	return s.state.String()
}

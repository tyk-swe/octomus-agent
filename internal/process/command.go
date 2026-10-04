package process

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/tyk-swe/octomus-agent/internal/redact"
)

func Command(binary string, cwd string) *exec.Cmd {
	cmd := exec.Command(binary)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case redact.TokenEnv, redact.WebhookEnv, "GIT_TERMINAL_PROMPT":
			continue
		// Repository-locating Git variables would redirect child git away from cmd.Dir; GIT_CONFIG* passes through.
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_COMMON_DIR",
			"GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE", "GIT_NO_REPLACE_OBJECTS",
			"GIT_REPLACE_REF_BASE":
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// HostChild is a command running in its own process group on this host, with pipes for its standard streams.
type HostChild struct {
	cmd    *exec.Cmd
	pgid   int
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	killed sync.Once
	waited sync.Once
	done   chan struct{}
	status Status
	err    error
}

// StartHost starts binary in its own process group with the Command environment plus extra, piping stdout and stderr,
// and stdin only when requested.
func StartHost(binary string, args []string, cwd string, extra []string, stdin bool) (*HostChild, error) {
	cmd := Command(binary, cwd)
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(cmd.Env, extra...)
	var parentEnds, childEnds []*os.File
	closeAll := func() {
		for _, f := range append(parentEnds, childEnds...) {
			f.Close()
		}
	}
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
		}
		return r, w, err
	}
	child := &HostChild{cmd: cmd, done: make(chan struct{})}
	if stdin {
		r, w, err := pipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdin, child.stdin = r, w
		parentEnds, childEnds = append(parentEnds, w), append(childEnds, r)
	}
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, child.stdout = stdoutW, stdoutR
	parentEnds, childEnds = append(parentEnds, stdoutR), append(childEnds, stdoutW)
	stderrR, stderrW, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr, child.stderr = stderrW, stderrR
	parentEnds, childEnds = append(parentEnds, stderrR), append(childEnds, stderrW)
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	// The child holds its own pipe fds now; the parent's copies must close or readers never see EOF.
	for _, f := range childEnds {
		f.Close()
	}
	child.pgid = cmd.Process.Pid
	return child, nil
}

func (h *HostChild) Stdin() *os.File       { return h.stdin }
func (h *HostChild) Stdout() io.ReadCloser { return h.stdout }
func (h *HostChild) Stderr() io.ReadCloser { return h.stderr }
func (h *HostChild) Terminate()            { _ = syscall.Kill(-h.pgid, syscall.SIGTERM) }

// Kill stops the whole process group once; a later call never signals a reused pid.
func (h *HostChild) Kill() {
	h.killed.Do(func() {
		_ = syscall.Kill(-h.pgid, syscall.SIGKILL)
		_ = h.cmd.Process.Kill()
	})
}

// Wait reaps the child once; later calls return the same result.
func (h *HostChild) Wait() (Status, error) {
	h.waited.Do(func() {
		defer close(h.done)
		err := h.cmd.Wait()
		if h.cmd.ProcessState == nil {
			h.err = err
			return
		}
		h.status = hostStatus(h.cmd.ProcessState)
	})
	<-h.done
	return h.status, h.err
}

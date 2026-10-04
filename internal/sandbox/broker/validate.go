package broker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// rootPattern names every owned root a sandbox may mount, relative to the data directory. Anything else in the data
// volume, including state.db, the trusted checkout and other roots' parents, is unreachable by construction.
var rootPattern = regexp.MustCompile(`^(tasks/` + uuidPattern + `(/verify)?|cycles/` + uuidPattern +
	`/(grounding|consolidation|adversary-[ab]|discovery-[0-9]{1,2})|baselines/` + uuidPattern + `|system/` + uuidPattern + `)$`)

var envKeyPattern = regexp.MustCompile(`^OPENCODE_[A-Z0-9_]{1,64}$`)

const (
	maxCommand  = 4096
	maxEnvBytes = 256 << 10
)

// plan is a request the broker has validated and resolved against its own configuration.
type plan struct {
	kind      string
	dir       string
	root      string
	rel       string
	scratch   bool
	runner    string
	mode      string
	probe     string
	command   string
	env       []string
	stdin     bool
	timeout   time.Duration
	readiness uint64
	// image is the image ID the configured tag resolved to for this sandbox.
	image string
}

func (c Config) plan(req wire.Request) (plan, error) {
	p := plan{stdin: req.Stdin, readiness: req.Readiness}
	switch req.Kind {
	case wire.KindRunner:
		p.kind = wire.KindRunner
		switch req.Runner {
		case wire.RunnerCodex:
			if req.Mode != wire.RunnerModeStdio {
				return plan{}, errors.New("Codex runs only over stdio")
			}
		case wire.RunnerOpenCode:
			if req.Mode != wire.RunnerModeOpenCode {
				return plan{}, errors.New("OpenCode runs only behind the sandbox HTTP bridge")
			}
			if req.Readiness == 0 || req.Readiness > 600 {
				return plan{}, errors.New("OpenCode readiness must be 1 to 600 seconds")
			}
			p.stdin = true
		default:
			return plan{}, errors.New("Unknown runner")
		}
		p.runner, p.mode = req.Runner, req.Mode
		size := 0
		for _, entry := range req.Env {
			key, _, ok := strings.Cut(entry, "=")
			if !ok || !envKeyPattern.MatchString(key) || strings.ContainsRune(entry, 0) {
				return plan{}, errors.New("Runner environment may only set OPENCODE_ variables")
			}
			size += len(entry)
		}
		if size > maxEnvBytes {
			return plan{}, errors.New("Runner environment is too large")
		}
		p.env = append([]string(nil), req.Env...)
	case wire.KindVerify:
		p.kind = wire.KindVerify
		if req.Command == "" || len(req.Command) > maxCommand || strings.ContainsRune(req.Command, 0) {
			return plan{}, errors.New("Verification command must be 1 to 4096 bytes without NUL")
		}
		if len(req.Env) > 0 || req.Stdin {
			return plan{}, errors.New("Verification sandboxes take no environment or stdin")
		}
		p.command = req.Command
	case wire.KindProbe:
		p.kind = wire.KindProbe
		if req.Dir != "" || len(req.Env) > 0 || req.Stdin {
			return plan{}, errors.New("Probe sandboxes mount nothing and take no input")
		}
		switch req.Mode {
		case wire.ProbeVersions, wire.ProbeContainment:
			p.probe = req.Mode
		default:
			return plan{}, errors.New("Unknown probe")
		}
	default:
		return plan{}, errors.New("Unknown sandbox kind")
	}
	seconds := c.MaxSeconds
	if req.Timeout > 0 && req.Timeout < seconds {
		seconds = req.Timeout
	}
	p.timeout = time.Duration(seconds) * time.Second
	if p.kind == wire.KindProbe {
		return p, nil
	}
	if err := c.resolveRoot(&p, req.Dir); err != nil {
		return plan{}, err
	}
	return p, nil
}

func (c Config) resolveRoot(p *plan, dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Base(dir) != wire.WorkspaceDir {
		return errors.New("Sandbox directory must be an owned root's workspace")
	}
	root := filepath.Dir(dir)
	rel, err := filepath.Rel(c.DataDir, root)
	if err != nil || !rootPattern.MatchString(rel) {
		return errors.New("Sandbox directory is not an owned root under the data directory")
	}
	p.dir, p.root, p.rel = dir, root, rel
	p.scratch = strings.HasPrefix(rel, "system/")
	required := []string{filepath.Join(rel, wire.WorkspaceDir)}
	if !p.scratch {
		required = append(required, filepath.Join(rel, workspace.GitDirName))
	}
	for _, dir := range wire.HomeDirs(p.kind) {
		required = append(required, filepath.Join(rel, dir))
	}
	for _, path := range required {
		if err := c.ownedDirectory(path); err != nil {
			return err
		}
	}
	if !p.scratch {
		// The .git pointer mounts read-only into the sandbox, so it must already be the plain file a trusted clone
		// writes; a missing or replaced pointer would otherwise be created or swapped inside the sandbox.
		if err := c.ownedFile(filepath.Join(rel, wire.WorkspaceDir, ".git")); err != nil {
			return err
		}
	}
	return nil
}

// ownedFile requires rel to resolve to a regular file owned by the sandbox user, inside directories ownedDirectory
// accepts, with no symlink components.
func (c Config) ownedFile(rel string) error {
	if err := c.ownedDirectory(filepath.Dir(rel)); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(c.DataDir, rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("Sandbox root is incomplete: %s is missing", rel)
		}
		return err
	}
	return c.ownedEntry(rel, info, false)
}

// ownedDirectory walks rel from the data directory without following symlinks: every component must be a real
// directory owned by the sandbox user. Docker also refuses subpaths that escape a volume; this check keeps the
// broker's own view strict and its errors clear.
func (c Config) ownedDirectory(rel string) error {
	current := c.DataDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("Sandbox root is incomplete: %s is missing", rel)
			}
			return err
		}
		if err := c.ownedEntry(rel, info, true); err != nil {
			return err
		}
	}
	return nil
}

// ownedEntry requires info, from an Lstat within rel, to be a plain file or directory owned by the sandbox user.
func (c Config) ownedEntry(rel string, info fs.FileInfo, wantDir bool) error {
	plain, what := info.Mode().IsRegular(), "file"
	if wantDir {
		plain, what = info.IsDir(), "directory"
	}
	if info.Mode()&fs.ModeSymlink != 0 || !plain {
		return fmt.Errorf("Sandbox root component %s is not a plain %s", rel, what)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != c.UID {
		return fmt.Errorf("Sandbox root component %s is not owned by the sandbox user", rel)
	}
	return nil
}

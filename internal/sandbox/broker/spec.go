package broker

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// runnerGitConfig replaces the global git configuration of runner sandboxes. The executor, repair and reviewer turns
// of a task share one persistent home, so git's per-user files there (~/.gitconfig, ~/.config/git/config, attributes
// and ignore) would let one turn change what git shows the next, such as the diff a fresh reviewer reads. With this
// read-only file as the global configuration, git reads none of those files; the image's own system configuration
// still applies. This covers git's own files only: the rest of the home, shell startup files and the runner's state
// included, still carries over from turn to turn, so the reviewer is also given the change set as the orchestrator's
// own git reads it. Verification sandboxes keep their per-run home's configuration, which repository commands may
// set.
const runnerGitConfig = `# Written by the Octomus sandbox broker for runner sandboxes.
[core]
	attributesFile = /dev/null
	excludesFile = /dev/null
`

// container builds the one container spec a plan can produce. Every hardening choice lives here, so a golden test can
// hold it: non-root, no capabilities, no privilege escalation, a read-only image, bounded resources, no log copy of
// transcripts, and only the mounts the plan's kind needs.
func (c Config) container(p plan, extraEnv []string) ContainerConfig {
	user := fmt.Sprintf("%d:%d", c.UID, c.GID)
	entrypoint, cmd := c.program(p)
	workdir := p.dir
	if p.kind == wire.KindProbe {
		workdir = "/tmp"
	}
	env := []string{
		"HOME=" + sandboxHome,
		"USER=octomus",
		"LANG=C.UTF-8",
		"TMPDIR=/tmp",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if p.kind == wire.KindRunner {
		env = append(env, "GIT_CONFIG_GLOBAL="+toolsGitConfig)
		if c.CAFile != "" {
			env = append(env, "SSL_CERT_FILE="+toolsCAFile, "NODE_EXTRA_CA_CERTS="+toolsCAFile)
		}
	}
	env = append(env, extraEnv...)
	env = append(env, p.env...)
	mounts := c.mounts(p)
	if fixtureMounts != nil {
		fixture, fixtureEnv := fixtureMounts(p)
		mounts, env = append(mounts, fixture...), append(env, fixtureEnv...)
	}
	tmpfs := map[string]string{
		"/tmp": fmt.Sprintf("rw,exec,nosuid,nodev,size=%d", c.Tmpfs),
	}
	network := "none"
	switch p.kind {
	case wire.KindRunner:
		network = c.RunnerNetwork
	case wire.KindVerify:
		network = c.VerifyNetwork
	case wire.KindProbe:
		if p.probe == wire.ProbeContainment {
			network = c.RunnerNetwork
		}
		tmpfs[sandboxHome] = fmt.Sprintf("rw,nosuid,nodev,size=%d,uid=%d,gid=%d,mode=0700", 64<<20, c.UID, c.GID)
	}
	return ContainerConfig{
		Image:        "",
		User:         user,
		Entrypoint:   entrypoint,
		Cmd:          cmd,
		WorkingDir:   workdir,
		Env:          env,
		Labels:       map[string]string{instanceLabel: c.Instance, kindLabel: p.kind, rootLabel: p.rel},
		AttachStdin:  p.stdin,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    p.stdin,
		StdinOnce:    p.stdin,
		Tty:          false,
		StopSignal:   "SIGTERM",
		HostConfig: hostConfig{
			Init:           true,
			Privileged:     false,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			NetworkMode:    network,
			IpcMode:        "private",
			PidsLimit:      c.Pids,
			Memory:         c.Memory,
			MemorySwap:     c.Memory,
			NanoCPUs:       c.NanoCPUs,
			OomScoreAdj:    1000,
			Tmpfs:          tmpfs,
			Mounts:         mounts,
			Runtime:        c.Runtime,
			AutoRemove:     false,
			LogConfig:      logConfig{Type: "none"},
			RestartPolicy:  restartPolicy{Name: "no"},
		},
	}
}

func (c Config) program(p plan) ([]string, []string) {
	switch p.kind {
	case wire.KindRunner:
		args := wire.RunnerArgs(p.runner)
		if p.mode == wire.RunnerModeOpenCode {
			return []string{toolsBinary, wire.InitFlag, wire.RunnerModeOpenCode, strconv.FormatUint(p.readiness, 10), "--", p.runner}, args
		}
		return []string{p.runner}, args
	case wire.KindVerify:
		program, args := wire.VerifyProgram(p.command)
		return []string{program}, args
	}
	return []string{toolsBinary, wire.InitFlag, p.probe}, nil
}

func (c Config) mounts(p plan) []Mount {
	data := func(rel, target string, readOnly bool) Mount {
		return Mount{Type: "volume", Source: c.DataVolume, Target: target, ReadOnly: readOnly,
			VolumeOptions: &VolumeOptions{NoCopy: true, Subpath: rel}}
	}
	mounts := []Mount{{Type: "volume", Source: c.ToolsVolume, Target: toolsMount, ReadOnly: true,
		VolumeOptions: &VolumeOptions{NoCopy: true}}}
	if p.kind == wire.KindProbe {
		return mounts
	}
	mounts = append(mounts, data(filepath.Join(p.rel, wire.WorkspaceDir), p.dir, false))
	if !p.scratch {
		// Trusted metadata and the work tree's .git pointer both mount read-only: the pointer itself must stay the
		// CloneAt file, or a turn could plant a repository the next turn's reviewer would diff against.
		mounts = append(mounts,
			data(filepath.Join(p.rel, workspace.GitDirName), filepath.Join(p.root, workspace.GitDirName), true),
			data(filepath.Join(p.rel, wire.WorkspaceDir, ".git"), filepath.Join(p.dir, ".git"), true),
		)
	}
	switch p.kind {
	case wire.KindRunner:
		mounts = append(mounts, data(filepath.Join(p.rel, wire.RunnerHome), sandboxHome, false))
		for _, dir := range wire.RunnerHomeDirs {
			mounts = append(mounts, Mount{Type: "volume", Source: c.RunnerVolume,
				Target: filepath.Join(sandboxHome, dir.Home), VolumeOptions: &VolumeOptions{NoCopy: true, Subpath: dir.Volume}})
		}
	case wire.KindVerify:
		mounts = append(mounts, data(filepath.Join(p.rel, wire.VerifyHome), sandboxHome, false))
	}
	return mounts
}

package broker

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
)

// runnerGitConfig replaces the global git configuration of runner sandboxes. The executor, repair and reviewer turns
// of a task share one persistent home, so git's per-user files there (~/.gitconfig, ~/.config/git/config, attributes
// and ignore) would let one turn change what git shows the next, such as the diff a fresh reviewer reads. With this
// read-only file as the global configuration, nothing in the home reaches git; the image's own system configuration
// still applies. Verification sandboxes keep their per-run home's configuration, which repository commands may set.
const runnerGitConfig = `# Written by the Octomus sandbox broker for runner sandboxes.
[core]
	attributesFile = /dev/null
	excludesFile = /dev/null
`

// container builds the one container spec a plan can produce. Every hardening choice lives here, so a golden test can
// hold it: non-root, no capabilities, no privilege escalation, a read-only image, bounded resources, no log copy of
// transcripts, and only the mounts the plan's kind needs.
func (c Config) container(p plan, extraEnv []string) engineapi.ContainerConfig {
	user := fmt.Sprintf("%d:%d", c.UID, c.GID)
	entrypoint, cmd := c.program(p)
	workdir := p.dir
	if p.kind == sandbox.KindProbe {
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
	if p.kind == sandbox.KindRunner {
		env = append(env, "GIT_CONFIG_GLOBAL="+toolsGitConfig)
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
	case sandbox.KindRunner:
		network = c.RunnerNetwork
	case sandbox.KindVerify:
		network = c.VerifyNetwork
	case sandbox.KindProbe:
		if p.probe == sandbox.ProbeContainment {
			network = c.RunnerNetwork
		}
		tmpfs[sandboxHome] = fmt.Sprintf("rw,nosuid,nodev,size=%d,uid=%d,gid=%d,mode=0700", 64<<20, c.UID, c.GID)
	}
	return engineapi.ContainerConfig{
		Image:        "",
		User:         user,
		Entrypoint:   entrypoint,
		Cmd:          cmd,
		WorkingDir:   workdir,
		Env:          env,
		Labels:       map[string]string{instanceLabel: c.Instance, kindLabel: p.kind.String(), rootLabel: p.rel},
		AttachStdin:  p.stdin,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    p.stdin,
		StdinOnce:    p.stdin,
		Tty:          false,
		StopSignal:   "SIGTERM",
		HostConfig: engineapi.HostConfig{
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
			LogConfig:      engineapi.LogConfig{Type: "none"},
			RestartPolicy:  engineapi.RestartPolicy{Name: "no"},
		},
	}
}

func (c Config) program(p plan) ([]string, []string) {
	switch p.kind {
	case sandbox.KindRunner:
		args := sandbox.RunnerArgs(runnerBackend(p.runner))
		if p.mode == sandbox.RunnerModeOpenCode {
			return []string{toolsBinary, "--sandbox-init", "opencode", strconv.FormatUint(p.readiness, 10), "--", p.runner}, args
		}
		return []string{p.runner}, args
	case sandbox.KindVerify:
		return []string{"bash"}, []string{"-o", "pipefail", "-c", p.command}
	}
	return []string{toolsBinary, "--sandbox-init", p.probe}, nil
}

func (c Config) mounts(p plan) []engineapi.Mount {
	data := func(rel, target string, readOnly bool) engineapi.Mount {
		return engineapi.Mount{Type: "volume", Source: c.DataVolume, Target: target, ReadOnly: readOnly,
			VolumeOptions: &engineapi.VolumeOptions{NoCopy: true, Subpath: rel}}
	}
	mounts := []engineapi.Mount{{Type: "volume", Source: c.ToolsVolume, Target: toolsMount, ReadOnly: true,
		VolumeOptions: &engineapi.VolumeOptions{NoCopy: true}}}
	if p.kind == sandbox.KindProbe {
		return mounts
	}
	mounts = append(mounts, data(filepath.Join(p.rel, "workspace"), p.dir, false))
	if !p.scratch {
		// Trusted metadata and the work tree's .git pointer both mount read-only: the pointer itself must stay the
		// CloneAt file, or a turn could plant a repository the next turn's reviewer would diff against.
		mounts = append(mounts,
			data(filepath.Join(p.rel, "repo.git"), filepath.Join(p.root, "repo.git"), true),
			data(filepath.Join(p.rel, "workspace", ".git"), filepath.Join(p.dir, ".git"), true),
		)
	}
	switch p.kind {
	case sandbox.KindRunner:
		mounts = append(mounts, data(filepath.Join(p.rel, sandbox.RunnerHome), sandboxHome, false))
		for _, dir := range sandbox.RunnerHomeDirs {
			mounts = append(mounts, engineapi.Mount{Type: "volume", Source: c.RunnerVolume,
				Target: filepath.Join(sandboxHome, dir.Home), VolumeOptions: &engineapi.VolumeOptions{NoCopy: true, Subpath: dir.Volume}})
		}
	case sandbox.KindVerify:
		mounts = append(mounts, data(filepath.Join(p.rel, sandbox.VerifyHome), sandboxHome, false))
	}
	return mounts
}

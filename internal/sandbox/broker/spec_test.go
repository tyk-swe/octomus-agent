// The golden container specs, the hardening every sandbox shares, request validation and deployment configuration.

package broker

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

var update = flag.Bool("update", false, "rewrite the golden container specs")

const testUUID = "0b8f1c1e-2a3b-4c5d-8e9f-0123456789ab"

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Image: "octomus-sandbox:test", DataDir: t.TempDir(), DataVolume: "octomus-data", RunnerVolume: "octomus-runner",
		ToolsVolume: "octomus-tools", RunnerNetwork: "octomus-sandbox-runner", VerifyNetwork: "octomus-sandbox-verify",
		Instance: "octomus", UID: os.Getuid(), GID: os.Getgid(), ClientUID: os.Getuid(),
		NanoCPUs: 2e9, Memory: 4 << 30, Pids: 1024, Tmpfs: 1 << 30, Max: 12, MaxSeconds: 21600,
	}
}

// OwnedRoot makes an owned root at rel under cfg's data directory, with a work tree, its git metadata and dirs, as
// the control plane would, and returns the work tree.
func OwnedRoot(t *testing.T, cfg Config, rel string, dirs ...string) string {
	t.Helper()
	root := filepath.Join(cfg.DataDir, rel)
	for _, dir := range append([]string{wire.WorkspaceDir, workspace.GitDirName}, dirs...) {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, wire.WorkspaceDir, ".git"),
		[]byte("gitdir: "+filepath.Join(root, workspace.GitDirName)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, wire.WorkspaceDir)
}

// TestContainerSpecsAreGolden holds every hardening choice for each kind of sandbox. A change to a golden file is a
// change to the isolation boundary and must be reviewed as one.
func TestContainerSpecsAreGolden(t *testing.T) {
	cfg := testConfig(t)
	taskDir := OwnedRoot(t, cfg, "tasks/"+testUUID, append(wire.HomeDirs(wire.KindRunner), wire.VerifyHome)...)
	cases := map[string]wire.Request{
		"runner-codex":    {Kind: "runner", Runner: "codex", Mode: wire.RunnerModeStdio, Dir: taskDir, Stdin: true},
		"runner-opencode": {Kind: "runner", Runner: "opencode", Mode: wire.RunnerModeOpenCode, Dir: taskDir, Readiness: 60, Env: []string{"OPENCODE_SERVER_PASSWORD=pw"}},
		"verify":          {Kind: "verify", Dir: taskDir, Command: "make test"},
		"probe":           {Kind: "probe", Mode: wire.ProbeContainment},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := cfg.plan(req)
			if err != nil {
				t.Fatal(err)
			}
			// Ownership is validated against this test's uid; the spec is rendered for the deployment's sandbox user.
			deployed := cfg
			deployed.UID, deployed.GID = 10001, 10001
			spec := deployed.container(p, []string{"HTTPS_PROXY=http://sandbox:token@egress:3128"})
			data, err := json.MarshalIndent(spec, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			text := strings.ReplaceAll(string(data), cfg.DataDir, "/var/lib/octomus/data")
			golden := filepath.Join("testdata", "spec-"+name+".json")
			if *update {
				if err := os.WriteFile(golden, []byte(text+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(text) != strings.TrimSpace(string(want)) {
				t.Fatalf("container spec for %s drifted from %s; rerun with -update and review the diff:\n%s", name, golden, text)
			}
		})
	}
}

func TestEverySandboxIsHardened(t *testing.T) {
	cfg := testConfig(t)
	taskDir := OwnedRoot(t, cfg, "tasks/"+testUUID, append(wire.HomeDirs(wire.KindRunner), wire.VerifyHome)...)
	for _, req := range []wire.Request{
		{Kind: "runner", Runner: "codex", Mode: wire.RunnerModeStdio, Dir: taskDir, Stdin: true},
		{Kind: "runner", Runner: "opencode", Mode: wire.RunnerModeOpenCode, Dir: taskDir, Readiness: 60},
		{Kind: "verify", Dir: taskDir, Command: "true"},
		{Kind: "probe", Mode: wire.ProbeVersions},
		{Kind: "probe", Mode: wire.ProbeContainment},
	} {
		p, err := cfg.plan(req)
		if err != nil {
			t.Fatalf("%+v: %v", req, err)
		}
		spec := cfg.container(p, nil)
		host := spec.HostConfig
		if spec.User == "" || strings.HasPrefix(spec.User, "0:") || spec.User == "root" {
			t.Errorf("%s runs as %q", req.Kind, spec.User)
		}
		if host.Privileged || len(host.CapAdd) > 0 || len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" ||
			!host.ReadonlyRootfs || len(host.SecurityOpt) != 1 || host.SecurityOpt[0] != "no-new-privileges:true" ||
			!host.Init || host.PidMode != "" || host.UsernsMode != "" || len(host.Binds) > 0 || len(host.Devices) > 0 ||
			host.NetworkMode == "host" || host.LogConfig.Type != "none" || host.AutoRemove ||
			host.Memory != cfg.Memory || host.MemorySwap != cfg.Memory || host.PidsLimit != cfg.Pids || host.NanoCPUs != cfg.NanoCPUs || host.OomScoreAdj != 1000 {
			t.Errorf("%s/%s host config is not hardened: %+v", req.Kind, req.Mode, host)
		}
		for _, mount := range host.Mounts {
			if mount.Type != "volume" || mount.VolumeOptions == nil || !mount.VolumeOptions.NoCopy {
				t.Errorf("%s mount %+v is not a no-copy named volume", req.Kind, mount)
			}
			if mount.Source == cfg.DataVolume && (mount.VolumeOptions.Subpath == "" || strings.Contains(mount.VolumeOptions.Subpath, "..")) {
				t.Errorf("%s mounts the data volume beyond an owned root: %+v", req.Kind, mount)
			}
			if mount.Source == cfg.DataVolume && strings.HasSuffix(mount.Target, workspace.GitDirName) && !mount.ReadOnly {
				t.Errorf("%s mounts trusted git metadata writable", req.Kind)
			}
			if mount.Source == cfg.ToolsVolume && !mount.ReadOnly {
				t.Errorf("%s mounts the tools volume writable", req.Kind)
			}
			if mount.Source == cfg.RunnerVolume && p.kind != wire.KindRunner {
				t.Errorf("%s sandbox receives runner credentials", req.Kind)
			}
		}
	}
}

func TestPlanValidation(t *testing.T) {
	cfg := testConfig(t)
	taskDir := OwnedRoot(t, cfg, "tasks/"+testUUID, append(wire.HomeDirs(wire.KindRunner), wire.VerifyHome)...)
	scratch := OwnedRoot(t, cfg, "system/"+testUUID, append(wire.HomeDirs(wire.KindRunner), wire.VerifyHome)...)
	if err := os.RemoveAll(filepath.Join(filepath.Dir(scratch), workspace.GitDirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.plan(wire.Request{Kind: "runner", Runner: "codex", Mode: wire.RunnerModeStdio, Dir: scratch}); err != nil {
		t.Fatalf("a scratch root needs no git metadata: %v", err)
	}
	for _, rel := range []string{
		"cycles/" + testUUID + "/grounding", "cycles/" + testUUID + "/discovery-9", "cycles/" + testUUID + "/adversary-b",
		"cycles/" + testUUID + "/consolidation", "baselines/" + testUUID, "tasks/" + testUUID + "/verify",
	} {
		dir := OwnedRoot(t, cfg, rel, wire.VerifyHome)
		if _, err := cfg.plan(wire.Request{Kind: "verify", Dir: dir, Command: "true"}); err != nil {
			t.Errorf("owned root %s refused: %v", rel, err)
		}
	}
	refused := map[string]wire.Request{
		"unknown kind":          {Kind: "shell", Dir: taskDir},
		"relative dir":          {Kind: "verify", Dir: "tasks/" + testUUID + "/workspace", Command: "true"},
		"unclean dir":           {Kind: "verify", Dir: taskDir + "/../workspace", Command: "true"},
		"not a workspace":       {Kind: "verify", Dir: filepath.Dir(taskDir), Command: "true"},
		"data dir":              {Kind: "verify", Dir: filepath.Join(cfg.DataDir, "workspace"), Command: "true"},
		"trusted checkout":      {Kind: "verify", Dir: filepath.Join(cfg.DataDir, "checkout", "workspace"), Command: "true"},
		"non-uuid task":         {Kind: "verify", Dir: filepath.Join(cfg.DataDir, "tasks", "task-1", "workspace"), Command: "true"},
		"unknown planning role": {Kind: "verify", Dir: filepath.Join(cfg.DataDir, "cycles", testUUID, "rogue", "workspace"), Command: "true"},
		"empty command":         {Kind: "verify", Dir: taskDir},
		"long command":          {Kind: "verify", Dir: taskDir, Command: strings.Repeat("x", 4097)},
		"verify with env":       {Kind: "verify", Dir: taskDir, Command: "true", Env: []string{"OPENCODE_X=1"}},
		"verify with stdin":     {Kind: "verify", Dir: taskDir, Command: "true", Stdin: true},
		"unknown runner":        {Kind: "runner", Runner: "bash", Mode: wire.RunnerModeStdio, Dir: taskDir},
		"codex over http":       {Kind: "runner", Runner: "codex", Mode: wire.RunnerModeOpenCode, Dir: taskDir, Readiness: 5},
		"opencode over stdio":   {Kind: "runner", Runner: "opencode", Mode: wire.RunnerModeStdio, Dir: taskDir},
		"opencode unbounded":    {Kind: "runner", Runner: "opencode", Mode: wire.RunnerModeOpenCode, Dir: taskDir},
		"runner env injection":  {Kind: "runner", Runner: "codex", Mode: wire.RunnerModeStdio, Dir: taskDir, Env: []string{"LD_PRELOAD=/tmp/x.so"}},
		"runner proxy override": {Kind: "runner", Runner: "codex", Mode: wire.RunnerModeStdio, Dir: taskDir, Env: []string{"HTTPS_PROXY=http://evil"}},
		"probe with a dir":      {Kind: "probe", Mode: wire.ProbeVersions, Dir: taskDir},
		"unknown probe":         {Kind: "probe", Mode: "shell"},
	}
	for name, req := range refused {
		if _, err := cfg.plan(req); err == nil {
			t.Errorf("%s: plan accepted %+v", name, req)
		}
	}
	link := OwnedRoot(t, cfg, "baselines/"+strings.Replace(testUUID, "0b8f", "1b8f", 1), wire.VerifyHome)
	if err := os.RemoveAll(filepath.Join(filepath.Dir(link), wire.VerifyHome)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(filepath.Dir(link), wire.VerifyHome)); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.plan(wire.Request{Kind: "verify", Dir: link, Command: "true"}); err == nil {
		t.Error("a symlinked verification home was accepted")
	}
	p, err := cfg.plan(wire.Request{Kind: "verify", Dir: taskDir, Command: "true", Timeout: 999999999})
	if err != nil || p.timeout.Seconds() != float64(cfg.MaxSeconds) {
		t.Fatalf("timeout = %v, %v; want the broker's cap", p.timeout, err)
	}
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{
		"OCTOMUS_SANDBOX_IMAGE": "octomus-sandbox:1", "OCTOMUS_DATA_DIR": "/var/lib/octomus/data",
		"OCTOMUS_SANDBOX_DATA_VOLUME": "d", "OCTOMUS_SANDBOX_RUNNER_VOLUME": "r", "OCTOMUS_SANDBOX_TOOLS_VOLUME": "t",
		"OCTOMUS_SANDBOX_RUNNER_NETWORK": "rn", "OCTOMUS_SANDBOX_VERIFY_NETWORK": "vn",
	}
	getenv := func(key string) string { return env[key] }
	cfg, err := LoadConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory != 4<<30 || cfg.Pids != 1024 || cfg.Max != 12 || cfg.NanoCPUs != 2e9 || cfg.UID != 10001 {
		t.Fatalf("defaults = %+v", cfg)
	}
	for key, value := range map[string]string{
		"OCTOMUS_SANDBOX_MEMORY":   "1k",
		"OCTOMUS_SANDBOX_PIDS":     "0",
		"OCTOMUS_SANDBOX_CPUS":     "lots",
		"OCTOMUS_SANDBOX_INSTANCE": "Bad Name",
		"OCTOMUS_SANDBOX_RUNTIME":  "run sc",
		"OCTOMUS_DATA_DIR":         "relative/data",
		"OCTOMUS_SANDBOX_IMAGE":    "",
		"OCTOMUS_EGRESS_PROXY":     "egress:3128",
	} {
		previous := env[key]
		env[key] = value
		if _, err := LoadConfig(getenv); err == nil {
			t.Errorf("%s=%q accepted", key, value)
		}
		env[key] = previous
	}
	// ParseFloat accepts NaN, which every range comparison lets through.
	for _, cpus := range []string{"NaN", "nan", "-nan", "+Inf", "-Inf", "0.05", "257"} {
		env["OCTOMUS_SANDBOX_CPUS"] = cpus
		if cfg, err := LoadConfig(getenv); err == nil || !strings.Contains(err.Error(), "OCTOMUS_SANDBOX_CPUS") {
			t.Errorf("OCTOMUS_SANDBOX_CPUS=%s accepted as %d nano CPUs (%v)", cpus, cfg.NanoCPUs, err)
		}
	}
	delete(env, "OCTOMUS_SANDBOX_CPUS")
	if n, err := ParseBytes("512m"); err != nil || n != 512<<20 {
		t.Fatalf("ParseBytes(512m) = %d, %v", n, err)
	}
}

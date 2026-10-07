package engine

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

func postureFixture() (wire.BrokerInfo, Deployment) {
	return wire.BrokerInfo{
		InstanceID: "broker-instance-1", Version: "0.2.0", DockerVersion: "29.0.0", APIVersion: "1.51",
		Image: "octomus-sandbox:local", ImageID: "sha256:current", Runtime: "runsc",
		Limits:   wire.BrokerLimits{NanoCPUs: 2_000_000_000, Memory: 4 << 30, Pids: 1024, Tmpfs: 1 << 30, Max: 12, MaxSeconds: 21600},
		Networks: wire.BrokerNetworks{Runner: "runner-network", Verify: "verify-network"}, Egress: true,
		Gateway: &wire.GatewayPosture{InstanceID: "gateway-instance-1", PolicyFingerprint: "policy-1"},
	}, Deployment{
		Repository: "/srv/trusted-checkout", GitHubRepo: "fixture/project",
		Egress: map[string][]string{"model": {"api.openai.com", "chatgpt.com"}, "build": {"proxy.golang.org", "registry.npmjs.org"}},
	}
}

func TestPostureFingerprintIgnoresActivityAndHostOrder(t *testing.T) {
	t.Parallel()
	info, deployment := postureFixture()
	want, err := postureFingerprint(info, deployment)
	if err != nil {
		t.Fatal(err)
	}
	info.Live = 7
	info.Image = "another-tag-for-the-same-image"
	info.ImageDigests = []string{"sha256:registry-digest"}
	info.Runners = map[string]string{"codex": "new probe output"}
	info.RunnerErrors = map[string]string{"opencode": "version probe unavailable"}
	deployment.Egress = map[string][]string{
		"build": {"registry.npmjs.org", "proxy.golang.org"},
		"model": {"chatgpt.com", "api.openai.com", "chatgpt.com"},
	}
	got, err := postureFingerprint(info, deployment)
	if err != nil || got != want {
		t.Fatalf("unchanged boundary fingerprint = %q, %v; want %q", got, err, want)
	}
	if !reflect.DeepEqual(deployment.Egress["model"], []string{"chatgpt.com", "api.openai.com", "chatgpt.com"}) {
		t.Fatal("fingerprinting changed the deployment host list")
	}
}

func TestPostureChangesInvalidateProof(t *testing.T) {
	t.Parallel()
	changes := map[string]func(*wire.BrokerInfo, *Deployment){
		"broker instance":  func(info *wire.BrokerInfo, _ *Deployment) { info.InstanceID += "-new" },
		"broker version":   func(info *wire.BrokerInfo, _ *Deployment) { info.Version = "0.3.0" },
		"Docker version":   func(info *wire.BrokerInfo, _ *Deployment) { info.DockerVersion = "30.0.0" },
		"API version":      func(info *wire.BrokerInfo, _ *Deployment) { info.APIVersion = "1.52" },
		"image":            func(info *wire.BrokerInfo, _ *Deployment) { info.ImageID = "sha256:new" },
		"runtime":          func(info *wire.BrokerInfo, _ *Deployment) { info.Runtime = "runc" },
		"CPU limit":        func(info *wire.BrokerInfo, _ *Deployment) { info.Limits.NanoCPUs++ },
		"memory limit":     func(info *wire.BrokerInfo, _ *Deployment) { info.Limits.Memory++ },
		"process limit":    func(info *wire.BrokerInfo, _ *Deployment) { info.Limits.Pids++ },
		"tmpfs limit":      func(info *wire.BrokerInfo, _ *Deployment) { info.Limits.Tmpfs++ },
		"sandbox limit":    func(info *wire.BrokerInfo, _ *Deployment) { info.Limits.Max++ },
		"time limit":       func(info *wire.BrokerInfo, _ *Deployment) { info.Limits.MaxSeconds++ },
		"runner network":   func(info *wire.BrokerInfo, _ *Deployment) { info.Networks.Runner += "-new" },
		"verify network":   func(info *wire.BrokerInfo, _ *Deployment) { info.Networks.Verify += "-new" },
		"gateway instance": func(info *wire.BrokerInfo, _ *Deployment) { info.Gateway.InstanceID += "-new" },
		"gateway policy":   func(info *wire.BrokerInfo, _ *Deployment) { info.Gateway.PolicyFingerprint += "-new" },
		"egress enabled":   func(info *wire.BrokerInfo, _ *Deployment) { info.Egress = false },
		"model hosts":      func(_ *wire.BrokerInfo, d *Deployment) { d.Egress["model"] = []string{"other.example"} },
		"build hosts":      func(_ *wire.BrokerInfo, d *Deployment) { d.Egress["build"] = []string{"other.example"} },
		"repository":       func(_ *wire.BrokerInfo, d *Deployment) { d.GitHubRepo = "fixture/other" },
		"checkout":         func(_ *wire.BrokerInfo, d *Deployment) { d.Repository = "/srv/other-checkout" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			info, deployment := postureFixture()
			record := SandboxSelfTest{ImageID: info.ImageID, Runtime: info.Runtime, Passed: true}
			if err := record.bindPosture(info, info, deployment); err != nil || !record.matchesPosture(info, deployment) {
				t.Fatalf("initial proof did not bind: %v", err)
			}
			change(&info, &deployment)
			if record.matchesPosture(info, deployment) {
				t.Fatal("earlier containment proof still matches a changed boundary")
			}
		})
	}
}

func TestPostureBindingRequiresStableObservedBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(before, after *wire.BrokerInfo, record *SandboxSelfTest)
		valid  bool
	}{
		{"unchanged", func(_, _ *wire.BrokerInfo, _ *SandboxSelfTest) {}, true},
		{"image refreshed at admission", func(_, after *wire.BrokerInfo, record *SandboxSelfTest) {
			after.ImageID, record.ImageID = "sha256:actual-exit-image", "sha256:actual-exit-image"
		}, true},
		{"image changed after exit", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.ImageID = "sha256:later" }, false},
		{"runtime changed after exit", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.Runtime = "runc" }, false},
		{"runtime changed at admission", func(_, after *wire.BrokerInfo, record *SandboxSelfTest) {
			after.Runtime, record.Runtime = "runc", "runc"
		}, false},
		{"broker restarted", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.InstanceID += "-new" }, false},
		{"gateway restarted", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.Gateway.InstanceID += "-new" }, false},
		{"gateway policy changed", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.Gateway.PolicyFingerprint += "-new" }, false},
		{"missing initial gateway", func(before, _ *wire.BrokerInfo, _ *SandboxSelfTest) { before.Gateway = nil }, false},
		{"missing final gateway", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.Gateway = nil }, false},
		{"limits changed", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.Limits.Memory++ }, false},
		{"network changed", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.Networks.Runner += "-new" }, false},
		{"missing initial instance", func(before, _ *wire.BrokerInfo, _ *SandboxSelfTest) { before.InstanceID = "" }, false},
		{"missing final instance", func(_, after *wire.BrokerInfo, _ *SandboxSelfTest) { after.InstanceID = "" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			before, deployment := postureFixture()
			after := before
			gateway := *before.Gateway
			after.Gateway = &gateway
			record := SandboxSelfTest{ImageID: before.ImageID, Runtime: before.Runtime, Passed: true}
			test.change(&before, &after, &record)
			err := record.bindPosture(before, after, deployment)
			if !test.valid {
				if err == nil || record.PostureFingerprint != nil {
					t.Fatalf("unstable proof was bound: %+v, %v", record, err)
				}
				return
			}
			if err != nil || !record.matchesPosture(after, deployment) {
				t.Fatalf("observed proof did not bind: %+v, %v", record, err)
			}
			if before.ImageID != after.ImageID && record.matchesPosture(before, deployment) {
				t.Fatal("probe proof was attributed to the image before admission")
			}
		})
	}
}

func TestLegacyAndUnidentifiedPosturesAreNotCurrent(t *testing.T) {
	t.Parallel()
	info, deployment := postureFixture()
	record := SandboxSelfTest{ImageID: info.ImageID, Runtime: info.Runtime, Passed: true}
	if record.matchesPosture(info, deployment) {
		t.Fatal("legacy image-only proof is current")
	}
	if err := record.bindPosture(info, info, deployment); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{"instance", "image", "gateway", "gateway instance", "gateway policy"} {
		unknown := info
		gateway := *info.Gateway
		unknown.Gateway = &gateway
		switch missing {
		case "instance":
			unknown.InstanceID = ""
		case "image":
			unknown.ImageID = ""
		case "gateway":
			unknown.Gateway = nil
		case "gateway instance":
			unknown.Gateway.InstanceID = ""
		case "gateway policy":
			unknown.Gateway.PolicyFingerprint = ""
		}
		if _, err := postureFingerprint(unknown, deployment); err == nil || record.matchesPosture(unknown, deployment) {
			t.Fatalf("missing %s identity accepted", missing)
		}
	}
	info.Egress, info.Gateway = false, nil
	if _, err := postureFingerprint(info, deployment); err != nil {
		t.Fatalf("disabled egress requires an unused gateway: %v", err)
	}
}

// Read only the synthetic seed and its pure encoder, without starting the browser fixture service.
func TestBrowserPostureFingerprint(t *testing.T) {
	t.Parallel()
	const script = `import ast, hashlib, json, sys
source = ast.parse(open(sys.argv[1]).read())
nodes = [node for node in ast.walk(source) if
    isinstance(node, ast.FunctionDef) and node.name == 'posture_fingerprint' or
    isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id in ('broker_info', 'egress_hosts') for target in node.targets)]
values = {'hashlib': hashlib, 'json': json}
exec(compile(ast.Module(body=nodes, type_ignores=[]), sys.argv[1], 'exec'), values)
print(json.dumps({'info': values['broker_info'], 'hosts': values['egress_hosts'], 'fingerprint': values['posture_fingerprint'](values['broker_info'], values['egress_hosts'])}))
`
	data, err := exec.Command("python3", "-c", script, filepath.Join("..", "..", "tests", "serve_ui.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic posture: %v: %s", err, data)
	}
	var seed struct {
		Info        wire.BrokerInfo     `json:"info"`
		Hosts       map[string][]string `json:"hosts"`
		Fingerprint string              `json:"fingerprint"`
	}
	if err := json.Unmarshal(data, &seed); err != nil {
		t.Fatal(err)
	}
	want, err := postureFingerprint(seed.Info, Deployment{Egress: seed.Hosts})
	if err != nil || seed.Fingerprint != want {
		t.Fatalf("synthetic proof fingerprint = %q, want %q; %v", seed.Fingerprint, want, err)
	}
}

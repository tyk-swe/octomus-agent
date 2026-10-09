// This driver runs inside the unchanged production control-plane image. The broker, gateway, installed helper and
// runner executables are the shipped binaries; only the provider and this test driver are fixtures.
package productionimage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

// The fixture supplies its complete model catalog; never fetch an unrelated public catalog in this test.
type contractBackend struct{ *sandbox.Remote }

func (b contractBackend) StartOpenCode(ctx context.Context, spec sandbox.Spec, seconds uint64) (*sandbox.OpenCodeServer, error) {
	spec.Env = append(spec.Env, "OPENCODE_DISABLE_MODELS_FETCH=true")
	return b.Remote.StartOpenCode(ctx, spec, seconds)
}

func eventually(t *testing.T, seconds time.Duration, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(seconds)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(description)
}

func TestProductionImageContracts(t *testing.T) {
	if os.Getenv("OCTOMUS_PRODUCTION_IMAGE_TEST") != "1" {
		t.Skip("run make test-production-images with the production OCI archives and Docker Engine 28+")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	box := contractBackend{sandbox.NewRemote("/run/octomus/sandboxd.sock")}
	info, err := box.RefreshInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expectedImage := os.Getenv("OCTOMUS_EXPECT_SANDBOX_ID")
	if expectedImage == "" || info.ImageID != expectedImage || !info.Egress || info.Gateway == nil {
		t.Fatalf("unexpected production broker identity or missing gateway: %+v", info)
	}
	for backend, expected := range map[string]string{"codex": "codex-cli " + runner.CodexVersion, "opencode": runner.OpenCodeVersion} {
		if info.Runners[backend] != expected || info.RunnerErrors[backend] != "" {
			t.Fatalf("production image %s: version %q, error %q; expected %q", backend, info.Runners[backend], info.RunnerErrors[backend], expected)
		}
	}
	probe, err := sandbox.Probe(ctx, box.Remote)
	if err != nil || !probe.Passed() || probe.Sandbox == nil || probe.Sandbox.ImageID != expectedImage ||
		probe.Sandbox.Incomplete || probe.Sandbox.OOM || probe.Sandbox.Runs != 1 {
		t.Fatalf("production image containment: %+v (%v)", probe, err)
	}
	records := map[string][]*model.SandboxRecord{}
	inspect := func(t *testing.T, backend string, run int) {
		t.Helper()
		request := fmt.Sprintf("inspect-%s-%d", backend, run)
		if err := os.WriteFile("/results/"+request, []byte("inspect the active runner container\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, "host did not inspect the production runner container", func() bool {
			_, err := os.Stat("/results/" + request + "-complete")
			return err == nil
		})
	}
	for _, backend := range []config.Backend{config.BackendCodex, config.BackendOpencode} {
		t.Run(string(backend), func(t *testing.T) {
			workspace := filepath.Join("/var/lib/octomus/data/system", uuid.NewString(), "workspace")
			if err := os.MkdirAll(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.SessionTimeoutSeconds, cfg.CommandTimeoutSeconds = 60, 60
			route := config.NewRoute("gpt-6-astra", "low")
			if backend == config.BackendOpencode {
				provider, variant := "contract", "high"
				route = config.Route{Backend: backend, Provider: &provider, Model: "contract-model", Variant: &variant}
			}
			owner, stop := context.WithCancel(ctx)
			defer stop()
			connect := func() runner.Adapter {
				client, err := runner.Connect(owner, backend, cfg, workspace, "image-contract", func(string) error { return nil }, box)
				if err != nil {
					t.Fatalf("connect through production broker: %v", err)
				}
				t.Cleanup(func() { _ = client.Close() })
				return client
			}
			finish := func(client runner.Adapter) {
				if err := client.Close(); err != nil {
					t.Fatalf("production runner cleanup: %v", err)
				}
				evidence := client.(interface{ SandboxEvidence() *model.SandboxRecord }).SandboxEvidence()
				if evidence == nil || evidence.ImageID != expectedImage || evidence.Incomplete || evidence.OOM || evidence.Runs != 1 ||
					evidence.Egress.Allowed["provider.octomus.test:443"] == 0 {
					t.Fatalf("missing exact-image or actual provider CONNECT evidence: %+v", evidence)
				}
				records[string(backend)] = append(records[string(backend)], evidence)
			}
			client := connect()
			session, err := client.Start(route, workspace, nil)
			if err != nil {
				t.Fatal(err)
			}
			inspect(t, string(backend), 1)
			answer, err := client.Turn(session, route, workspace, "Return the controlled contract result.", nil, nil)
			if err != nil || !strings.Contains(answer, "Controlled contract result") {
				t.Fatalf("production client completed turn: %q (%v)", answer, err)
			}
			structured, err := client.Turn(session, route, workspace, "Return the controlled review result.", schemas.ReviewSchema(), nil)
			if err != nil {
				t.Fatalf("production client structured turn: %v", err)
			}
			if _, err := runner.FinishTurn(structured, schemas.ReviewSchema()); err != nil {
				t.Fatal(err)
			}
			finish(client)
			client = connect()
			resumed, err := client.Start(route, workspace, &session)
			if err != nil || resumed != session {
				t.Fatalf("production image lost session across containers: %q (%v)", resumed, err)
			}
			inspect(t, string(backend), 2)
			done := make(chan error, 1)
			go func() {
				_, err := client.Turn(session, route, workspace, "CANCEL_CONTRACT_TURN", nil, nil)
				done <- err
			}()
			eventually(t, 30*time.Second, "cancellation turn did not reach the TLS provider through egress", func() bool {
				_, err := os.Stat("/provider-status/turn-entered-" + string(backend))
				return err == nil
			})
			stop()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled turn produced successful evidence")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("production client did not acknowledge cancellation")
			}
			finish(client)
		})
	}
	if t.Failed() {
		return
	}
	workspace := filepath.Join("/var/lib/octomus/data/system", uuid.NewString(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	command := `python3 -c 'import errno,os,socket; assert os.getuid() == 10001; assert os.statvfs("/").f_flag & os.ST_RDONLY; assert all(not os.path.exists(p) for p in ["/var/run/docker.sock", "/run/secrets/github_token", "/contract-driver", "/provider.py", "/provider.pem", "/provider.key", "/provider-status", "/results"]); s=socket.socket(); s.settimeout(2); assert s.connect_ex(("11.255.254.2",443)) not in (0,errno.ECONNREFUSED,errno.ECONNRESET); s.close(); open("verified.txt", "w").write("production image")'`
	out, verification, err := sandbox.Verify(ctx, box, workspace, command, 30, true)
	if err != nil || out == nil || !out.Status.Success() || verification == nil || verification.ImageID != expectedImage ||
		verification.Incomplete || verification.OOM || verification.Runs != 1 {
		t.Fatalf("production image verification sandbox: %+v (%v)", verification, err)
	}
	eventually(t, 10*time.Second, "broker still reports live sandboxes after cleanup", func() bool {
		info, err := box.RefreshInfo(ctx)
		return err == nil && info.Live == 0
	})
	result := map[string]any{"passed": true, "runners": info.Runners, "contracts": records,
		"containment": probe, "containment_sandbox": probe.Sandbox, "verification": verification, "broker_live": 0}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/results/contract.json", data, 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Println("PASS: exact production image, both real clients, structured turns, cross-container resume, cancellation, verification and containment")
}

package broker

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestRebuiltImageTakesEffectAndAVanishedOneFailsClearly(t *testing.T) {
	e := newFakeEngine(t)
	cfg := testConfig(t)
	b := e.broker(t, cfg)
	remote := serve(t, b)
	run := func() error {
		child, err := remote.Start(context.Background(), versionsProbe)
		if err != nil {
			return err
		}
		go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
		_, err = child.Wait()
		return err
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if image := e.Created()[0].Spec.Image; image != "sha256:first" {
		t.Fatalf("first sandbox ran %s", image)
	}
	// The operator rebuilds the image under the same tag; nothing restarts the broker.
	e.mu.Lock()
	e.images[cfg.Image] = "sha256:second"
	e.mu.Unlock()
	if err := run(); err != nil {
		t.Fatal(err)
	}
	created := e.Created()
	if image := created[len(created)-1].Spec.Image; image != "sha256:second" {
		t.Fatalf("sandbox after a rebuild ran %s; want the image the tag names now", image)
	}
	if info := b.Info(); info.ImageID != "sha256:second" || info.Runners["codex"] != "codex-fake sha256:second" {
		t.Fatalf("broker info after a rebuild = %s with runners %v; want the new image probed", info.ImageID, info.Runners)
	}
	// The tag is removed, and the old image pruned.
	e.mu.Lock()
	delete(e.images, cfg.Image)
	e.mu.Unlock()
	if err := run(); err == nil || !strings.Contains(err.Error(), "not available locally") {
		t.Fatalf("sandbox after the tag vanished = %v; want a clear refusal", err)
	}
}

func TestContainmentProbeNamesTheImageItRanOn(t *testing.T) {
	e := newFakeEngine(t)
	e.run = func(c *fakeContainer) {
		if !slices.Contains(c.Spec.Entrypoint, wire.ProbeContainment) {
			defaultRun(c)
			return
		}
		c.Stdout(`{"checks":[{"id":"non_root","label":"Runs as an unprivileged user","passed":true,"detail":"uid 10001"}],"kernel":"6.1"}`)
		c.End(0)
	}
	cfg := testConfig(t)
	remote := serve(t, e.broker(t, cfg))
	// The broker's info is read, and cached, before the operator rebuilds the image under the same tag.
	if info, err := remote.Info(context.Background()); err != nil || info.ImageID != "sha256:first" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	e.mu.Lock()
	e.images[cfg.Image] = "sha256:second"
	e.mu.Unlock()
	report, err := sandbox.Probe(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	// The probe's own request moved the broker to the rebuilt image, so only its record names what it proved.
	if report.Sandbox == nil || report.Sandbox.ImageID != "sha256:second" {
		t.Fatalf("probe sandbox record = %+v; want the rebuilt image it ran on", report.Sandbox)
	}
	// The posture the dashboard compares that proof with is read again, not served from before the rebuild.
	if info, err := remote.Info(context.Background()); err != nil || info.ImageID != "sha256:second" {
		t.Fatalf("info after the probe = %s, %v; want the image the probe moved the broker to", info.ImageID, err)
	}
}

func TestRunnerWhoseVersionFailsIsNotReportedAsMissing(t *testing.T) {
	e := newFakeEngine(t)
	e.run = func(c *fakeContainer) {
		if c.Spec.Image != "sha256:second" {
			defaultRun(c)
			return
		}
		// OpenCode is installed but hangs on its first run; the probe itself succeeds.
		c.Stdout(`{"codex":"codex-fake"}`)
		c.Stderr("opencode --version failed: timed out after 30s\n")
		c.End(0)
	}
	cfg := testConfig(t)
	log := &testutil.SyncBuffer{}
	cfg.Log = log
	b := e.broker(t, cfg)
	remote := serve(t, b)
	e.mu.Lock()
	e.images[cfg.Image] = "sha256:second"
	e.mu.Unlock()
	if _, err := b.image(context.Background()); err != nil {
		t.Fatal(err)
	}
	if info := b.Info(); info.Runners["codex"] != "codex-fake" || !strings.Contains(info.RunnerErrors["opencode"], "timed out after 30s") {
		t.Fatalf("broker info = runners %v, errors %v; want OpenCode's failure kept", info.Runners, info.RunnerErrors)
	}
	if !strings.Contains(log.String(), "opencode") || !strings.Contains(log.String(), "timed out after 30s") {
		t.Fatalf("broker log = %q; want the failed runner named", log.String())
	}
	_, err := remote.RunnerVersion(context.Background(), sandbox.Spec{Runner: config.BackendOpencode}, 10)
	if err == nil || strings.Contains(err.Error(), "not installed") || !strings.Contains(err.Error(), "--version failed") ||
		!strings.Contains(err.Error(), "timed out after 30s") {
		t.Fatalf("OpenCode version = %v; want its failure, not a missing runner", err)
	}
	if _, err := remote.RunnerVersion(context.Background(), sandbox.Spec{Runner: config.BackendCodex}, 10); err != nil {
		t.Fatalf("Codex version = %v", err)
	}
}

func TestFailedProbeOfARebuiltImageIsNotRepeatedForEveryRequest(t *testing.T) {
	e := newFakeEngine(t)
	e.run = func(c *fakeContainer) {
		if c.Spec.Image == "sha256:broken" {
			c.Stderr("codex: not found")
			c.End(127)
			return
		}
		defaultRun(c)
	}
	cfg := testConfig(t)
	b := e.broker(t, cfg)
	// The operator rebuilds the image under the same tag, without its runners.
	e.mu.Lock()
	e.images[cfg.Image] = "sha256:broken"
	e.mu.Unlock()
	for range 3 {
		if _, err := b.image(context.Background()); err == nil || !strings.Contains(err.Error(), "codex: not found") {
			t.Fatalf("sandbox on an image whose probe fails = %v; want the probe failure", err)
		}
	}
	if created := len(e.Created()); created != 1 {
		t.Fatalf("three requests ran %d version probes; want one, remembered", created)
	}
	if info := b.Info(); info.ImageID != "sha256:first" {
		t.Fatalf("broker info after a failed probe = %s; want the image it last probed", info.ImageID)
	}
}

func TestImageInspectionFailureIsNotReportedAsAMissingImage(t *testing.T) {
	e := newFakeEngine(t)
	b := e.broker(t, testConfig(t))
	e.mu.Lock()
	e.imageStatus = http.StatusInternalServerError
	e.mu.Unlock()
	if _, err := b.image(context.Background()); err == nil || strings.Contains(err.Error(), "not available locally") ||
		!strings.Contains(err.Error(), "Inspecting sandbox image") {
		t.Fatalf("image inspection that failed = %v; want it reported as a failed inspection", err)
	}
}

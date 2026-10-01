package broker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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

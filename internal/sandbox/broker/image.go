package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

type imageFailure struct {
	id  string
	err error
	at  time.Time
}

// probeRetry is how long a rebuilt image whose runner versions could not be probed is refused without probing it
// again.
const probeRetry = 30 * time.Second

// inspectImage resolves the configured tag. Only an image the daemon does not have is reported as missing.
func (b *Broker) inspectImage(ctx context.Context) (engineImage, error) {
	image, err := b.engine.imageInspect(ctx, b.cfg.Image)
	switch {
	case notFound(err):
		return image, fmt.Errorf("Sandbox image %s is not available locally (the broker never pulls): %w", b.cfg.Image, err)
	case err != nil:
		return image, fmt.Errorf("Inspecting sandbox image %s: %w", b.cfg.Image, err)
	}
	return image, nil
}

// image resolves the configured tag for a new sandbox. An image rebuilt under the same tag takes effect for the next
// sandbox, with its runner versions probed again; a tag that no longer resolves fails clearly, rather than leaving
// sandboxes on an image that may already be pruned. A rebuilt image whose probe failed is refused for probeRetry.
func (b *Broker) image(ctx context.Context) (string, error) {
	image, err := b.inspectImage(ctx)
	if err != nil {
		return "", err
	}
	current := func() string {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.info.ImageID
	}
	if image.ID == current() {
		return image.ID, nil
	}
	select {
	case b.refresh <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-b.refresh }()
	if image.ID == current() {
		return image.ID, nil
	}
	if b.failed.id == image.ID && time.Since(b.failed.at) < probeRetry {
		return "", b.failed.err
	}
	versions, failures, err := b.probeVersions(ctx, image.ID)
	if err != nil {
		err = fmt.Errorf("Probing runner versions in the rebuilt sandbox image %s: %w", b.cfg.Image, err)
		// A request that gave up says nothing about the image.
		if ctx.Err() == nil {
			b.failed = imageFailure{id: image.ID, err: err, at: time.Now()}
		}
		return "", err
	}
	b.mu.Lock()
	b.info.ImageID, b.info.ImageDigests, b.info.Runners, b.info.RunnerErrors = image.ID, image.RepoDigests, versions, failures
	b.mu.Unlock()
	b.logf("Sandbox image %s now resolves to %s", b.cfg.Image, image.ID)
	return image.ID, nil
}

// probeVersions runs the version probe on image. It returns each installed runner's version, and why each runner that
// is installed but did not answer failed (nil when none did), so the broker never reports that one as missing.
func (b *Broker) probeVersions(ctx context.Context, image string) (map[string]string, map[string]string, error) {
	p, err := b.cfg.plan(wire.Request{Kind: wire.KindProbe, Mode: wire.ProbeVersions, Timeout: 120})
	if err != nil {
		return nil, nil, err
	}
	p.image = image
	var stdout, stderr bytes.Buffer
	collect := func(buf *bytes.Buffer) func([]byte) error {
		return func(p []byte) error {
			if buf.Len()+len(p) > 64<<10 {
				return errors.New("version probe output is too large")
			}
			buf.Write(p)
			return nil
		}
	}
	report, err := b.runSandbox(ctx, p, collect(&stdout), collect(&stderr), nil)
	if err != nil {
		return nil, nil, err
	}
	if report.Error != "" && !report.Killed {
		return nil, nil, fmt.Errorf("version probe failed: %s", report.Error)
	}
	if report.Code != 0 || report.Killed || report.OOM {
		return nil, nil, fmt.Errorf("version probe failed with exit %d: %s", report.Code, strings.TrimSpace(stderr.String()))
	}
	versions := map[string]string{}
	if err := json.Unmarshal(stdout.Bytes(), &versions); err != nil {
		return nil, nil, fmt.Errorf("version probe output: %w", err)
	}
	var failures map[string]string
	for _, line := range strings.Split(stderr.String(), "\n") {
		for _, name := range wire.Runners {
			if reason, ok := strings.CutPrefix(line, name+wire.VersionFailed); ok && versions[name] == "" {
				if failures == nil {
					failures = map[string]string{}
				}
				failures[name] = redact.Text(strings.ToValidUTF8(reason, "�"))
				b.logf("Runner %s is in sandbox image %s but its --version failed: %s", name, image, failures[name])
			}
		}
	}
	return versions, failures, nil
}

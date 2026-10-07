package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

type probeTransport func(*http.Request) (*http.Response, error)

func (f probeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func probeRequest(ctx context.Context) *http.Request {
	r := httptest.NewRequest(http.MethodPost, wire.SandboxesPath, strings.NewReader(`{"kind":"probe","mode":"versions"}`))
	r.Header.Set("Upgrade", wire.UpgradeProtocol)
	return r.WithContext(ctx)
}

// Exercise the actual image-refresh request, prepare, failed removal and reaper with a scripted HTTP transport.
// Refusing after create avoids requiring an attach socket; synctest advances the full production teardown budget.
func TestImageProbeRetainsCapacityUntilRemoval(t *testing.T) {
	for _, cancelCreate := range []bool{false, true} {
		name := "daemon warning"
		if cancelCreate {
			name = "request cancelled during create"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Max, cfg.Log = 1, io.Discard
			synctest.Test(t, func(t *testing.T) {
				b := newBroker(cfg)
				defer close(b.closing)
				b.info.ImageID = "sha256:first"
				var image atomic.Value
				image.Store("sha256:second")
				var removable atomic.Bool
				var creates atomic.Int64
				creating := make(chan context.Context, 1)
				finishCreate := make(chan struct{})
				b.engine.http = &http.Client{Transport: probeTransport(func(r *http.Request) (*http.Response, error) {
					status, body := http.StatusOK, ""
					switch {
					case strings.Contains(r.URL.Path, "/images/"):
						body = `{"Id":"` + image.Load().(string) + `"}`
					case strings.HasSuffix(r.URL.Path, "/containers/create"):
						creates.Add(1)
						creating <- r.Context()
						if cancelCreate {
							<-finishCreate
							body = `{"Id":"probe"}`
						} else {
							body = `{"Id":"probe","Warnings":["limit unavailable"]}`
						}
					case r.Method == http.MethodDelete:
						if !removable.Load() {
							status, body = http.StatusInternalServerError, `{"message":"removal unavailable"}`
						}
					default:
						t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
						status = http.StatusInternalServerError
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				})}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				response := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					b.handleSandbox(context.Background(), response, probeRequest(ctx))
				}()
				createCtx := <-creating
				if cancelCreate {
					cancel()
					synctest.Wait()
					if createCtx.Err() != nil {
						t.Fatal("client cancellation abandoned the daemon create")
					}
					close(finishCreate)
				}
				synctest.Wait()
				if len(b.slots) != 1 || b.Info().Live != 1 {
					t.Fatalf("image probe lost ownership during teardown: slots %d, live %d", len(b.slots), b.Info().Live)
				}
				time.Sleep(teardownBudget + time.Second)
				<-done
				if response.Code != http.StatusInternalServerError || len(b.slots) != 1 || b.Info().Live != 1 {
					t.Fatalf("failed probe released its slot: status %d, slots %d, live %d", response.Code, len(b.slots), b.Info().Live)
				}
				// Switching back to the cached image must not bypass the retained probe's capacity.
				image.Store("sha256:first")
				waiting, cancelWaiting := context.WithTimeout(context.Background(), time.Second)
				defer cancelWaiting()
				b.handleSandbox(context.Background(), httptest.NewRecorder(), probeRequest(waiting))
				if creates.Load() != 1 || len(b.slots) != 1 {
					t.Fatal("another sandbox was admitted while the probe remained unremoved")
				}
				removable.Store(true)
				time.Sleep(reapDelay)
				synctest.Wait()
				if len(b.slots) != 0 || b.Info().Live != 0 {
					t.Fatal("confirmed probe removal did not return its capacity")
				}
			})
		})
	}
}

func TestImageRefreshWithSingleSlot(t *testing.T) {
	e := newFakeEngine(t)
	cfg := testConfig(t)
	cfg.Max = 1
	b := e.broker(t, cfg)
	e.mu.Lock()
	e.images[cfg.Image] = "sha256:rebuilt"
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response := httptest.NewRecorder()
	// The recorder cannot hijack, so the requested container is discarded after its image probe succeeds.
	b.handleSandbox(ctx, response, probeRequest(ctx))
	created := e.Created()
	if len(created) != 2 || b.Info().ImageID != "sha256:rebuilt" {
		t.Fatalf("Max=1 image refresh did not reach request preparation: %d creates, response %s", len(created), response.Body)
	}
	for _, c := range created {
		if c.Spec.Image != "sha256:rebuilt" {
			encoded, _ := json.Marshal(c.Spec)
			t.Fatalf("image refresh used an unprobed image: %s", encoded)
		}
	}
	if len(b.slots) != 0 || len(e.Remaining()) != 0 {
		t.Fatal("successful probe or discarded request retained capacity")
	}
}

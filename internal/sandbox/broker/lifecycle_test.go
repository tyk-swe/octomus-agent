package broker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// The Engine's HTTP responses and attach pipe are entirely local. In particular a /start handler can continue
// running while its response is withheld, without a socket, Docker daemon or wall-clock teardown delay.
type lifecycleAttach struct {
	io.Writer
	io.Closer
}

func (lifecycleAttach) CloseWrite() error { return nil }

func lifecycleResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func lifecyclePrepared(t *testing.T, b *Broker) *prepared {
	t.Helper()
	release, err := b.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	s := &prepared{b: b, id: "pending-start", name: "pending-start", image: "sha256:fixture", release: release,
		attach: &attachStream{conn: lifecycleAttach{io.Discard, w}, reader: bufio.NewReader(r)}}
	if b.leases != nil {
		s.lease, err = b.leases.Grant(s.name, wire.KindRunner)
		if err != nil {
			t.Fatal(err)
		}
	}
	b.live[s.id] = struct{}{}
	return s
}

func TestPendingStartRemainsBoundedAndControllable(t *testing.T) {
	for _, cause := range []string{"lifetime", "kill", "disconnect", "closed controls", "kill then disconnect"} {
		t.Run(cause, func(t *testing.T) {
			leaseDir := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				b := newBroker(Config{Max: 1, LeaseDir: leaseDir, Log: io.Discard})
				defer close(b.closing)
				startSeen := make(chan context.Context, 1)
				killSeen := make(chan struct{}, 1)
				var removed, killed atomic.Bool
				b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/start"):
						startSeen <- r.Context()
						<-r.Context().Done()
						return nil, r.Context().Err()
					case strings.HasSuffix(r.URL.Path, "/kill"):
						killed.Store(true)
						if cause == "kill then disconnect" {
							killSeen <- struct{}{}
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
						return lifecycleResponse(http.StatusConflict, `{"message":"not running yet"}`), nil
					case strings.HasSuffix(r.URL.Path, "/json"):
						return lifecycleResponse(http.StatusOK, `{"State":{"OOMKilled":false}}`), nil
					case r.Method == http.MethodDelete:
						if r.Context().Err() != nil || r.URL.Query().Get("force") != "1" || !strings.HasSuffix(r.URL.Path, "/pending-start") {
							t.Error("uncertain start was not force-removed by ID under an independent live context")
						}
						leases, err := os.ReadDir(leaseDir)
						if err != nil || len(leases) != 0 {
							t.Error("uncertain start retained egress during removal")
						}
						removed.Store(true)
						return lifecycleResponse(http.StatusNoContent, ""), nil
					default:
						t.Errorf("unconfirmed start must not be waited as an exited command: %s", r.URL.Path)
						return lifecycleResponse(http.StatusInternalServerError, ""), nil
					}
				})
				s := lifecyclePrepared(t, b)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				controls := make(chan control)
				done := make(chan struct{})
				var report wire.ExitReport
				var failure error
				began := time.Now()
				timeout := time.Hour
				go func() {
					defer close(done)
					report, failure = s.execute(ctx, timeout, output{stdout: discard, stderr: discard}, controls)
				}()
				startCtx := <-startSeen
				deadline, bounded := startCtx.Deadline()
				if !bounded || !deadline.Equal(began.Add(timeout)) {
					t.Fatalf("start request deadline = %v, %v; want the absolute hard lifetime", deadline, bounded)
				}
				switch cause {
				case "lifetime":
					time.Sleep(timeout)
				case "kill":
					select {
					case controls <- control{signal: wire.SignalKill}:
					case <-time.After(time.Second):
						t.Fatal("pending start blocked kill delivery")
					}
				case "disconnect":
					cancel()
				case "closed controls":
					close(controls)
				case "kill then disconnect":
					controls <- control{signal: wire.SignalKill}
					<-killSeen
					cancel()
				}
				select {
				case <-done:
				case <-time.After(teardownBudget + time.Second):
					t.Fatal("pending start prevented bounded teardown")
				}
				if startCtx.Err() == nil || !removed.Load() || len(b.slots) != 0 || b.Info().Live != 0 {
					t.Fatalf("pending start escaped cancellation or confirmed removal: start %v, removed %v, slots %d, live %d",
						startCtx.Err(), removed.Load(), len(b.slots), b.Info().Live)
				}
				if report.Sandbox == nil || !report.Sandbox.Incomplete || report.Sandbox.Runs != 1 {
					t.Fatalf("uncertain run lost its incomplete evidence: %+v", report)
				}
				if cause == "lifetime" || cause == "kill" {
					if failure == nil || report.Killed || !strings.Contains(failure.Error(), "Starting the sandbox") {
						t.Fatalf("unconfirmed start invented a command result: %+v, %v", report, failure)
					}
				} else if failure != nil || report.Error != streamClosed {
					t.Fatalf("closed lifeline = %+v, %v", report, failure)
				}
				if cause == "kill" && !killed.Load() {
					t.Fatal("pending start did not receive the kill request")
				}
			})
		})
	}
}

func TestStartupConsumesTheRunningLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBroker(Config{Max: 1, Log: io.Discard})
		began := time.Now()
		killed := make(chan struct{})
		b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/start"):
				time.Sleep(4 * time.Second)
				return lifecycleResponse(http.StatusNoContent, ""), nil
			case strings.HasSuffix(r.URL.Path, "/wait"):
				<-killed
				return lifecycleResponse(http.StatusOK, `{"StatusCode":137}`), nil
			case strings.HasSuffix(r.URL.Path, "/kill"):
				if elapsed := time.Since(began); elapsed != 10*time.Second {
					t.Errorf("kill after %v; startup must consume the 10-second lifetime", elapsed)
				}
				close(killed)
				return lifecycleResponse(http.StatusNoContent, ""), nil
			default:
				t.Fatalf("unexpected Engine request: %s", r.URL.Path)
				return nil, io.ErrUnexpectedEOF
			}
		})
		s := &prepared{b: b, id: "late-start"}
		end := s.run(context.Background(), 10*time.Second, nil, nil)
		if end.err != nil || !end.started || !end.killed || end.reason != wire.TimeLimitReason || end.result == nil || end.result.StatusCode != 137 {
			t.Fatalf("late start result = %+v", end)
		}
	})
}

func TestKillBeforeStartIsAppliedAfterStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBroker(Config{Max: 1, Log: io.Discard})
		starting, allowStart, firstKill, killed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var kills atomic.Int64
		b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/start"):
				close(starting)
				<-allowStart
				return lifecycleResponse(http.StatusNoContent, ""), nil
			case strings.HasSuffix(r.URL.Path, "/kill"):
				if kills.Add(1) == 1 {
					close(firstKill)
					return lifecycleResponse(http.StatusConflict, `{"message":"not running yet"}`), nil
				}
				close(killed)
				return lifecycleResponse(http.StatusNoContent, ""), nil
			case strings.HasSuffix(r.URL.Path, "/wait"):
				<-killed
				return lifecycleResponse(http.StatusOK, `{"StatusCode":137}`), nil
			default:
				return nil, io.ErrUnexpectedEOF
			}
		})
		controls := make(chan control)
		done := make(chan ending, 1)
		go func() {
			done <- (&prepared{b: b, id: "late-start"}).run(context.Background(), time.Hour, controls, nil)
		}()
		<-starting
		controls <- control{signal: wire.SignalKill}
		<-firstKill
		close(allowStart)
		end := <-done
		if end.err != nil || !end.killed || end.result == nil || end.result.StatusCode != 137 || kills.Load() != 2 {
			t.Fatalf("a late start escaped an earlier kill: %+v, %d kills", end, kills.Load())
		}
	})
}

func TestUnknownStartRetainsCapacityUntilRemoval(t *testing.T) {
	leaseDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		b := newBroker(Config{Max: 1, LeaseDir: leaseDir, Log: io.Discard})
		defer close(b.closing)
		var removable atomic.Bool
		b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/start"):
				// Docker may have started the process before this response was lost.
				return nil, io.ErrUnexpectedEOF
			case strings.HasSuffix(r.URL.Path, "/json"):
				return lifecycleResponse(http.StatusOK, `{"State":{"OOMKilled":false}}`), nil
			case r.Method == http.MethodDelete:
				if removable.Load() {
					return lifecycleResponse(http.StatusNoContent, ""), nil
				}
				return lifecycleResponse(http.StatusInternalServerError, `{"message":"daemon is busy"}`), nil
			default:
				t.Errorf("unexpected Engine request after unknown start: %s", r.URL.Path)
				return nil, io.ErrUnexpectedEOF
			}
		})
		s := lifecyclePrepared(t, b)
		report, err := s.execute(context.Background(), time.Minute, output{stdout: discard, stderr: discard}, nil)
		if err == nil || !strings.Contains(err.Error(), "removing the sandbox also failed") || report.Sandbox == nil || !report.Sandbox.Incomplete {
			t.Fatalf("failed unknown-start teardown = %+v, %v", report, err)
		}
		if len(b.slots) != 1 || b.Info().Live != 1 {
			t.Fatal("unknown-start removal failure returned live capacity")
		}
		waiting, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := b.acquire(waiting); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("admission bypassed the potentially running container: %v", err)
		}
		leases, err := os.ReadDir(leaseDir)
		if err != nil || len(leases) != 0 {
			t.Fatalf("unknown start retained egress while awaiting removal: %v, %v", leases, err)
		}
		removable.Store(true)
		time.Sleep(reapDelay)
		synctest.Wait()
		if len(b.slots) != 0 || b.Info().Live != 0 {
			t.Fatal("confirmed removal did not return capacity")
		}
	})
}

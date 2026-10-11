package broker

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// obstructLease makes a removal failure independent of the test user's filesystem privileges.
func obstructLease(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one granted lease: %v, %v", entries, err)
	}
	file := filepath.Join(dir, entries[0].Name())
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, "obstruction"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCleanupRetriesLeaseIndependentlyOfContainer(t *testing.T) {
	for _, failRemoval := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease only", true: "lease and container"}[failRemoval], func(t *testing.T) {
			leaseDir := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				var log testutil.SyncBuffer
				b := newBroker(Config{Max: 1, LeaseDir: leaseDir, Log: &log})
				defer close(b.closing)
				var removable atomic.Bool
				removable.Store(!failRemoval)
				var attempts atomic.Int64
				b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodDelete {
						t.Fatalf("cleanup attempted an unexpected Docker operation: %s", r.Method)
					}
					attempts.Add(1)
					if !removable.Load() {
						return lifecycleResponse(http.StatusInternalServerError, `{"message":"removal is unavailable"}`), nil
					}
					return lifecycleResponse(http.StatusNoContent, ""), nil
				})
				s := lifecyclePrepared(t, b)
				blocked := obstructLease(t, leaseDir)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := s.remove(ctx)
				cancel()
				if err == nil || !strings.Contains(err.Error(), "Revoking the sandbox egress lease") || attempts.Load() == 0 {
					t.Fatalf("revocation failure must be reported without preventing removal: %v, %d attempts", err, attempts.Load())
				}
				if strings.Contains(err.Error(), s.lease) || strings.Contains(log.String(), s.lease) {
					t.Fatal("cleanup diagnostics disclosed a lease credential")
				}
				wantLive := 0
				if failRemoval {
					wantLive = 1
				}
				if len(b.slots) != wantLive || b.Info().Live != wantLive {
					t.Fatal("capacity did not follow confirmed container removal")
				}
				before := attempts.Load()
				if err := os.RemoveAll(blocked); err != nil {
					t.Fatal(err)
				}
				removable.Store(true)
				time.Sleep(reapDelay)
				synctest.Wait()
				if len(b.slots) != 0 || b.Info().Live != 0 || !strings.Contains(log.String(), "Revoked the egress lease") {
					t.Fatalf("reaper did not confirm both resources: live=%d, slots=%d, log=%s", b.Info().Live, len(b.slots), &log)
				}
				if !failRemoval && attempts.Load() != before {
					t.Fatal("lease-only retry attempted removal of an already settled container")
				}
				if err := s.remove(context.Background()); err == nil {
					t.Fatal("repeat cleanup must retain the original explicit outcome, not race a background retry")
				}
			})
		})
	}
}

func TestReaperHandsUnconfirmedCapacityToShutdownSweep(t *testing.T) {
	leaseDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		b := newBroker(Config{Max: 1, LeaseDir: leaseDir, Instance: "fixture", Log: io.Discard})
		var removable atomic.Bool
		b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet {
				return lifecycleResponse(http.StatusOK, `[{"Id":"pending-start","Labels":{"octomus.sandbox.instance":"fixture"}}]`), nil
			}
			if removable.Load() {
				return lifecycleResponse(http.StatusNoContent, ""), nil
			}
			return lifecycleResponse(http.StatusInternalServerError, `{"message":"removal is unavailable"}`), nil
		})
		s := lifecyclePrepared(t, b)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := s.remove(ctx); err == nil {
			t.Fatal("expected retained container cleanup")
		}
		cancel()
		close(b.closing)
		synctest.Wait()
		if len(b.slots) != 1 || b.Info().Live != 1 {
			t.Fatal("closing the reaper prematurely released unconfirmed capacity")
		}
		removable.Store(true)
		if err := b.sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(b.slots) != 0 || b.Info().Live != 0 {
			t.Fatal("shutdown sweep did not settle confirmed removal")
		}
	})
}

func TestFailedCreateKeepsAdmissionUntilConfirmedCleanup(t *testing.T) {
	for _, unanswered := range []bool{false, true} {
		t.Run(map[bool]string{false: "daemon failure", true: "unanswered create"}[unanswered], func(t *testing.T) {
			leaseDir := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				b := newBroker(Config{Max: 1, LeaseDir: leaseDir, Log: io.Discard})
				defer close(b.closing)
				var available, removed atomic.Bool
				var deletions atomic.Int64
				b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/create"):
						if unanswered {
							return nil, io.ErrUnexpectedEOF
						}
						return lifecycleResponse(http.StatusInternalServerError, `{"message":"failed creation"}`), nil
					case r.Method == http.MethodGet:
						if !available.Load() || removed.Load() {
							return lifecycleResponse(http.StatusNotFound, `{"message":"not yet visible"}`), nil
						}
						return lifecycleResponse(http.StatusOK, `{"Id":"late-container","State":{"OOMKilled":false}}`), nil
					case r.Method == http.MethodDelete:
						deletions.Add(1)
						if !available.Load() {
							return lifecycleResponse(http.StatusInternalServerError, `{"message":"removal unavailable"}`), nil
						}
						if removed.Swap(true) {
							return lifecycleResponse(http.StatusNotFound, `{"message":"already removed"}`), nil
						}
						// The removal succeeded but its answer was lost. The next attempt must retain the observed ID.
						return nil, io.ErrUnexpectedEOF
					default:
						t.Errorf("failed creation must never start a child: %s", r.URL.Path)
						return nil, io.ErrUnexpectedEOF
					}
				})
				release, err := b.acquire(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := b.prepare(context.Background(), context.Background(), plan{kind: wire.KindVerify, image: "sha256:fixture"}, release); err == nil {
					t.Fatal("expected failed preparation")
				}
				if len(b.slots) != 1 || b.Info().Live != 1 {
					t.Fatal("failed create prematurely released admission")
				}
				if entries, err := os.ReadDir(leaseDir); err != nil || len(entries) != 0 {
					t.Fatalf("failed create retained egress: %v, %v", entries, err)
				}
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				if len(b.slots) != 1 || b.Info().Live != 1 {
					t.Fatal("elapsed time defaulted an unanswered create to confirmed absence")
				}
				if unanswered && deletions.Load() != 0 {
					t.Fatal("unknown container was removed before observing its reserved name")
				}
				available.Store(true)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				if !removed.Load() || len(b.slots) != 0 || b.Info().Live != 0 {
					t.Fatal("late removal did not settle admission")
				}
			})
		})
	}
}

func TestRejectedCreateDoesNotDeleteConflictingContainer(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict} {
		b := newBroker(Config{Max: 1, LeaseDir: t.TempDir(), Log: io.Discard})
		b.engine.http.Transport = probeTransport(func(r *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(r.URL.Path, "/create") {
				t.Errorf("rejected create must not delete an unrelated container: %s", r.URL.Path)
			}
			return lifecycleResponse(status, `{"message":"creation rejected"}`), nil
		})
		release, err := b.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.prepare(context.Background(), context.Background(), plan{kind: wire.KindVerify, image: "sha256:fixture"}, release); err == nil {
			t.Fatal("expected refused creation")
		}
		if len(b.slots) != 0 || b.Info().Live != 0 {
			t.Fatal("rejected creation retained admission")
		}
		if entries, err := os.ReadDir(b.cfg.LeaseDir); err != nil || len(entries) != 0 {
			t.Fatalf("rejected creation retained egress: %v, %v", entries, err)
		}
		close(b.closing)
	}
}

// Shared trusted-checkout ref mutations: real local-Git regressions for the
// per-repository fetch lock.
package git_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// seedRemoteBranch creates branch on the bare remote at the checkout's main.
func seedRemoteBranch(t *testing.T, c config.Config, branch string) {
	t.Helper()
	realGit(t, c.Repository, "push", "origin", "main:"+branch)
}

// advanceRemoteBranch moves an existing remote branch forward from main,
// leaving the trusted checkout's tracking ref stale while main stays put. It
// returns the advanced remote commit.
func advanceRemoteBranch(t *testing.T, c config.Config, branch string) string {
	t.Helper()
	stale := realGit(t, c.Repository, "rev-parse", "refs/remotes/origin/"+branch)
	local := "advance-" + branch
	realGit(t, c.Repository, "checkout", "-q", "-b", local, "main")
	writeFile(t, filepath.Join(c.Repository, "advance-"+branch+".txt"), branch+"\n")
	realGit(t, c.Repository, "add", ".")
	realGit(t, c.Repository, "commit", "-qm", "Advance "+branch)
	advanced := realGit(t, c.Repository, "rev-parse", "HEAD")
	realGit(t, c.Repository, "push", "origin", "HEAD:"+branch)
	realGit(t, c.Repository, "checkout", "-q", "main")
	realGit(t, c.Repository, "branch", "-D", local)
	// A push updates the checkout's own tracking ref; put it back so the fetch
	// under test must update it and therefore take the ref lock.
	realGit(t, c.Repository, "update-ref", "refs/remotes/origin/"+branch, stale)
	return advanced
}

// installFetchGate plants a reference-transaction hook that holds a fetch inside
// its ref transaction while the trigger file exists, so a second fetch can be
// let loose while the first still holds the ref lock. Removing the trigger
// releases the first.
func installFetchGate(t *testing.T, root, repository string) (trigger, entered string) {
	t.Helper()
	trigger = filepath.Join(root, "fetch-gate")
	entered = filepath.Join(root, "fetch-gate-entered")
	// Only the first prepared transaction that actually updates a ref (Git also
	// invokes the hook for empty no-op transactions) is held. A later fetch that
	// Git lets run concurrently must reach its own ref-lock failure, not this
	// gate.
	script := fmt.Sprintf(
		"#!/bin/sh\n[ \"$1\" = prepared ] || exit 0\n[ -f %q ] || exit 0\nrefs=$(cat)\n[ -n \"$refs\" ] || exit 0\n[ -f %q ] && exit 0\ntouch %q\nwhile [ -f %q ]; do sleep 0.005; done\nexit 0\n",
		trigger, entered, entered, trigger)
	hooks := filepath.Join(repository, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteExecutable(filepath.Join(hooks, "reference-transaction"), []byte(script)); err != nil {
		t.Fatal(err)
	}
	return trigger, entered
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestConcurrentFetchSerializesSharedCheckout advances an existing remote branch
// other than main, holds the first fetch inside its ref transaction, and then
// starts a second fetch. Without shared-checkout coordination the second fetch
// races the held ref lock and fails; with it the second fetch waits and both
// succeed. main must not move.
func TestConcurrentFetchSerializesSharedCheckout(t *testing.T) {
	c, root := fixtureRoot(t)
	ctx := context.Background()
	mainBefore, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	seedRemoteBranch(t, c, "feature")
	if err := git.Fetch(ctx, c); err != nil {
		t.Fatalf("establish tracking refs: %v", err)
	}
	advanced := advanceRemoteBranch(t, c, "feature")

	trigger, entered := installFetchGate(t, root, c.Repository)
	if err := os.WriteFile(trigger, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(trigger)

	key := filepath.Clean(c.Repository)
	waited := make(chan struct{}, 1)
	restore := git.SetRepoLockWaitHook(func(repository string) {
		if repository != key {
			return
		}
		select {
		case waited <- struct{}{}:
		default:
		}
	})
	defer restore()

	first := make(chan error, 1)
	go func() { first <- git.Fetch(ctx, c) }()
	if !waitForFile(entered, 15*time.Second) {
		t.Fatal("the first fetch never entered its ref transaction")
	}
	second := make(chan error, 1)
	go func() { second <- git.Fetch(ctx, c) }()

	var secondErr error
	held := false
	select {
	case secondErr = <-second:
		// The second fetch finished while the first held the ref lock: without
		// coordination it raced and failed.
	case <-waited:
		// The second fetch is waiting for the shared checkout's lock.
		held = true
	case <-time.After(15 * time.Second):
		t.Fatal("the second fetch neither finished nor waited for the shared checkout")
	}
	os.Remove(trigger)
	if err := <-first; err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if held {
		secondErr = <-second
	}
	if secondErr != nil {
		t.Fatalf("second fetch: %v", secondErr)
	}

	head, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "refs/remotes/origin/feature"})
	if err != nil {
		t.Fatal(err)
	}
	if head != advanced {
		t.Fatalf("tracking ref = %s; want the advanced commit %s", head, advanced)
	}
	mainAfter, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	if mainAfter != mainBefore {
		t.Fatalf("main moved during the concurrent fetch: %s -> %s", mainBefore, mainAfter)
	}
}

// TestFetchWaiterHonorsContext holds the checkout's lock and cancels a waiting
// fetch: it must return promptly with the context error and never touch Git.
func TestFetchWaiterHonorsContext(t *testing.T) {
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	release, err := git.AcquireRepoLockForTest(ctx, c.Repository)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	key := filepath.Clean(c.Repository)
	waited := make(chan struct{}, 1)
	restore := git.SetRepoLockWaitHook(func(repository string) {
		if repository != key {
			return
		}
		select {
		case waited <- struct{}{}:
		default:
		}
	})
	defer restore()

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- git.Fetch(waitCtx, c) }()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		t.Fatal("the fetch never waited for the held mutation lock")
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter error = %v; want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the canceled fetch waiter did not return")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a canceled waiter took %s to return", elapsed)
	}
}

// TestFetchForkHeadsUsesSharedCheckoutLock proves the fork-head writer cannot
// bypass the shared checkout's mutation lock.
func TestFetchForkHeadsUsesSharedCheckoutLock(t *testing.T) {
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	release, err := git.AcquireRepoLockForTest(ctx, c.Repository)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	defer releaseOnce()

	key := filepath.Clean(c.Repository)
	waited := make(chan struct{}, 1)
	restore := git.SetRepoLockWaitHook(func(repository string) {
		if repository != key {
			return
		}
		select {
		case waited <- struct{}{}:
		default:
		}
	})
	defer restore()

	done := make(chan error, 1)
	go func() {
		_, err := git.FetchForkHeads(ctx, c, []uint64{1})
		done <- err
	}()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		t.Fatal("FetchForkHeads never waited for the shared checkout")
	}
	releaseOnce()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FetchForkHeads after release: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("FetchForkHeads did not finish after the lock was released")
	}
}

// TestFetchIsIndependentAcrossRepositories holds one checkout's lock and proves
// a fetch against a different trusted checkout still completes.
func TestFetchIsIndependentAcrossRepositories(t *testing.T) {
	held, _ := fixtureRoot(t)
	other, _ := fixtureRoot(t)
	ctx := context.Background()
	release, err := git.AcquireRepoLockForTest(ctx, held.Repository)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	done := make(chan error, 1)
	go func() { done <- git.Fetch(ctx, other) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fetch on an unrelated repository: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a fetch waited for a different repository's mutation lock")
	}
}

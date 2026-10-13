// Shared-checkout operations: a fetch and a clone of the trusted checkout must not overlap.

package git_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// checkingOutGit is tests/fixtures/git.sh plus a detector: a command that mutates or copies the
// shared checkout named in the fixture root holds a marker directory while it runs, and records an
// overlap when another such command is already inside. It keeps the marker until a second such
// command arrives — to a deadline, so a serialized caller is never held forever — so an overlap is
// observed by construction rather than by timing.
const checkingOutGit = `#!/bin/sh
if [ -n "${OCTOMUS_TOKEN+set}${OCTOMUS_NOTIFICATION_WEBHOOK_URL+set}" ]; then
    echo 'fixture git: a service secret reached the Git environment' >&2
    exit 1
fi
url=https://github.com/fixture/project.git
if [ "$#" -eq 3 ] && [ "$1" = remote ] && [ "$2" = get-url ] && [ "$3" = origin ]; then
    echo "$url"
    exit 0
fi
shared=$(cat "$OCTOMUS_FIXTURE/shared-checkout" 2>/dev/null)
operation=$1
if [ "$operation" = "-c" ]; then
    operation=$5
fi
case $operation in
fetch|clone|update-ref|maintenance|gc) guarded=yes ;;
*) guarded= ;;
esac
if [ -n "$guarded" ] && [ "$PWD" = "$shared" ]; then
    echo "$operation" >> "$OCTOMUS_FIXTURE/operations"
    if mkdir "$OCTOMUS_FIXTURE/inside" 2>/dev/null; then
        n=0
        while [ "$n" -lt 60 ]; do
            here=$(wc -l < "$OCTOMUS_FIXTURE/operations" | tr -d ' ')
            [ "${here:-0}" -ge 2 ] && break
            sleep 0.05
            n=$((n + 1))
        done
        rmdir "$OCTOMUS_FIXTURE/inside" 2>/dev/null
    else
        echo "overlap $operation" >> "$OCTOMUS_FIXTURE/overlap"
    fi
fi
for arg; do
    shift
    if [ "$arg" = "$url" ]; then
        arg=$OCTOMUS_FIXTURE/remote.git
    fi
    set -- "$@" "$arg"
done
exec /usr/bin/git "$@"
`

// checkoutFixture is fixtureRoot with a git that reports overlapping shared-checkout operations.
func checkoutFixture(t *testing.T) (config.Config, string) {
	t.Helper()
	c, root := fixtureRoot(t)
	if err := os.WriteFile(filepath.Join(root, "shared-checkout"), []byte(c.Repository), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteExecutable(filepath.Join(root, "bin", "git"), []byte(checkingOutGit)); err != nil {
		t.Fatal(err)
	}
	return c, root
}

func overlaps(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "overlap"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	return []string{string(data)}
}

// waitForOperation waits until the fixture reports command inside the shared checkout.
func waitForOperation(t *testing.T, root, command string) {
	t.Helper()
	done := time.Now().Add(10 * time.Second)
	for time.Now().Before(done) {
		if strings.Contains(operations(t, root), command+"\n") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the fixture never reported %q inside the shared checkout: %q", command, operations(t, root))
}

func operations(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "operations"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// A task's first turn fetches the shared trusted checkout while planning, baseline verification or
// another task may be cloning it. Both operations run as real git without coordination, so the
// clone's copy of the checkout's objects can race the fetch's automatic maintenance. The fixture
// holds each shared-checkout operation in place to observe the overlap deterministically: the test
// requires that the two never run inside the checkout together, and that both still succeed.
func TestSharedCheckoutOperationsDoNotOverlap(t *testing.T) {
	// Not parallel: the fixture root and its marker are process-wide, and the hold is wall-clock.
	c, root := checkoutFixture(t)
	ctx := context.Background()
	revision, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "tasks", "overlap", "workspace")
	start := make(chan struct{})
	var wg sync.WaitGroup
	fetchErr := make(chan error, 1)
	cloneErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		fetchErr <- git.Fetch(ctx, c)
	}()
	go func() {
		defer wg.Done()
		<-start
		cloneErr <- git.CloneAt(ctx, c, workspace, revision)
	}()
	close(start)
	wg.Wait()
	close(fetchErr)
	close(cloneErr)
	if err := <-fetchErr; err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := <-cloneErr; err != nil {
		t.Fatalf("clone: %v", err)
	}
	if overlap := overlaps(t, root); len(overlap) != 0 {
		t.Fatalf("shared-checkout operations overlapped: %v (operations: %s)", overlap, operations(t, root))
	}
	if head, err := git.Git(ctx, c, workspace, []string{"rev-parse", "HEAD"}); err != nil || head != revision {
		t.Fatalf("cloned workspace HEAD = %q, %v; want %s", head, err, revision)
	}
	if seen := operations(t, root); seen != "clone\nfetch\n" && seen != "fetch\nclone\n" {
		t.Fatalf("both shared-checkout operations must have run: %s", seen)
	}
}

// A waiter whose cycle or task was cancelled stops waiting instead of holding its worker open until
// the fetch it queued behind finishes. Nothing is run in the checkout for it.
func TestSharedCheckoutWaitIsCancellable(t *testing.T) {
	// Not parallel, for the same reason as above.
	c, root := checkoutFixture(t)
	ctx := context.Background()
	holder := make(chan error, 1)
	go func() { holder <- git.Fetch(ctx, c) }()
	waitForOperation(t, root, "fetch")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	// The holder is inside the checkout's writer for about a second; a gate that ignored cancellation
	// would block here for as long as it runs.
	start := time.Now()
	err := git.Fetch(cancelled, c)
	elapsed := time.Since(start)
	if seen := operations(t, root); seen != "fetch\n" {
		t.Fatalf("a cancelled wait ran a shared-checkout command: %q", seen)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("a cancelled shared-checkout wait took %s, until the running fetch finished", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v; want context.Canceled", err)
	}
	if err := <-holder; err != nil {
		t.Fatalf("holding fetch: %v", err)
	}
	if overlap := overlaps(t, root); len(overlap) != 0 {
		t.Fatalf("shared-checkout operations overlapped: %v", overlap)
	}
}

package git

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// repoLocks serializes ref mutations of one trusted checkout. Git's own ref
// locks do not compose across processes: two fetches updating the same
// remote-tracking ref can both read its old value, and the loser fails with
// "cannot lock ref ... is now at <new>". Every writer that changes the shared
// clone's refs (Fetch, FetchForkHeads) holds this lock, so concurrent tasks
// cannot race each other on the checkout they all read, while read-only
// commands, independent task workspaces and different repositories stay
// concurrent.
var repoLocks = &keyedLocks{entries: map[string]*keyedLock{}}

type keyedLocks struct {
	mu      sync.Mutex
	entries map[string]*keyedLock
}

// keyedLock is one repository's mutation token plus a reference count. The
// count includes the holder and every waiter, so the entry can be dropped once
// the last reference leaves rather than growing for the process lifetime.
type keyedLock struct {
	token chan struct{}
	held  int
}

// repoLockWait, when set, observes a caller that must wait for a repository's
// mutation lock. Tests install it (through export_test.go) to synchronize a
// held lock with a waiting caller without sleeping; production leaves it nil.
var repoLockWait atomic.Pointer[func(repository string)]

// lockRepo waits for the shared checkout's mutation lock. Waiting honors ctx,
// so a canceled caller returns promptly without touching Git, and the lock is
// released even when cancellation lands after a token was offered.
func lockRepo(ctx context.Context, repository string) (release func(), err error) {
	key := filepath.Clean(repository)
	repoLocks.mu.Lock()
	entry := repoLocks.entries[key]
	if entry == nil {
		entry = &keyedLock{token: make(chan struct{}, 1)}
		repoLocks.entries[key] = entry
	}
	entry.held++
	repoLocks.mu.Unlock()
	drop := func() {
		repoLocks.mu.Lock()
		entry.held--
		if entry.held == 0 {
			delete(repoLocks.entries, key)
		}
		repoLocks.mu.Unlock()
	}
	release = func() {
		<-entry.token
		drop()
	}
	// Try the uncontended case first, then announce a wait so tests can observe
	// it before blocking.
	select {
	case entry.token <- struct{}{}:
	default:
		if observer := repoLockWait.Load(); observer != nil {
			(*observer)(key)
		}
		select {
		case entry.token <- struct{}{}:
		case <-ctx.Done():
			drop()
			return nil, ctx.Err()
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		release()
		return nil, cerr
	}
	return release, nil
}

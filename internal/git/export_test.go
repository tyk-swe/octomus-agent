package git

import "context"

// SetRepoLockWaitHook installs an observer that runs whenever a caller must
// wait for a repository's mutation lock, and returns a restore that removes it.
// Tests use it to synchronize a held lock with a waiting caller without
// sleeping; production never sets it.
func SetRepoLockWaitHook(hook func(repository string)) func() {
	if hook == nil {
		repoLockWait.Store(nil)
		return func() { repoLockWait.Store(nil) }
	}
	repoLockWait.Store(&hook)
	return func() { repoLockWait.Store(nil) }
}

// AcquireRepoLockForTest holds a repository's mutation lock and returns its
// release. Tests use it to make a trusted-checkout writer wait deterministically.
func AcquireRepoLockForTest(ctx context.Context, repository string) (func(), error) {
	return lockRepo(ctx, repository)
}

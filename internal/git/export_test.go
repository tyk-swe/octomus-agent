package git

import "context"

// AcquireCheckoutForTest holds a repository's checkout gate for writing and returns its
// release. Tests use it to make a trusted-checkout writer wait deterministically.
func AcquireCheckoutForTest(ctx context.Context, repository string) (func(), error) {
	return gateFor(repository).acquire(ctx, true)
}

// CheckoutWriterWaitingForTest reports a writer queued at the checkout gate.
func CheckoutWriterWaitingForTest(repository string) bool {
	gate := gateFor(repository)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.waiting > 0
}

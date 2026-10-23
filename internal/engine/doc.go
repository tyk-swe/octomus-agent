// Package engine is the scheduler and cycle orchestration: planning,
// execution, review, verification, recovery and delivery. It is the only
// layer the HTTP API reaches state through, and it owns the admission gate
// that serializes checkpoints with operator actions.
package engine

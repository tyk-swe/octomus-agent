// Package process runs orchestrator child processes in their own process
// groups, with bounded capture and deadlines. Untrusted children never
// start here; they go through internal/sandbox.
package process

// Package redact is the shared secret scrubber: it removes credentials and
// tokens from text, errors and saved evidence before they reach logs,
// prompts or the dashboard, and bounds display output. It knows nothing
// about the records it cleans.
package redact

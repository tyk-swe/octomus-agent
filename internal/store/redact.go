package store

// Redaction itself lives in package redact, which process, git and runner use
// without depending on persistence. The store scrubs event messages and
// exports with it; the forwarding names below remain only until
// internal/engine calls package redact directly.

import (
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// WebhookEnv forwards redact.WebhookEnv.
const WebhookEnv = redact.WebhookEnv

// DisplayTransform forwards redact.DisplayTransform.
type DisplayTransform = redact.DisplayTransform

// ErrorMessage forwards redact.Error.
func ErrorMessage(err error) string { return redact.Error(err) }

// RedactSecrets forwards redact.Secrets.
func RedactSecrets(input string) string { return redact.Secrets(input) }

// Redact forwards redact.Text.
func Redact(input string) string { return redact.Text(input) }

// DisplayJSON forwards redact.DisplayJSON.
func DisplayJSON(object map[string]any) (map[string]any, []DisplayTransform) {
	return redact.DisplayJSON(object)
}

// RedactedValue serializes a typed export, decodes it as generic JSON (numbers
// kept verbatim) and scrubs every string in place. Exports use it so redaction
// happens after the facts are computed from the saved records.
func RedactedValue(value any) (map[string]any, error) {
	generic, err := wirejson.GenericMap(value)
	if err != nil {
		return nil, err
	}
	redact.JSON(generic)
	return generic, nil
}

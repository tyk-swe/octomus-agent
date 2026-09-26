package store

// Redaction itself lives in package redact, which process, git, runner and
// engine use without depending on persistence. The store scrubs event messages
// and exports with it.

import (
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

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

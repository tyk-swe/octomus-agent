package store_test

import (
	"fmt"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

// RedactedValue scrubs every string of a typed export after serialization,
// nested ones included, whatever whitespace separates a bearer token.
func TestRedactedValueScrubsEveryExportString(t *testing.T) {
	// Every Unicode White_Space code point, including ASCII vertical tab.
	whitespace := "\t\n\v\f\r \u0085                 　"
	for _, separator := range whitespace {
		t.Run(fmt.Sprintf("U+%04X", separator), func(t *testing.T) {
			input := "before bEaReR" + string(separator) + "\t" + "synthetic-private-credential after"
			value, err := store.RedactedValue(map[string]any{"nested": []any{input}, "count": 7})
			must(t, err)
			if got := canonical(t, value); got != `{"count":7,"nested":["before [redacted] after"]}` {
				t.Fatalf("redacted export = %s", got)
			}
		})
	}
}

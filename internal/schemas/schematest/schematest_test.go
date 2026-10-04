// The schema-to-decoder match reports drift on either side.

package schematest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

type document struct {
	Items []item `json:"items"`
}

type item struct {
	ID   string `json:"id"`
	Done bool   `json:"done"`
}

var boolean = schemas.Schema{"type": "boolean"}

func TestMatchFindsDriftOnEitherSide(t *testing.T) {
	exact := schemas.Object(schemas.Schema{"items": schemas.Array(schemas.Object(schemas.Schema{
		"id": schemas.String(), "done": boolean,
	}))})
	for name, tc := range map[string]struct {
		schema schemas.Schema
		want   string
	}{
		"exact": {exact, ""},
		"extra schema field": {schemas.Object(schemas.Schema{"items": schemas.Array(schemas.Object(schemas.Schema{
			"id": schemas.String(), "done": boolean, "risk": schemas.String(),
		}))}), "document.items[]: schema properties [done id risk]"},
		"missing schema field": {schemas.Object(schemas.Schema{"items": schemas.Array(schemas.Object(schemas.Schema{
			"id": schemas.String(),
		}))}), "document.items[]: schema properties [id]"},
		"different kind": {schemas.Object(schemas.Schema{"items": schemas.Array(schemas.Object(schemas.Schema{
			"id": schemas.String(), "done": schemas.String(),
		}))}), "document.items[].done: schema"},
		"not an array": {schemas.Object(schemas.Schema{"items": schemas.String()}), "document.items: schema"},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{TB: t}
			Match(r, "document", tc.schema, reflect.TypeOf(document{}))
			if tc.want == "" {
				if len(r.errors) != 0 {
					t.Fatalf("exact schema reported %q", r.errors)
				}
				return
			}
			if len(r.errors) == 0 || !strings.HasPrefix(r.errors[0], tc.want) {
				t.Fatalf("errors = %q; want one starting %q", r.errors, tc.want)
			}
		})
	}
}

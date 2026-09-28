package engine

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/schemas/schematest"
)

func TestPlanningSchemasMatchTheirDocuments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		schema schemas.Schema
		typ    reflect.Type
	}{
		{"proposal document", schemas.ProposalSchema(), reflect.TypeOf(proposalDocument{})},
		{"assessment document", assessmentSchema(), reflect.TypeOf(assessmentDocument{})},
		{"grounding document", groundingSchema(), reflect.TypeOf(groundingDocument{})},
	} {
		schematest.Match(t, tc.name, tc.schema, tc.typ)
		sample := schematest.Sample(tc.schema)
		if err := schemas.Validate(sample, tc.schema); err != nil {
			t.Fatalf("sample %s: %v", tc.name, err)
		}
		schematest.SameAfterDecoding(t, sample, reflect.New(tc.typ).Interface())
	}
}

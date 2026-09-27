package model

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/schemas/schematest"
)

// Runners hold each answer to these schemas and the strict decoders then read
// it, so a field added to only one side would fail every planning or review
// turn with an unexpected or missing field.
func TestStructuredSchemasMatchStrictDecoders(t *testing.T) {
	proposals := schemas.ProposalSchema()
	schematest.Match(t, "proposal document", proposals, reflect.TypeOf(struct {
		Proposals []Proposal `json:"proposals"`
	}{}))
	schematest.Match(t, "review", schemas.ReviewSchema(), reflect.TypeOf(Review{}))

	// A sample answer the schema accepts must decode strictly and keep every
	// value it carried.
	document := schematest.Sample(proposals).(map[string]any)
	if err := schemas.Validate(document, proposals); err != nil {
		t.Fatalf("sample proposal document: %v", err)
	}
	var decoded []Proposal
	schematest.SameAfterDecoding(t, document["proposals"], &decoded)
	if len(decoded) != 1 || decoded[0].Title != "x" || !reflect.DeepEqual(decoded[0].RelevantPaths, []string{"x"}) {
		t.Fatalf("decoded proposals = %#v", decoded)
	}
	review := schematest.Sample(schemas.ReviewSchema())
	if err := schemas.Validate(review, schemas.ReviewSchema()); err != nil {
		t.Fatalf("sample review: %v", err)
	}
	var decodedReview Review
	schematest.SameAfterDecoding(t, review, &decodedReview)
	if !decodedReview.Completed || len(decodedReview.Findings) != 1 || decodedReview.Findings[0].Priority != "x" {
		t.Fatalf("decoded review = %#v", decodedReview)
	}
}

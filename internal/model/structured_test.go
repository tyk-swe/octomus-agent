package model

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/schemas/schematest"
)

func TestStructuredSchemasMatchStrictDecoders(t *testing.T) {
	proposals := schemas.ProposalSchema()
	schematest.Match(t, "proposal document", proposals, reflect.TypeOf(struct {
		Proposals []Proposal `json:"proposals"`
	}{}))
	schematest.Match(t, "review", schemas.ReviewSchema(), reflect.TypeOf(Review{}))

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

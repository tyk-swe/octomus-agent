package schemas

import "testing"

func TestStructuredResultSchemas(t *testing.T) {
	proposal := map[string]any{}
	for _, key := range []string{"id", "title", "problem", "benefit", "category", "target", "tier", "scope", "prompt", "decision", "reason", "problem_key"} {
		proposal[key] = "value"
	}
	for _, key := range []string{"evidence", "dependencies", "relevant_paths", "reconsiders"} {
		proposal[key] = []any{}
	}
	valid := map[string]any{"proposals": []any{proposal}}
	if err := Validate(valid, ProposalSchema()); err != nil {
		t.Fatal(err)
	}
	delete(proposal, "prompt")
	if err := Validate(valid, ProposalSchema()); err == nil {
		t.Fatal("accepted a proposal without a prompt")
	}
	proposal["prompt"] = "value"
	proposal["extra"] = "value"
	if err := Validate(valid, ProposalSchema()); err == nil {
		t.Fatal("accepted an unexpected proposal field")
	}
	review := map[string]any{"completed": true, "summary": "reviewed", "findings": []any{}}
	if err := Validate(review, ReviewSchema()); err != nil {
		t.Fatal(err)
	}
	review["completed"] = "true"
	if err := Validate(review, ReviewSchema()); err == nil {
		t.Fatal("accepted a non-boolean review verdict")
	}
}

package schemas

import (
	"strings"
	"testing"
)

func TestStructuredResultSchemas(t *testing.T) {
	proposal := validProposal()
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

func validProposal() map[string]any {
	proposal := map[string]any{}
	for _, key := range []string{"id", "title", "problem", "benefit", "category", "target", "tier", "scope", "prompt", "decision", "reason", "problem_key"} {
		proposal[key] = "value"
	}
	for _, key := range []string{"evidence", "dependencies", "relevant_paths", "reconsiders"} {
		proposal[key] = []any{}
	}
	return proposal
}

// Nested failures name the schema path, never an untrusted key.
func TestStructuredResultErrorsNameTheField(t *testing.T) {
	finding := func() map[string]any {
		return map[string]any{"title": "t", "file": "f", "detail": "d", "priority": "p"}
	}
	review := func(findings any) map[string]any {
		return map[string]any{"completed": true, "summary": "s", "findings": findings}
	}
	badTitle := validProposal()
	badTitle["title"] = 1.0
	badEvidence := validProposal()
	badEvidence["evidence"] = "not a list"
	badItem := validProposal()
	badItem["evidence"] = []any{"ok", true}
	noPrompt := validProposal()
	delete(noPrompt, "prompt")
	extra := finding()
	extra["injected_key_name"] = "x"
	for _, test := range []struct {
		name   string
		value  any
		schema Schema
		want   string
	}{
		{"root object", []any{}, ProposalSchema(), "Structured result must be an object"},
		{"root missing", map[string]any{}, ProposalSchema(), `Structured result is missing required field "proposals"`},
		{"root extra", map[string]any{"proposals": []any{}, "other": 1.0}, ProposalSchema(), "Structured result has an unexpected field"},
		{"root boolean", map[string]any{"completed": "true", "summary": "s", "findings": []any{}}, ReviewSchema(), "Structured result field completed must be a boolean"},
		{"nested string", map[string]any{"proposals": []any{validProposal(), badTitle}}, ProposalSchema(), "Structured result field proposals[1].title must be a string"},
		{"nested array", map[string]any{"proposals": []any{badEvidence}}, ProposalSchema(), "Structured result field proposals[0].evidence must be an array"},
		{"nested item", map[string]any{"proposals": []any{badItem}}, ProposalSchema(), "Structured result field proposals[0].evidence[1] must be a string"},
		{"nested missing", map[string]any{"proposals": []any{noPrompt}}, ProposalSchema(), `Structured result object proposals[0] is missing required field "prompt"`},
		{"list object", map[string]any{"proposals": []any{"x"}}, ProposalSchema(), "Structured result field proposals[0] must be an object"},
		{"findings array", review("x"), ReviewSchema(), "Structured result field findings must be an array"},
		{"findings extra", review([]any{extra}), ReviewSchema(), "Structured result object findings[0] has an unexpected field"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.value, test.schema)
			if err == nil || err.Error() != test.want {
				t.Fatalf("Validate() = %v; want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "injected_key_name") {
				t.Fatalf("error echoes an unexpected key: %v", err)
			}
		})
	}
}

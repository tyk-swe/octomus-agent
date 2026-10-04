// Structured-output schemas accept only complete, exact answers.

package schemas

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
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

// Every structured-output schema is held to the Go type that decodes its
// answers: same fields, same kinds, and a schema-valid sample decodes without
// loss.
func TestEverySchemaMatchesTheTypeThatDecodesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema Schema
		typ    reflect.Type
		check  func(t *testing.T, decoded any)
	}{
		{"proposal document", ProposalSchema(), reflect.TypeOf(model.ProposalDocument{}), func(t *testing.T, decoded any) {
			document := decoded.(*model.ProposalDocument)
			if len(document.Proposals) != 1 || document.Proposals[0].Title != "x" || !reflect.DeepEqual(document.Proposals[0].RelevantPaths, []string{"x"}) {
				t.Fatalf("decoded proposals = %#v", document.Proposals)
			}
		}},
		{"assessment document", AssessmentSchema(), reflect.TypeOf(model.AssessmentDocument{}), func(t *testing.T, decoded any) {
			document := decoded.(*model.AssessmentDocument)
			if len(document.Assessments) != 1 || document.Assessments[0].ID != "x" || document.Assessments[0].Decision != "x" {
				t.Fatalf("decoded assessments = %#v", document.Assessments)
			}
		}},
		{"grounding document", GroundingSchema(), reflect.TypeOf(model.GroundingDocument{}), func(t *testing.T, decoded any) {
			if document := decoded.(*model.GroundingDocument); document.Context != "x" {
				t.Fatalf("decoded grounding = %#v", document)
			}
		}},
		{"review", ReviewSchema(), reflect.TypeOf(model.Review{}), func(t *testing.T, decoded any) {
			review := decoded.(*model.Review)
			if !review.Completed || len(review.Findings) != 1 || review.Findings[0].Priority != "x" {
				t.Fatalf("decoded review = %#v", review)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match(t, tc.name, tc.schema, tc.typ)
			answer := sample(tc.schema)
			if err := Validate(answer, tc.schema); err != nil {
				t.Fatalf("sample %s: %v", tc.name, err)
			}
			decoded := reflect.New(tc.typ).Interface()
			sameAfterDecoding(t, answer, decoded)
			tc.check(t, decoded)
		})
	}
}

// match reports every difference between a schema and the type decoding its
// answers: the field set, the required set and each field's kind.
func match(t testing.TB, path string, schema Schema, typ reflect.Type) {
	t.Helper()
	switch typ.Kind() {
	case reflect.String:
		if !reflect.DeepEqual(schema, String()) {
			t.Errorf("%s: schema %v; want a string", path, schema)
		}
	case reflect.Bool:
		if !reflect.DeepEqual(schema, Schema{"type": "boolean"}) {
			t.Errorf("%s: schema %v; want a boolean", path, schema)
		}
	case reflect.Slice:
		items, ok := schema["items"].(Schema)
		if schema["type"] != "array" || !ok {
			t.Errorf("%s: schema %v; want an array", path, schema)
			return
		}
		match(t, path+"[]", items, typ.Elem())
	case reflect.Struct:
		properties, ok := schema["properties"].(Schema)
		if schema["type"] != "object" || !ok || schema["additionalProperties"] != false {
			t.Errorf("%s: schema %v; want a closed object", path, schema)
			return
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			fields[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
		}
		want := slices.Sorted(maps.Keys(fields))
		if got := slices.Sorted(maps.Keys(properties)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema properties %v; %s fields %v", path, got, typ, want)
		}
		if required, _ := schema["required"].([]string); !reflect.DeepEqual(required, want) {
			t.Errorf("%s: schema requires %v; want every field %v", path, required, want)
		}
		for name, fieldType := range fields {
			if property, ok := properties[name].(Schema); ok {
				match(t, path+"."+name, property, fieldType)
			}
		}
	default:
		t.Errorf("%s: %s has no structured-result schema form", path, typ)
	}
}

// sample builds a schema-valid answer with one element per array and "x" for
// every string.
func sample(schema Schema) any {
	switch schema["type"] {
	case "object":
		object := map[string]any{}
		for key, property := range schema["properties"].(Schema) {
			object[key] = sample(property.(Schema))
		}
		return object
	case "array":
		return []any{sample(schema["items"].(Schema))}
	case "boolean":
		return true
	default:
		return "x"
	}
}

// sameAfterDecoding decodes value into dst and fails when the decoder refuses
// it or when re-encoding dst changes the answer.
func sameAfterDecoding(t testing.TB, value, dst any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("decoder refused a schema-valid answer: %v", err)
	}
	again, err := json.Marshal(dst)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	if json.Unmarshal(data, &before) != nil || json.Unmarshal(again, &after) != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("decoding changed the answer:\n%s\n%s", data, again)
	}
}

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

var boolean = Schema{"type": "boolean"}

func TestMatchFindsDriftOnEitherSide(t *testing.T) {
	exact := Object(Schema{"items": Array(Object(Schema{
		"id": String(), "done": boolean,
	}))})
	for name, tc := range map[string]struct {
		schema Schema
		want   string
	}{
		"exact": {exact, ""},
		"extra schema field": {Object(Schema{"items": Array(Object(Schema{
			"id": String(), "done": boolean, "risk": String(),
		}))}), "document.items[]: schema properties [done id risk]"},
		"missing schema field": {Object(Schema{"items": Array(Object(Schema{
			"id": String(),
		}))}), "document.items[]: schema properties [id]"},
		"different kind": {Object(Schema{"items": Array(Object(Schema{
			"id": String(), "done": String(),
		}))}), "document.items[].done: schema"},
		"not an array": {Object(Schema{"items": String()}), "document.items: schema"},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{TB: t}
			match(r, "document", tc.schema, reflect.TypeOf(document{}))
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

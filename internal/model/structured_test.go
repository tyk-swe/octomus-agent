package model

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

// Runners hold each answer to these schemas and the strict decoders then read
// it, so a field added to only one side would fail every planning or review
// turn with an unexpected or missing field.
func TestStructuredSchemasMatchStrictDecoders(t *testing.T) {
	proposals := schemas.ProposalSchema()
	matchSchema(t, "proposal document", proposals, reflect.TypeOf(struct {
		Proposals []Proposal `json:"proposals"`
	}{}))
	matchSchema(t, "review", schemas.ReviewSchema(), reflect.TypeOf(Review{}))

	// A sample answer the schema accepts must decode strictly and keep every
	// value it carried.
	document := sampleAnswer(proposals).(map[string]any)
	if err := schemas.Validate(document, proposals); err != nil {
		t.Fatalf("sample proposal document: %v", err)
	}
	var decoded []Proposal
	sameAfterDecoding(t, document["proposals"], &decoded)
	if len(decoded) != 1 || decoded[0].Title != "x" || !reflect.DeepEqual(decoded[0].RelevantPaths, []string{"x"}) {
		t.Fatalf("decoded proposals = %#v", decoded)
	}
	review := sampleAnswer(schemas.ReviewSchema())
	if err := schemas.Validate(review, schemas.ReviewSchema()); err != nil {
		t.Fatalf("sample review: %v", err)
	}
	var decodedReview Review
	sameAfterDecoding(t, review, &decodedReview)
	if !decodedReview.Completed || len(decodedReview.Findings) != 1 || decodedReview.Findings[0].Priority != "x" {
		t.Fatalf("decoded review = %#v", decodedReview)
	}
}

// matchSchema checks that schema describes exactly the JSON form of typ.
func matchSchema(t *testing.T, path string, schema schemas.Schema, typ reflect.Type) {
	t.Helper()
	switch typ.Kind() {
	case reflect.String:
		if !reflect.DeepEqual(schema, schemas.String()) {
			t.Errorf("%s: schema %v; want a string", path, schema)
		}
	case reflect.Bool:
		if !reflect.DeepEqual(schema, schemas.Schema{"type": "boolean"}) {
			t.Errorf("%s: schema %v; want a boolean", path, schema)
		}
	case reflect.Slice:
		items, ok := schema["items"].(schemas.Schema)
		if schema["type"] != "array" || !ok {
			t.Errorf("%s: schema %v; want an array", path, schema)
			return
		}
		matchSchema(t, path+"[]", items, typ.Elem())
	case reflect.Struct:
		properties, ok := schema["properties"].(schemas.Schema)
		if schema["type"] != "object" || !ok || schema["additionalProperties"] != false {
			t.Errorf("%s: schema %v; want a closed object", path, schema)
			return
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			fields[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
		}
		want := sortedKeys(fields)
		if got := sortedKeys(properties); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema properties %v; %s fields %v", path, got, typ, want)
		}
		if required, _ := schema["required"].([]string); !reflect.DeepEqual(required, want) {
			t.Errorf("%s: schema requires %v; want every field %v", path, required, want)
		}
		for name, fieldType := range fields {
			if property, ok := properties[name].(schemas.Schema); ok {
				matchSchema(t, path+"."+name, property, fieldType)
			}
		}
	default:
		t.Errorf("%s: %s has no structured-result schema form", path, typ)
	}
}

// sampleAnswer fills every schema field, with one item in each list.
func sampleAnswer(schema schemas.Schema) any {
	switch schema["type"] {
	case "object":
		object := map[string]any{}
		for key, property := range schema["properties"].(schemas.Schema) {
			object[key] = sampleAnswer(property.(schemas.Schema))
		}
		return object
	case "array":
		return []any{sampleAnswer(schema["items"].(schemas.Schema))}
	case "boolean":
		return true
	default:
		return "x"
	}
}

// sameAfterDecoding decodes value into dst and checks that encoding dst
// again gives back the same JSON.
func sameAfterDecoding(t *testing.T, value, dst any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("strict decoder refused a schema-valid answer: %v", err)
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

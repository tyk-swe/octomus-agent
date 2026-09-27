package schematest

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

func Match(t testing.TB, path string, schema schemas.Schema, typ reflect.Type) {
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
		Match(t, path+"[]", items, typ.Elem())
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
		want := slices.Sorted(maps.Keys(fields))
		if got := slices.Sorted(maps.Keys(properties)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema properties %v; %s fields %v", path, got, typ, want)
		}
		if required, _ := schema["required"].([]string); !reflect.DeepEqual(required, want) {
			t.Errorf("%s: schema requires %v; want every field %v", path, required, want)
		}
		for name, fieldType := range fields {
			if property, ok := properties[name].(schemas.Schema); ok {
				Match(t, path+"."+name, property, fieldType)
			}
		}
	default:
		t.Errorf("%s: %s has no structured-result schema form", path, typ)
	}
}

func Sample(schema schemas.Schema) any {
	switch schema["type"] {
	case "object":
		object := map[string]any{}
		for key, property := range schema["properties"].(schemas.Schema) {
			object[key] = Sample(property.(schemas.Schema))
		}
		return object
	case "array":
		return []any{Sample(schema["items"].(schemas.Schema))}
	case "boolean":
		return true
	default:
		return "x"
	}
}

func SameAfterDecoding(t testing.TB, value, dst any) {
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

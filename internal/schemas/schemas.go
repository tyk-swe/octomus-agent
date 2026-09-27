// Package schemas validates the small structured-result vocabulary used by both
// runners. A parseable result alone is never evidence of a completed review.
package schemas

import (
	"encoding/json"
	"fmt"
	"sort"
)

type Schema = map[string]any

func String() Schema            { return Schema{"type": "string"} }
func Array(items Schema) Schema { return Schema{"type": "array", "items": items} }
func Object(properties Schema) Schema {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return Schema{"type": "object", "properties": properties, "required": keys, "additionalProperties": false}
}
func ProposalSchema() Schema {
	fields := Schema{}
	for _, key := range []string{"id", "title", "problem", "benefit", "category", "target", "tier", "scope", "prompt", "decision", "reason", "problem_key"} {
		fields[key] = String()
	}
	for _, key := range []string{"evidence", "dependencies", "relevant_paths", "reconsiders"} {
		fields[key] = Array(String())
	}
	return Object(Schema{"proposals": Array(Object(fields))})
}
func ReviewSchema() Schema {
	return Object(Schema{"completed": Schema{"type": "boolean"}, "summary": String(), "findings": Array(Object(Schema{"title": String(), "file": String(), "detail": String(), "priority": String()}))})
}

// Validate checks a decoded structured result against schema. Failures below
// the root name their field path, built only from schema property names and
// array indices, so untrusted keys are never echoed.
func Validate(value any, schema Schema) error { return validate(value, schema, "") }

func validate(value any, schema Schema, path string) error {
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return mismatch(path, "an object")
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("Invalid object schema")
		}
		var required []any
		switch keys := schema["required"].(type) {
		case []any:
			required = keys
		case []string:
			for _, key := range keys {
				required = append(required, key)
			}
		}
		for _, key := range required {
			name, ok := key.(string)
			_, exists := object[name]
			if !ok || !exists {
				encoded, _ := json.Marshal(key)
				if path == "" {
					return fmt.Errorf("Structured result is missing required field %s", encoded)
				}
				return fmt.Errorf("Structured result object %s is missing required field %s", path, encoded)
			}
		}
		// Visit keys in order so the first validation failure is stable.
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if nested, ok := properties[key]; ok {
				child := key
				if path != "" {
					child = path + "." + key
				}
				if err := validate(object[key], asSchema(nested), child); err != nil {
					return err
				}
			} else if schema["additionalProperties"] == false {
				if path == "" {
					return fmt.Errorf("Structured result has an unexpected field")
				}
				return fmt.Errorf("Structured result object %s has an unexpected field", path)
			}
		}
	case "array":
		values, ok := value.([]any)
		if !ok {
			return mismatch(path, "an array")
		}
		for i, v := range values {
			if err := validate(v, asSchema(schema["items"]), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return mismatch(path, "a string")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return mismatch(path, "a boolean")
		}
	default:
		return fmt.Errorf("Unsupported structured result schema")
	}
	return nil
}

// mismatch keeps the root message unchanged and names nested fields.
func mismatch(path, kind string) error {
	if path == "" {
		return fmt.Errorf("Structured result must be %s", kind)
	}
	return fmt.Errorf("Structured result field %s must be %s", path, kind)
}
func asSchema(v any) Schema { s, _ := v.(map[string]any); return s }

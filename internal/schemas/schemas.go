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
func Validate(value any, schema Schema) error {
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("Structured result must be an object")
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
				return fmt.Errorf("Structured result is missing required field %s", encoded)
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
				if err := Validate(object[key], asSchema(nested)); err != nil {
					return err
				}
			} else if schema["additionalProperties"] == false {
				return fmt.Errorf("Structured result has an unexpected field")
			}
		}
	case "array":
		values, ok := value.([]any)
		if !ok {
			return fmt.Errorf("Structured result must be an array")
		}
		for _, v := range values {
			if err := Validate(v, asSchema(schema["items"])); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("Structured result must be a string")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("Structured result must be a boolean")
		}
	default:
		return fmt.Errorf("Unsupported structured result schema")
	}
	return nil
}
func asSchema(v any) Schema { s, _ := v.(map[string]any); return s }

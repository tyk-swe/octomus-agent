package wirejson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Match encoding/json's nesting bound, which Decoder.Token does not enforce.
const maxDepth = 10000

// Parse decodes one JSON value, preserving numbers as json.Number. It rejects
// duplicate object keys, invalid Unicode strings and trailing data.
func Parse(data []byte) (any, error) {
	value, err := parse(data)
	return value, marked(err)
}

func parse(data []byte) (any, error) {
	if err := ValidStrings(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return value, nil
}

func parseValue(dec *json.Decoder, depth int) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	if depth >= maxDepth {
		return nil, fmt.Errorf("JSON exceeds the maximum nesting depth")
	}
	var value any
	switch delim {
	case '{':
		object := make(map[string]any)
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name := key.(string)
			if _, duplicate := object[name]; duplicate {
				return nil, fmt.Errorf("duplicate field %q", name)
			}
			item, err := parseValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = item
		}
		value = object
	case '[':
		array := []any{}
		for dec.More() {
			item, err := parseValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, item)
		}
		value = array
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delim)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return value, nil
}

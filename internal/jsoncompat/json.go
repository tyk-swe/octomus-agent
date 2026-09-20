// Package jsoncompat implements the saved serde JSON contract at wire boundaries.
// In particular, Go's usual null-to-zero and case-insensitive field matching are
// not valid for these records. Unknown-field policy belongs to each record type.
package jsoncompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decode reads an owned record. dst is an alias without UnmarshalJSON methods;
// defaultAll applies struct-level defaults already installed by the caller.
// A field tagged wire:"default" may be absent. Pointers are optional, never
// conflated with a required scalar's explicit null. Failed reads leave dst alone.
func Decode(data []byte, dst any, strict, defaultAll bool) error {
	original := reflect.ValueOf(dst).Elem()
	v := reflect.New(original.Type()).Elem()
	v.Set(original)
	typ := v.Type()
	fields := make(map[string]int, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		fields[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = i
	}
	seen := make([]bool, typ.NumField())
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return err
	}
	sequence := token == json.Delim('[')
	switch token {
	case json.Delim('{'):
		for dec.More() {
			start := dec.InputOffset()
			key, err := dec.Token()
			if err != nil {
				return err
			}
			if err := validStrings(data[start:dec.InputOffset()]); err != nil {
				return err
			}
			name := key.(string)
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return err
			}
			i, ok := fields[name]
			if !ok {
				if strict {
					return fmt.Errorf("unknown field %q", name)
				}
				continue
			}
			if seen[i] {
				return fmt.Errorf("duplicate field %q", name)
			}
			seen[i] = true
			if err := decodeValue(raw, v.Field(i)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case json.Delim('['):
		// serde also accepts a record's fields in declaration order.
		for i := 0; dec.More(); i++ {
			if i >= typ.NumField() {
				return fmt.Errorf("too many record fields")
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return err
			}
			seen[i] = true
			if err := decodeValue(raw, v.Field(i)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("expected a record")
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !seen[i] && !defaultAll && f.Tag.Get("wire") != "default" && (sequence || f.Type.Kind() != reflect.Pointer) {
			return fmt.Errorf("missing field %q", strings.Split(f.Tag.Get("json"), ",")[0])
		}
		// A Rust Vec or map never serializes as null.
		if v.Field(i).Kind() == reflect.Slice && v.Field(i).IsNil() {
			v.Field(i).Set(reflect.MakeSlice(f.Type, 0, 0))
		}
		if v.Field(i).Kind() == reflect.Map && v.Field(i).IsNil() {
			v.Field(i).Set(reflect.MakeMap(f.Type))
		}
	}
	original.Set(v)
	return nil
}

func decodeValue(raw []byte, v reflect.Value) error {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) && v.Kind() != reflect.Pointer && v.Kind() != reflect.Interface {
		return fmt.Errorf("null is not allowed")
	}
	// The nested type owns its defaults, field strictness, and enums.
	if _, ok := v.Addr().Interface().(json.Unmarshaler); ok {
		return json.Unmarshal(raw, v.Addr().Interface())
	}
	switch v.Kind() {
	case reflect.Map:
		dec := json.NewDecoder(bytes.NewReader(raw))
		token, err := dec.Token()
		if err != nil || token != json.Delim('{') {
			return fmt.Errorf("expected a map")
		}
		result := reflect.MakeMap(v.Type())
		for dec.More() {
			start := dec.InputOffset()
			key, err := dec.Token()
			if err != nil {
				return err
			}
			if err := validStrings(raw[start:dec.InputOffset()]); err != nil {
				return err
			}
			var data json.RawMessage
			if err := dec.Decode(&data); err != nil {
				return err
			}
			item := reflect.New(v.Type().Elem()).Elem()
			if err := decodeValue(data, item); err != nil {
				return err
			}
			result.SetMapIndex(reflect.ValueOf(key.(string)), item)
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		v.Set(result)
		return nil
	case reflect.Pointer:
		if bytes.Equal(raw, []byte("null")) {
			v.SetZero()
			return nil
		}
		result := reflect.New(v.Type().Elem())
		if err := decodeValue(raw, result.Elem()); err != nil {
			return err
		}
		v.Set(result)
		return nil
	case reflect.String:
		if err := validStrings(raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, v.Addr().Interface())
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		result := reflect.MakeSlice(v.Type(), len(values), len(values))
		for i, data := range values {
			if err := decodeValue(data, result.Index(i)); err != nil {
				return err
			}
		}
		v.Set(result)
		return nil
	case reflect.Interface:
		if err := validStrings(raw); err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return err
		}
		normalized, err := normalizeNumbers(value)
		if err != nil {
			return err
		}
		if normalized == nil {
			v.SetZero()
		} else {
			v.Set(reflect.ValueOf(normalized))
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// serde classifies -0 as a float, so integer fields must reject it.
		if bytes.Equal(raw, []byte("-0")) {
			return fmt.Errorf("expected an integer, got -0")
		}
	}
	return json.Unmarshal(raw, v.Addr().Interface())
}

// Marshal produces compact serde-compatible bytes: declaration-order fields,
// sorted map keys, literal Unicode/HTML, and no trailing newline.
func Marshal(value any) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	data := bytes.TrimSuffix(out.Bytes(), []byte{'\n'})
	// encoding/json always escapes JS line separators, even with EscapeHTML=false.
	// Consume escape pairs so a literal backslash-u sequence is never rewritten.
	result := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] == '\\' && i+1 < len(data) {
			if i+6 <= len(data) && (string(data[i:i+6]) == `\u2028` || string(data[i:i+6]) == `\u2029`) {
				if data[i+5] == '8' {
					result = append(result, 0xe2, 0x80, 0xa8)
				} else {
					result = append(result, 0xe2, 0x80, 0xa9)
				}
				i += 5
				continue
			}
			result = append(result, data[i], data[i+1])
			i++
			continue
		}
		result = append(result, data[i])
	}
	return result, nil
}

// Record serializes a value alias, keeping empty Go containers compatible with
// Rust's non-null vectors/maps. A shallow copy suffices; no elements are changed.
func Record(value any) ([]byte, error) {
	v := reflect.ValueOf(value)
	copy := reflect.New(v.Type()).Elem()
	copy.Set(v)
	for i := 0; i < copy.NumField(); i++ {
		f := copy.Field(i)
		switch f.Kind() {
		case reflect.Slice:
			if f.IsNil() {
				f.Set(reflect.MakeSlice(f.Type(), 0, 0))
			}
		case reflect.Map:
			if f.IsNil() {
				f.Set(reflect.MakeMap(f.Type()))
			}
		}
	}
	return Marshal(copy.Interface())
}

// Clone gives snapshot owners separate maps, slices, and optional values, also
// inside opaque JSON evidence. Scalars and immutable strings are copied as-is.
func Clone[T any](value T) T { return clone(reflect.ValueOf(value)).Interface().(T) }
func clone(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(clone(v.Elem()))
		return c
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		c := reflect.New(v.Type()).Elem()
		c.Set(clone(v.Elem()))
		return c
	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			c.Field(i).Set(clone(v.Field(i)))
		}
		return c
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			c.SetMapIndex(iter.Key(), clone(iter.Value()))
		}
		return c
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(clone(v.Index(i)))
		}
		return c
	default:
		return v
	}
}

// Go otherwise replaces invalid UTF-8 or unmatched surrogate escapes with U+FFFD;
// serde rejects them. Validate string escapes before allowing that replacement.
func validStrings(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			break
		}
		if data[i] != 'u' {
			continue
		}
		if i+5 > len(data) {
			return fmt.Errorf("invalid Unicode escape")
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return err
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return fmt.Errorf("unpaired low surrogate")
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+7 > len(data) || string(data[i+1:i+3]) != `\u` {
				return fmt.Errorf("unpaired high surrogate")
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return fmt.Errorf("unpaired high surrogate")
			}
			i += 6
		}
	}
	return nil
}

func Enum(data []byte, names []string) (uint8, error) {
	if err := validStrings(data); err != nil {
		return 0, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return 0, err
	}
	name, ok := token.(string)
	if token == json.Delim('{') {
		token, err = dec.Token()
		if err != nil {
			return 0, err
		}
		name, ok = token.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, err
		}
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || dec.More() {
			return 0, fmt.Errorf("invalid enum payload")
		}
		if _, err := dec.Token(); err != nil {
			return 0, err
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, fmt.Errorf("trailing enum data")
	}
	if ok {
		for i, allowed := range names {
			if name == allowed {
				return uint8(i), nil
			}
		}
	}
	return 0, fmt.Errorf("invalid enum value %q", name)
}

// serde_json keeps signed/unsigned 64-bit integers exact and uses finite f64
// otherwise. Match both its parsing and shortest float spelling, including
// integer-valued floats.
func normalizeNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		s := string(v)
		if !strings.ContainsAny(s, ".eE") && s != "-0" {
			if strings.HasPrefix(s, "-") {
				if _, err := strconv.ParseInt(s, 10, 64); err == nil {
					return v, nil
				}
			} else {
				if _, err := strconv.ParseUint(s, 10, 64); err == nil {
					return v, nil
				}
			}
		}
		n, err := parseSerdeFloat(s)
		if err != nil {
			return nil, err
		}
		return json.Number(FormatFloat(n)), nil
	case []any:
		for i, item := range v {
			normalized, err := normalizeNumbers(item)
			if err != nil {
				return nil, err
			}
			v[i] = normalized
		}
		return v, nil
	case map[string]any:
		for key, item := range v {
			normalized, err := normalizeNumbers(item)
			if err != nil {
				return nil, err
			}
			v[key] = normalized
		}
		return v, nil
	default:
		return value, nil
	}
}

// FormatFloat spells a finite f64 the way serde_json (ryu) does: fixed notation
// for exponents -5 through 15 with an explicit decimal for integral values, and
// unpadded exponent notation otherwise.
func FormatFloat(n float64) string {
	repr := strconv.FormatFloat(n, 'e', -1, 64)
	parts := strings.Split(repr, "e")
	exponent, _ := strconv.Atoi(parts[1])
	if exponent >= -5 && exponent < 16 {
		repr = strconv.FormatFloat(n, 'f', -1, 64)
		if !strings.Contains(repr, ".") {
			repr += ".0"
		}
		return repr
	}
	repr = parts[0] + "e"
	if exponent >= 0 {
		repr += "+"
	}
	return repr + strconv.Itoa(exponent)
}

// Float64 marshals with serde's float spelling, so an integral value such as
// 60.0 keeps its decimal point instead of encoding/json's 60.
type Float64 float64

func (f Float64) MarshalJSON() ([]byte, error) {
	return []byte(FormatFloat(float64(f))), nil
}

func EnumName(value uint8, names []string) string {
	if int(value) >= len(names) {
		return ""
	}
	return names[value]
}
func MarshalEnum(value uint8, names []string) ([]byte, error) {
	name := EnumName(value, names)
	if name == "" {
		return nil, fmt.Errorf("invalid enum value %d", value)
	}
	return json.Marshal(name)
}

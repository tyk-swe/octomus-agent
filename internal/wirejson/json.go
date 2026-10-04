package wirejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Error struct{ inner error }

func (e *Error) Error() string { return e.inner.Error() }
func (e *Error) Unwrap() error { return e.inner }

func marked(err error) error {
	if err == nil {
		return nil
	}
	var target *Error
	if errors.As(err, &target) {
		return err
	}
	return &Error{inner: err}
}

// DecodeStrict decodes a struct whose every non-pointer field must be present and whose keys must all be known:
// operator requests and model answers.
func DecodeStrict[T any](data []byte, dst *T) error {
	var decoded T
	if err := marked(decode(data, &decoded, true, false)); err != nil {
		return err
	}
	*dst = decoded
	return nil
}

// DecodeRecord decodes a saved record: unknown keys are ignored, so older binaries can read newer records, but every
// other rule (required fields, duplicates, exact case, no null, valid UTF-8, no trailing data) holds.
func DecodeRecord[T any](data []byte, dst *T) error {
	var decoded T
	if err := marked(decode(data, &decoded, false, false)); err != nil {
		return err
	}
	*dst = decoded
	return nil
}

// DecodeDefaults decodes strictly into dst, whose current values stand for every absent field; dst is unchanged on
// failure.
func DecodeDefaults(data []byte, dst any) error {
	return marked(decode(data, dst, true, true))
}

func decode(data []byte, dst any, strict, defaultAll bool) error {
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
	switch token {
	case json.Delim('{'):
		for dec.More() {
			start := dec.InputOffset()
			key, err := dec.Token()
			if err != nil {
				return err
			}
			if err := ValidStrings(data[start:dec.InputOffset()]); err != nil {
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
	default:
		return fmt.Errorf("expected an object")
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !seen[i] && !defaultAll && f.Tag.Get("wire") != "default" && f.Type.Kind() != reflect.Pointer {
			return fmt.Errorf("missing field %q", strings.Split(f.Tag.Get("json"), ",")[0])
		}
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
		seen := make(map[string]struct{})
		for dec.More() {
			start := dec.InputOffset()
			key, err := dec.Token()
			if err != nil {
				return err
			}
			if err := ValidStrings(raw[start:dec.InputOffset()]); err != nil {
				return err
			}
			name := key.(string)
			if _, ok := seen[name]; ok {
				return fmt.Errorf("duplicate field %q", name)
			}
			seen[name] = struct{}{}
			var data json.RawMessage
			if err := dec.Decode(&data); err != nil {
				return err
			}
			item := reflect.New(v.Type().Elem()).Elem()
			if err := decodeValue(data, item); err != nil {
				return err
			}
			result.SetMapIndex(reflect.ValueOf(name), item)
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
		if err := ValidStrings(raw); err != nil {
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
		if err := ValidStrings(raw); err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return err
		}
		if value == nil {
			v.SetZero()
		} else {
			v.Set(reflect.ValueOf(value))
		}
		return nil
	}
	return json.Unmarshal(raw, v.Addr().Interface())
}

func Marshal(value any) ([]byte, error) {
	return markedPair(json.Marshal(value))
}

func markedPair(data []byte, err error) ([]byte, error) {
	return data, marked(err)
}

func Equal(a, b any) bool {
	left, err := Marshal(a)
	if err != nil {
		return false
	}
	right, err := Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func Generic(value any) (any, error) {
	var result any
	if err := genericInto(value, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func GenericMap(value any) (map[string]any, error) {
	var result map[string]any
	if err := genericInto(value, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func genericInto(value, dst any) error {
	data, err := Marshal(value)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(dst)
}

func Record(value any) ([]byte, error) {
	return markedPair(record(value))
}

func record(value any) ([]byte, error) {
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
	return json.Marshal(copy.Interface())
}

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

func ValidStrings(data []byte) error {
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

// EnumName is the wire name of an enum value, or "" outside names.
func EnumName[T ~uint8](value T, names []string) string {
	if int(value) >= len(names) {
		return ""
	}
	return names[value]
}

// EnumText implements an enum's MarshalText: its wire name, or an *Error for a
// value outside names.
func EnumText[T ~uint8](value T, names []string) ([]byte, error) {
	name := EnumName(value, names)
	if name == "" {
		return nil, &Error{inner: fmt.Errorf("invalid enum value %d", value)}
	}
	return []byte(name), nil
}

// ParseEnum implements an enum's UnmarshalText: text must equal one name
// exactly. Anything else is an *Error naming the value and the accepted names.
func ParseEnum(text []byte, names []string) (uint8, error) {
	for i, allowed := range names {
		if string(text) == allowed {
			return uint8(i), nil
		}
	}
	return 0, &Error{inner: fmt.Errorf("invalid enum value %q (expected one of: %s)", text, strings.Join(names, ", "))}
}

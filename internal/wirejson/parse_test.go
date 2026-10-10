package wirejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  string
		want any
	}{
		{`null`, nil},
		{`true`, true},
		{`false`, false},
		{`12345678901234567890`, json.Number("12345678901234567890")},
		{`-1.25e+100`, json.Number("-1.25e+100")},
		{`"\ud83d\ude00"`, "😀"},
		{`"\\ud800"`, `\ud800`},
		{`[]`, []any{}},
		{`{}`, map[string]any{}},
		{` [{"key":null},{"key":[true,"value",1]}] `, []any{
			map[string]any{"key": nil},
			map[string]any{"key": []any{true, "value", json.Number("1")}},
		}},
	} {
		value, err := Parse([]byte(tc.raw))
		if err != nil || !reflect.DeepEqual(value, tc.want) {
			t.Errorf("Parse(%s) = %#v, %v; want %#v", tc.raw, value, err, tc.want)
		}
	}
}

func TestParseRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw     string
		message string
	}{
		{`{"key":null,"key":1}`, `duplicate field "key"`},
		{`{"key":1,"\u006bey":2}`, `duplicate field "key"`},
		{`{"outer":[{"key":1,"key":2}]}`, `duplicate field "key"`},
		{"{\"key\":\"\xff\"}", "invalid UTF-8"},
		{`"\ud800"`, "unpaired high surrogate"},
		{`{"\udc00":null}`, "unpaired low surrogate"},
		{`{} []`, "trailing JSON data"},
		{`{}x`, "trailing JSON data"},
		{``, ""},
		{`{`, ""},
		{`[`, ""},
		{`{"key":}`, ""},
		{`{"key":1,}`, ""},
		{`[1,]`, ""},
		{`[1}`, ""},
		{`{1:2}`, ""},
	} {
		value, err := Parse([]byte(tc.raw))
		var typed *Error
		if value != nil || !errors.As(err, &typed) {
			t.Errorf("Parse(%s) = %#v, %v; want nil and a typed error", tc.raw, value, err)
			continue
		}
		if tc.message != "" && err.Error() != tc.message {
			t.Errorf("Parse(%s) = %q; want %q", tc.raw, err, tc.message)
		}
	}
}

func TestParseNestingBound(t *testing.T) {
	t.Parallel()
	for _, depth := range []int{10000, 10001} {
		data := []byte(strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth))
		_, err := Parse(data)
		if (err == nil) != json.Valid(data) {
			t.Fatalf("Parse at depth %d = %v; want encoding/json's nesting bound", depth, err)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, raw := range []string{
		`null`, `[]`, `{"key":1}`, `{"key":null,"key":1}`, `[{"key":true}]`,
		`12345678901234567890`, `"\ud83d\ude00"`, `"\ud800"`, `{"key":1,}`, `{} []`,
	} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := Parse(data)
		if err != nil {
			var typed *Error
			if value != nil || !errors.As(err, &typed) {
				t.Fatalf("refused JSON returned %#v, %v; want nil and a typed error", value, err)
			}
			return
		}
		if !json.Valid(data) {
			t.Fatalf("accepted invalid JSON: %q", data)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var want any
		if err := dec.Decode(&want); err != nil || !reflect.DeepEqual(value, want) {
			t.Fatalf("Parse(%q) = %#v; encoding/json = %#v, %v", data, value, want, err)
		}
	})
}

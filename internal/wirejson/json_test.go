// Strict typed JSON decoding: absent fields, refusals and accepted documents.

package wirejson

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type decodeRecord struct {
	S string         `json:"s"`
	P *string        `json:"p"`
	D string         `json:"d" wire:"default"`
	L []string       `json:"l" wire:"default"`
	M map[string]int `json:"m" wire:"default"`
	A any            `json:"a" wire:"default"`
}

func TestDecodeAbsentFields(t *testing.T) {
	stale := "stale"
	dst := decodeRecord{P: &stale, D: "kept"}
	if err := decodeAs([]byte(`{"s":"x"}`), &dst, true, false); err != nil {
		t.Fatal(err)
	}
	if dst.S != "x" || dst.P == nil || *dst.P != "stale" || dst.D != "kept" || dst.L == nil || len(dst.L) != 0 {
		t.Fatalf("absent fields = %+v; want P and D kept and L empty", dst)
	}
	before := dst
	for _, raw := range []string{`{"s":"y","unknown":1}`, `{"s":"y","s":"z"}`, `{"p":"y"}`, `{"s":"y","l":null}`, `{"s":"y"} {}`} {
		err := decodeAs([]byte(raw), &dst, true, false)
		var typed *Error
		if !errors.As(err, &typed) {
			t.Fatalf("decodeAs(%s) = %v; want a typed error", raw, err)
		}
		if dst.S != before.S || dst.P != before.P || dst.D != before.D || len(dst.L) != 0 {
			t.Fatalf("decodeAs(%s) changed dst to %+v", raw, dst)
		}
	}
	if err := decodeAs([]byte(`{"s":"y"}`), &decodeRecord{}, false, false); err != nil {
		t.Fatalf("absent pointer and default fields: %v", err)
	}
	if err := decodeAs([]byte(`{}`), &decodeRecord{}, false, true); err != nil {
		t.Fatalf("defaultAll with every field absent: %v", err)
	}
}

func TestDecodeRefusals(t *testing.T) {
	kept := "kept"
	start := decodeRecord{S: "start", P: &kept, D: "default", L: []string{"l"}, M: map[string]int{"m": 1}, A: map[string]any{"a": []any{"b"}}}
	for _, tc := range []struct {
		raw     string
		lenient bool
		message string
	}{
		{raw: `{"s":"x","\ud800x":1}`, lenient: true, message: "unpaired high surrogate"},
		{raw: "{\"s\":\"x\",\"\xff\":1}", lenient: true, message: "invalid UTF-8"},
		{raw: `{"s":"\udc00"}`, message: "s: unpaired low surrogate"},
		{raw: `{"s":"\ud800A"}`, message: "s: unpaired high surrogate"},
		{raw: "{\"s\":\"\xff\"}", message: "s: invalid UTF-8"},
		{raw: `{"s":"x","p":"\udc00"}`, message: "p: unpaired low surrogate"},
		{raw: `{"s":"x","l":["ok","\ud800"]}`, message: "l: unpaired high surrogate"},
		{raw: `{"s":"x","m":{"\udc00":1}}`, message: "m: unpaired low surrogate"},
		{raw: `{"s":"x","a":{"k":["\ud800"]}}`, message: "a: unpaired high surrogate"},
		{raw: `{"s":"x","a":{"\udc00":1}}`, message: "a: unpaired low surrogate"},
		{raw: `{"s":"x","z":1}`, message: `unknown field "z"`},
		{raw: `{"s":"x","s":"y"}`, message: `duplicate field "s"`},
		{raw: `{"s":"x","\u0073":"y"}`, message: `duplicate field "s"`},
		{raw: `{"s":"x","m":{"k":1,"k":2}}`, message: `m: duplicate field "k"`},
		{raw: `{"s":"x","a":{"k":1,"k":2}}`, message: `a: duplicate field "k"`},
		{raw: `{"s":"x","a":[{"k":null,"\u006b":2}]}`, message: `a: duplicate field "k"`},
		{raw: `{"s":"x","z":1,"z":2}`, lenient: true, message: `duplicate field "z"`},
		{raw: `{"s":"x","z":1,"\u007a":2}`, lenient: true, message: `duplicate field "z"`},
		{raw: `{"s":"x","z":{"k":1,"k":2}}`, lenient: true, message: `z: duplicate field "k"`},
		{raw: `{"s":"x","z":[{"k":"\ud800"}]}`, lenient: true, message: "z: unpaired high surrogate"},
		{raw: "{\"s\":\"x\",\"z\":\"\xff\"}", lenient: true, message: "z: invalid UTF-8"},
		{raw: `{"s":"x","m":{"k":1,"\u006b":2}}`, message: `m: duplicate field "k"`},
		{raw: `{}`, message: `missing field "s"`},
		{raw: `{"p":"x","d":"x"}`, lenient: true, message: `missing field "s"`},
		{raw: `{"s":null}`, message: "s: null is not allowed"},
		{raw: `{"s":"x","l":null}`, message: "l: null is not allowed"},
		{raw: `{"s":"x","m":null}`, message: "m: null is not allowed"},
		{raw: `{"s":"x","m":[]}`, message: "m: expected a map"},
		{raw: `{"s":"x"} {}`, message: "trailing JSON data"},
		{raw: `{"s":"x"}x`, message: "trailing JSON data"},
		{raw: `[]`, message: "expected an object"},
		{raw: `"s"`, message: "expected an object"},
		{raw: `null`, message: "expected an object"},
		{raw: ``},
		{raw: `{"s":"x"`},
		{raw: `{"s":1}`},
		{raw: `{"s":"x","l":{}}`},
	} {
		dst := Clone(start)
		strict := !tc.lenient
		err := decodeAs([]byte(tc.raw), &dst, strict, false)
		var typed *Error
		if !errors.As(err, &typed) {
			t.Errorf("decodeAs(%s, strict=%t) = %v; want a typed error", tc.raw, strict, err)
			continue
		}
		if tc.message != "" && err.Error() != tc.message {
			t.Errorf("decodeAs(%s, strict=%t) = %q; want %q", tc.raw, strict, err, tc.message)
		}
		if !reflect.DeepEqual(dst, start) {
			t.Errorf("decodeAs(%s) changed dst to %+v", tc.raw, dst)
		}
	}
}

func TestDecodeWellFormed(t *testing.T) {
	y := "y"
	empty := func(r decodeRecord) decodeRecord {
		if r.L == nil {
			r.L = []string{}
		}
		if r.M == nil {
			r.M = map[string]int{}
		}
		return r
	}
	for _, tc := range []struct {
		raw     string
		lenient bool
		want    decodeRecord
	}{
		{raw: `{"s":"x"}`, want: empty(decodeRecord{S: "x"})},
		{raw: ` {"s" : "x"} `, want: empty(decodeRecord{S: "x"})},
		{raw: `{"s":"\ud83d\ude00"}`, want: empty(decodeRecord{S: "😀"})},
		{raw: `{"s":"\\ud800"}`, want: empty(decodeRecord{S: `\ud800`})},
		{raw: `{"s":"x","z":{"deep":["\\"]}}`, lenient: true, want: empty(decodeRecord{S: "x"})},
		{raw: `{"s":"x","l":[],"m":{}}`, want: empty(decodeRecord{S: "x"})},
		{
			raw:  `{"s":"x","p":"y","d":"z","l":["a","b"],"m":{"k":1,"j":2},"a":{"n":12345678901234567890,"list":[true,null,"v"]}}`,
			want: decodeRecord{S: "x", P: &y, D: "z", L: []string{"a", "b"}, M: map[string]int{"k": 1, "j": 2}, A: map[string]any{"n": json.Number("12345678901234567890"), "list": []any{true, nil, "v"}}},
		},
	} {
		var dst decodeRecord
		if err := decodeAs([]byte(tc.raw), &dst, !tc.lenient, false); err != nil {
			t.Errorf("decodeAs(%s) = %v", tc.raw, err)
			continue
		}
		if !reflect.DeepEqual(dst, tc.want) {
			t.Errorf("decodeAs(%s) = %#v; want %#v", tc.raw, dst, tc.want)
		}
	}

	stale := "stale"
	dst := decodeRecord{P: &stale, A: "stale"}
	if err := decodeAs([]byte(`{"s":"x","p":null,"a":null}`), &dst, true, false); err != nil || dst.P != nil || dst.A != nil {
		t.Fatalf("explicit nulls = %+v, %v; want P and A cleared", dst, err)
	}

	dst = decodeRecord{S: "kept", L: []string{"kept"}}
	if err := decodeAs([]byte(`{"d":"x"}`), &dst, true, true); err != nil {
		t.Fatal(err)
	}
	if want := (decodeRecord{S: "kept", D: "x", L: []string{"kept"}, M: map[string]int{}}); !reflect.DeepEqual(dst, want) {
		t.Fatalf("defaultAll = %#v; want %#v", dst, want)
	}
	if err := decodeAs([]byte(`{"z":1}`), &dst, true, true); err == nil || err.Error() != `unknown field "z"` {
		t.Fatalf("defaultAll with an unknown field = %v; want it refused", err)
	}
}

type testEnum uint8

var testEnumNames = []string{"off", "on"}

func (v testEnum) MarshalText() ([]byte, error)     { return EnumText(v, testEnumNames) }
func (v *testEnum) UnmarshalText(text []byte) error { return ParseEnum(v, text, testEnumNames) }

type enumRecord struct {
	Mode   testEnum  `json:"mode"`
	Option *testEnum `json:"option"`
}

func TestEnumTextNamesEveryValueAndParseEnumAcceptsOnlyExactNames(t *testing.T) {
	for i, name := range testEnumNames {
		text, err := EnumText(uint8(i), testEnumNames)
		if err != nil || string(text) != name {
			t.Fatalf("EnumText(%d) = %q, %v; want %q", i, text, err, name)
		}
		var value testEnum
		if err := ParseEnum(&value, []byte(name), testEnumNames); err != nil || int(value) != i {
			t.Fatalf("ParseEnum(%q) = %d, %v; want %d", name, value, err, i)
		}
	}
	var typed *Error
	if text, err := EnumText(uint8(len(testEnumNames)), testEnumNames); !errors.As(err, &typed) {
		t.Fatalf("EnumText(%d) = %q, %v; want a typed error", len(testEnumNames), text, err)
	}
	for _, text := range []string{"", "On", " on", "on ", "o", "onn", "null", "\"on\""} {
		value := testEnum(1)
		err := ParseEnum(&value, []byte(text), testEnumNames)
		if !errors.As(err, &typed) {
			t.Fatalf("ParseEnum(%q) = %v; want a typed error", text, err)
		}
		if value != 1 {
			t.Fatalf("ParseEnum(%q) set the value to %d; a refused name must leave it unchanged", text, value)
		}
		if want := fmt.Sprintf("invalid enum value %q (expected one of: off, on)", text); err.Error() != want {
			t.Fatalf("ParseEnum(%q) = %q; want %q", text, err, want)
		}
	}
}

func TestDecodeHoldsEnumFieldsToTheirExactNames(t *testing.T) {
	var dst enumRecord
	if err := decodeAs([]byte(`{"mode":"on","option":null}`), &dst, true, false); err != nil || dst.Mode != 1 || dst.Option != nil {
		t.Fatalf("Decode = %+v, %v", dst, err)
	}
	if err := decodeAs([]byte(`{"mode":"off","option":"on"}`), &dst, true, false); err != nil || dst.Mode != 0 || dst.Option == nil || *dst.Option != 1 {
		t.Fatalf("Decode = %+v, %v", dst, err)
	}
	if data, err := json.Marshal(dst); err != nil || string(data) != `{"mode":"off","option":"on"}` {
		t.Fatalf("json.Marshal = %s, %v", data, err)
	}
	for raw, message := range map[string]string{
		`{"mode":null}`:                   "mode: null is not allowed",
		`{"mode":1}`:                      "",
		`{"mode":true}`:                   "",
		`{"mode":"On"}`:                   `mode: invalid enum value "On" (expected one of: off, on)`,
		`{"mode":"on","option":2}`:        "",
		`{"mode":"on","option":"x"}`:      `option: invalid enum value "x" (expected one of: off, on)`,
		`{"mode":"on","option":[]}`:       "",
		`{"mode":"on","option":"\ud800"}`: "",
	} {
		dst := enumRecord{Mode: 1}
		err := decodeAs([]byte(raw), &dst, true, false)
		var typed *Error
		if !errors.As(err, &typed) {
			t.Errorf("decodeAs(%s) = %v; want a typed error", raw, err)
			continue
		}
		if message != "" && err.Error() != message {
			t.Errorf("decodeAs(%s) = %q; want %q", raw, err, message)
		}
		if dst.Mode != 1 || dst.Option != nil {
			t.Errorf("decodeAs(%s) changed dst to %+v", raw, dst)
		}
	}
}

func decodeAs(data []byte, dst any, strict, defaultAll bool) error {
	return marked(decode(data, dst, strict, defaultAll))
}

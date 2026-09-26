package wirejson

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type testEnum uint8

var testEnumNames = []string{"alpha", "beta"}

func TestUnmarshalEnumChangesDestinationOnlyOnSuccess(t *testing.T) {
	dst := testEnum(1)
	if err := UnmarshalEnum([]byte(`"alpha"`), testEnumNames, &dst); err != nil || dst != 0 {
		t.Fatalf("alpha = %d, %v", dst, err)
	}
	dst = 1
	for _, tc := range []struct{ raw, message string }{
		{`"gamma"`, `invalid enum value "gamma" (expected one of: alpha, beta)`},
		{`1`, "expected an enum string"},
		{`"alpha" "beta"`, "trailing enum data"},
		{"\"\xff\"", "invalid UTF-8"},
		{`"\udc00"`, "unpaired low surrogate"},
		{`"\ud800"`, "unpaired high surrogate"},
		{`null`, "expected an enum string"},
		{`""`, `invalid enum value "" (expected one of: alpha, beta)`},
	} {
		err := UnmarshalEnum([]byte(tc.raw), testEnumNames, &dst)
		var typed *Error
		if !errors.As(err, &typed) || err.Error() != tc.message {
			t.Errorf("UnmarshalEnum(%s) = %v; want typed %q", tc.raw, err, tc.message)
		}
		if dst != 1 {
			t.Fatalf("UnmarshalEnum(%s) changed dst to %d", tc.raw, dst)
		}
	}
}

func TestMarshalEnumRefusesOutOfRangeValues(t *testing.T) {
	if data, err := MarshalEnum(testEnum(1), testEnumNames); err != nil || string(data) != `"beta"` {
		t.Fatalf("MarshalEnum(1) = %s, %v", data, err)
	}
	for _, value := range []testEnum{2, 7} {
		data, err := MarshalEnum(value, testEnumNames)
		var typed *Error
		if !errors.As(err, &typed) || data != nil || err.Error() != fmt.Sprintf("invalid enum value %d", value) {
			t.Fatalf("MarshalEnum(%d) = %s, %v; want a typed range error", value, data, err)
		}
	}
	if EnumName(testEnum(0), testEnumNames) != "alpha" || EnumName(testEnum(2), testEnumNames) != "" {
		t.Fatal("EnumName range")
	}
}

type decodeRecord struct {
	S string         `json:"s"`
	P *string        `json:"p"`
	D string         `json:"d" wire:"default"`
	L []string       `json:"l" wire:"default"`
	M map[string]int `json:"m" wire:"default"`
	A any            `json:"a" wire:"default"`
}

// Callers rely on absent fields keeping dst's values for installed defaults,
// which is why record decoders must start from a zero value.
func TestDecodeKeepsAbsentFieldsAndChangesDstOnlyOnSuccess(t *testing.T) {
	stale := "stale"
	dst := decodeRecord{P: &stale, D: "kept"}
	if err := Decode([]byte(`{"s":"x"}`), &dst, true, false); err != nil {
		t.Fatal(err)
	}
	if dst.S != "x" || dst.P == nil || *dst.P != "stale" || dst.D != "kept" || dst.L == nil || len(dst.L) != 0 {
		t.Fatalf("absent fields = %+v; want P and D kept and L empty", dst)
	}
	before := dst
	for _, raw := range []string{`{"s":"y","unknown":1}`, `{"s":"y","s":"z"}`, `{"p":"y"}`, `{"s":"y","l":null}`, `{"s":"y"} {}`} {
		err := Decode([]byte(raw), &dst, true, false)
		var typed *Error
		if !errors.As(err, &typed) {
			t.Fatalf("Decode(%s) = %v; want a typed error", raw, err)
		}
		if dst.S != before.S || dst.P != before.P || dst.D != before.D || len(dst.L) != 0 {
			t.Fatalf("Decode(%s) changed dst to %+v", raw, dst)
		}
	}
	if err := Decode([]byte(`{"s":"y"}`), &decodeRecord{}, false, false); err != nil {
		t.Fatalf("absent pointer and default fields: %v", err)
	}
	if err := Decode([]byte(`{}`), &decodeRecord{}, false, true); err != nil {
		t.Fatalf("defaultAll with every field absent: %v", err)
	}
}

// Go's decoder turns lone surrogates and invalid bytes into U+FFFD, so the
// scanner must refuse them first, and must not lose track of where a string
// ends when it meets an escaped quote or backslash.
func TestValidStringsRefusesMalformedUnicode(t *testing.T) {
	for _, tc := range []struct{ raw, message string }{
		{`"\ud800"`, "unpaired high surrogate"},
		{`"\uDBFF"`, "unpaired high surrogate"},
		{`"\udc00"`, "unpaired low surrogate"},
		{`"\uDFFF"`, "unpaired low surrogate"},
		{`"\ud800A"`, "unpaired high surrogate"},
		{`"\ud800\u0041"`, "unpaired high surrogate"},
		{`"\ud800\ud800"`, "unpaired high surrogate"},
		{`"\ud800\\udc00"`, "unpaired high surrogate"},
		{`"\u12"`, "invalid Unicode escape"},
		{"\"\xff\"", "invalid UTF-8"},
		{"\"\xed\xa0\x80\"", "invalid UTF-8"},
		{`["\"\udc00"]`, "unpaired low surrogate"},
		{`["\\", "\udc00"]`, "unpaired low surrogate"},
		{`{"ok":"\\","\ud800x":1}`, "unpaired high surrogate"},
	} {
		err := ValidStrings([]byte(tc.raw))
		if err == nil || err.Error() != tc.message {
			t.Errorf("ValidStrings(%s) = %v; want %q", tc.raw, err, tc.message)
		}
		// Plain errors: the runner reports these as protocol failures, and a
		// marked *Error would be classified as an internal codec failure.
		var marked *Error
		if errors.As(err, &marked) {
			t.Errorf("ValidStrings(%s) returned a marked *Error", tc.raw)
		}
	}
	for _, raw := range []string{
		`"\ud83d\ude00"`,
		`"\uD83D\uDE00"`,
		`"\\ud800"`,
		`"\\u0000"`,
		`"\u0000"`,
		`"a\"b"`,
		`"\u00e9 é"`,
		`["\\\\", "\"", "\ud83d\ude00"]`,
		`{"k":[1,true,null,"\ud83d\ude00"]}`,
	} {
		if err := ValidStrings([]byte(raw)); err != nil {
			t.Errorf("ValidStrings(%s) = %v; want nil", raw, err)
		}
	}
}

func TestDecodeRefusesAmbiguousOrMalformedObjects(t *testing.T) {
	kept := "kept"
	start := decodeRecord{S: "start", P: &kept, D: "default", L: []string{"l"}, M: map[string]int{"m": 1}, A: map[string]any{"a": []any{"b"}}}
	for _, tc := range []struct {
		raw     string
		lenient bool
		message string // empty when the text comes from encoding/json
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
		err := Decode([]byte(tc.raw), &dst, strict, false)
		var typed *Error
		if !errors.As(err, &typed) {
			t.Errorf("Decode(%s, strict=%t) = %v; want a typed error", tc.raw, strict, err)
			continue
		}
		if tc.message != "" && err.Error() != tc.message {
			t.Errorf("Decode(%s, strict=%t) = %q; want %q", tc.raw, strict, err, tc.message)
		}
		if !reflect.DeepEqual(dst, start) {
			t.Errorf("Decode(%s) changed dst to %+v", tc.raw, dst)
		}
	}
}

func TestDecodeAcceptsWellFormedObjects(t *testing.T) {
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
		// Opaque values keep numbers exact rather than rounding them to float64.
		{
			raw:  `{"s":"x","p":"y","d":"z","l":["a","b"],"m":{"k":1,"j":2},"a":{"n":12345678901234567890,"list":[true,null,"v"]}}`,
			want: decodeRecord{S: "x", P: &y, D: "z", L: []string{"a", "b"}, M: map[string]int{"k": 1, "j": 2}, A: map[string]any{"n": json.Number("12345678901234567890"), "list": []any{true, nil, "v"}}},
		},
	} {
		var dst decodeRecord
		if err := Decode([]byte(tc.raw), &dst, !tc.lenient, false); err != nil {
			t.Errorf("Decode(%s) = %v", tc.raw, err)
			continue
		}
		if !reflect.DeepEqual(dst, tc.want) {
			t.Errorf("Decode(%s) = %#v; want %#v", tc.raw, dst, tc.want)
		}
	}

	// An explicit null clears an optional value, where absence keeps it.
	stale := "stale"
	dst := decodeRecord{P: &stale, A: "stale"}
	if err := Decode([]byte(`{"s":"x","p":null,"a":null}`), &dst, true, false); err != nil || dst.P != nil || dst.A != nil {
		t.Fatalf("explicit nulls = %+v, %v; want P and A cleared", dst, err)
	}

	// defaultAll makes every field optional and keeps the installed values.
	dst = decodeRecord{S: "kept", L: []string{"kept"}}
	if err := Decode([]byte(`{"d":"x"}`), &dst, true, true); err != nil {
		t.Fatal(err)
	}
	if want := (decodeRecord{S: "kept", D: "x", L: []string{"kept"}, M: map[string]int{}}); !reflect.DeepEqual(dst, want) {
		t.Fatalf("defaultAll = %#v; want %#v", dst, want)
	}
	if err := Decode([]byte(`{"z":1}`), &dst, true, true); err == nil || err.Error() != `unknown field "z"` {
		t.Fatalf("defaultAll with an unknown field = %v; want it refused", err)
	}
}

type strictItem struct {
	N string `json:"n"`
}

func (v *strictItem) UnmarshalJSON(data []byte) error {
	type plain strictItem
	decoded := plain{}
	if err := Decode(data, &decoded, true, false); err != nil {
		return err
	}
	*v = strictItem(decoded)
	return nil
}

type outerRecord struct {
	Item  strictItem   `json:"item"`
	Items []strictItem `json:"items" wire:"default"`
}

// A nested type's own decoder decides its strictness, so a lenient parent
// cannot relax a strict child, and the child's typed error keeps its path.
func TestNestedDecodersKeepTheirOwnStrictness(t *testing.T) {
	var outer outerRecord
	if err := Decode([]byte(`{"item":{"n":"x"},"items":[{"n":"y"}],"ignored":true}`), &outer, false, false); err != nil {
		t.Fatal(err)
	}
	if want := (outerRecord{Item: strictItem{N: "x"}, Items: []strictItem{{N: "y"}}}); !reflect.DeepEqual(outer, want) {
		t.Fatalf("outer = %#v; want %#v", outer, want)
	}
	for _, tc := range []struct{ raw, message string }{
		{`{"item":{"n":"x","extra":1}}`, `item: unknown field "extra"`},
		{`{"item":{"n":"x"},"items":[{"n":"y"},{"n":"z","n":"w"}]}`, `items: duplicate field "n"`},
		{`{"item":{}}`, `item: missing field "n"`},
		{`{"item":null}`, "item: null is not allowed"},
	} {
		err := Decode([]byte(tc.raw), &outerRecord{}, false, false)
		var typed *Error
		if !errors.As(err, &typed) || err.Error() != tc.message {
			t.Errorf("Decode(%s) = %v; want typed %q", tc.raw, err, tc.message)
		}
	}
}

func TestRecordWritesEmptyContainersThatDecodeStrictly(t *testing.T) {
	y := "y"
	for _, tc := range []struct {
		value decodeRecord
		want  string
	}{
		{decodeRecord{S: "x"}, `{"s":"x","p":null,"d":"","l":[],"m":{},"a":null}`},
		{decodeRecord{S: "x", P: &y, D: "d", L: []string{"a"}, M: map[string]int{"k": 1}, A: []any{}}, `{"s":"x","p":"y","d":"d","l":["a"],"m":{"k":1},"a":[]}`},
	} {
		data, err := Record(tc.value)
		if err != nil || string(data) != tc.want {
			t.Fatalf("Record(%+v) = %s, %v; want %s", tc.value, data, err, tc.want)
		}
		var decoded decodeRecord
		if err := Decode(data, &decoded, true, false); err != nil {
			t.Fatalf("Decode(Record(%+v)) = %v", tc.value, err)
		}
		if data, err := Record(decoded); err != nil || string(data) != tc.want {
			t.Fatalf("Record round trip = %s, %v; want %s", data, err, tc.want)
		}
	}
	for name, err := range map[string]error{
		"Record":  second(Record(struct{ C chan int }{C: make(chan int)})),
		"Marshal": second(Marshal(make(chan int))),
	} {
		var typed *Error
		if !errors.As(err, &typed) {
			t.Errorf("%s(chan) = %v; want a typed error", name, err)
		}
	}
	if err := second(Marshal(nil)); err != nil {
		t.Fatalf("Marshal(nil) = %v", err)
	}
}

func second(_ []byte, err error) error { return err }

// Generic views feed API responses and exports, so a uint64 beyond float64's
// exact range must keep its saved spelling instead of being rounded.
func TestGenericKeepsExactNumbersAndMarksEncodeFailures(t *testing.T) {
	type record struct {
		N    uint64         `json:"n"`
		F    float64        `json:"f"`
		List []int64        `json:"list"`
		Any  any            `json:"any"`
		Map  map[string]any `json:"map"`
	}
	value := record{
		N: 18446744073709551615, F: 0.1, List: []int64{-9007199254740993},
		Any: json.Number("12345678901234567890.5"), Map: map[string]any{"k": true},
	}
	want := map[string]any{
		"n": json.Number("18446744073709551615"), "f": json.Number("0.1"),
		"list": []any{json.Number("-9007199254740993")},
		"any":  json.Number("12345678901234567890.5"), "map": map[string]any{"k": true},
	}
	object, err := GenericMap(value)
	if err != nil || !reflect.DeepEqual(object, want) {
		t.Fatalf("GenericMap = %#v, %v; want %#v", object, err, want)
	}
	generic, err := Generic(value)
	if err != nil || !reflect.DeepEqual(generic, any(want)) {
		t.Fatalf("Generic = %#v, %v; want %#v", generic, err, want)
	}
	if generic, err := Generic([]uint64{18446744073709551615}); err != nil ||
		!reflect.DeepEqual(generic, []any{json.Number("18446744073709551615")}) {
		t.Fatalf("Generic(list) = %#v, %v", generic, err)
	}
	if object, err := GenericMap([]int{1}); err == nil {
		t.Fatalf("GenericMap(list) = %#v; want an error", object)
	}
	for name, err := range map[string]error{
		"Generic":    func() error { _, err := Generic(make(chan int)); return err }(),
		"GenericMap": func() error { _, err := GenericMap(struct{ C chan int }{}); return err }(),
	} {
		var typed *Error
		if !errors.As(err, &typed) {
			t.Errorf("%s(chan) = %v; want a typed encode error", name, err)
		}
	}
}

type cloneItem struct {
	Tags []string
}

type cloneRecord struct {
	Name     string
	P        *string
	L        []string
	M        map[string][]int
	A        any
	Items    []cloneItem
	NilList  []string
	NilMap   map[string]int
	NilPtr   *string
	NilAny   any
	EmptyMap map[string]int
}

// Snapshot owners mutate what they receive, so a clone must share no map,
// slice or pointer with its source, also inside opaque evidence.
func TestCloneSharesNoMutableState(t *testing.T) {
	build := func() cloneRecord {
		p := "p"
		return cloneRecord{
			Name:     "n",
			P:        &p,
			L:        []string{"l"},
			M:        map[string][]int{"k": {1}},
			A:        map[string]any{"evidence": []any{"saved", map[string]any{"k": "v"}}},
			Items:    []cloneItem{{Tags: []string{"t"}}},
			EmptyMap: map[string]int{},
		}
	}
	original := build()
	clone := Clone(original)
	if !reflect.DeepEqual(clone, original) {
		t.Fatalf("Clone = %#v; want %#v", clone, original)
	}
	if clone.NilList != nil || clone.NilMap != nil || clone.NilPtr != nil || clone.NilAny != nil || clone.EmptyMap == nil {
		t.Fatalf("Clone changed nil or empty containers: %#v", clone)
	}
	*clone.P = "changed"
	clone.L[0] = "changed"
	clone.M["k"][0] = 9
	clone.M["new"] = nil
	evidence := clone.A.(map[string]any)["evidence"].([]any)
	evidence[0] = "changed"
	evidence[1].(map[string]any)["k"] = "changed"
	clone.Items[0].Tags[0] = "changed"
	clone.EmptyMap["new"] = 1
	if !reflect.DeepEqual(original, build()) {
		t.Fatalf("mutating the clone changed the original: %#v", original)
	}
}

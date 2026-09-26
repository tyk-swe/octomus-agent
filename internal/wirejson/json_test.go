package wirejson

import (
	"errors"
	"strings"
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
	_, err := MarshalEnum(testEnum(7), testEnumNames)
	var typed *Error
	if !errors.As(err, &typed) || !strings.Contains(err.Error(), "invalid enum value 7") {
		t.Fatalf("MarshalEnum(7) = %v; want a typed range error", err)
	}
	if EnumName(testEnum(0), testEnumNames) != "alpha" || EnumName(testEnum(2), testEnumNames) != "" {
		t.Fatal("EnumName range")
	}
}

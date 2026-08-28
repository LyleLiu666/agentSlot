package jsonvalue

import "testing"

func TestEqualUsesJSONValueSemantics(t *testing.T) {
	for _, test := range []struct {
		name        string
		left, right string
		equal       bool
	}{
		{name: "object order and whitespace", left: `{"a":1,"b":2}`, right: `{ "b": 2, "a": 1 }`, equal: true},
		{name: "equivalent escapes", left: `{"text":"\u4e2d","path":"a\/b"}`, right: `{"path":"a/b","text":"中"}`, equal: true},
		{name: "exact exponent", left: `1e1000`, right: `10e999`, equal: true},
		{name: "huge exponent stays compact", left: `1e1000000000`, right: `10e999999999`, equal: true},
		{name: "negative zero", left: `-0`, right: `0.0`, equal: true},
		{name: "large integer differs", left: `9007199254740992`, right: `9007199254740993`, equal: false},
		{name: "array order", left: `[1,2]`, right: `[2,1]`, equal: false},
		{name: "type differs", left: `"1"`, right: `1`, equal: false},
		{name: "duplicate left", left: `{"a":1,"a":1}`, right: `{"a":1}`, equal: false},
		{name: "invalid right", left: `null`, right: `not-json`, equal: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Equal([]byte(test.left), []byte(test.right)); got != test.equal {
				t.Fatalf("Equal(%s, %s) = %v, want %v", test.left, test.right, got, test.equal)
			}
		})
	}
}

func TestValidRejectsAmbiguousOrMultipleValues(t *testing.T) {
	for _, raw := range []string{`{"a":1,"a":2}`, `{} {}`, ``, `[1,]`, `{"a":}`} {
		if Valid([]byte(raw)) {
			t.Fatalf("Valid(%q) = true", raw)
		}
	}
	for _, raw := range []string{`null`, `true`, `1e1000`, `"text"`, `[]`, `{}`} {
		if !Valid([]byte(raw)) {
			t.Fatalf("Valid(%q) = false", raw)
		}
	}
}

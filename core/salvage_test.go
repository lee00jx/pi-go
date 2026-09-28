package core

import (
	"reflect"
	"testing"
)

func TestParseSalvage(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   map[string]any
		wantOK bool
	}{
		{"complete", `{"text":"hi"}`, map[string]any{"text": "hi"}, true},
		{"empty string", ``, map[string]any{}, false},
		{"blank", `   `, map[string]any{}, false},
		{"truncated mid-string value", `{"text":"hel`, map[string]any{}, false},
		{"truncated after colon", `{"text":`, map[string]any{}, false},
		{"truncated mid-key after comma", `{"a":"b","c`, map[string]any{"a": "b"}, false},
		{"truncated mid-key at start", `{"te`, map[string]any{}, false},
		{"truncated after comma", `{"a":"b",`, map[string]any{"a": "b"}, false},
		{"truncated mid-array value", `{"nums":[1,2,`, map[string]any{"nums": []any{1.0, 2.0}}, false},
		{"truncated mid-nested-object", `{"a":{"b":"c"`, map[string]any{"a": map[string]any{"b": "c"}}, false},
		{"truncated after number", `{"n":12`, map[string]any{"n": float64(12)}, false},
		{"bare array top-level", `[1,2`, nil, false}, // not an object → {}
		{"garbage", `{{{`, map[string]any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseSalvage(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (salvage=%q)", ok, tc.wantOK, got)
			}
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("want empty object, got %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepairJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no-op", `{"a":"b"}`, `{"a":"b"}`},
		// Interpreted (double-quoted) strings: the inputs contain real control
		// bytes / invalid escapes, the wants contain the two-char text escapes.
		{"raw newline in string", "{\"a\":\"l1\nl2\"}", "{\"a\":\"l1\\nl2\"}"},
		{"raw tab in string", "{\"a\":\"x\ty\"}", "{\"a\":\"x\\ty\"}"},
		{"invalid escape doubled", "{\"a\":\"\\q\"}", "{\"a\":\"\\\\q\"}"}, // backslash-q
		{"trailing backslash", `{"a":"x\`, `{"a":"x\\`},
		{"valid unicode kept", `{"a":"中"}`, `{"a":"中"}`},
		{"broken unicode doubled", `{"a":"\u12zz"}`, `{"a":"\\u12zz"}`},
		{"valid escapes kept", `{"a":"\"\\/"}`, `{"a":"\"\\/"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepairJSON(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Round-trip: repaired + closed truncated input must parse back to the
// expected arguments, mirroring how the OpenAI client finalizes
// tool-call argument fragments.
func TestParseSalvageRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]any
	}{
		{`{"cmd":"ls -la","timeout":5`, map[string]any{"cmd": "ls -la", "timeout": float64(5)}},
		{`{"cmd":"line1`, map[string]any{}}, // cut inside first value: nothing recoverable per-key
		// Ambiguous quotes (a completed string followed by a stray fragment)
		// are beyond best-effort: fall back to {} like any dead end.
		{`{"cmd":"echo "a"`, map[string]any{}},
	}
	for _, tc := range cases {
		got, _ := ParseSalvage(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("input %q: got %v, want %v", tc.in, got, tc.want)
		}
	}
}

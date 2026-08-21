package agent

import (
	"strings"
	"testing"
)

// TestIntArgOptionalCoercion pins the accepted value shapes for integer tool
// arguments: numeric JSON decodes (float64), int/int64 (in-process callers),
// and quoted numeric strings (models sometimes quote numbers). Fractions,
// non-numeric text, and empty strings error — never silently 0.
func TestIntArgOptionalCoercion(t *testing.T) {
	cases := []struct {
		name string
		val  interface{}
		want int
	}{
		{"float64 whole", float64(3), 3},
		{"int", 3, 3},
		{"int64", int64(3), 3},
		{"quoted number", "3", 3},
		{"quoted with whitespace", "  3  ", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := intArgOptional(map[string]interface{}{"id": tc.val}, "id")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}

	errorCases := []struct {
		name string
		val  interface{}
	}{
		{"float fraction", float64(3.5)},
		{"non-numeric text", "abc"},
		{"fraction string", "3.5"},
		{"empty string", ""},
		{"bool", true},
	}
	for _, tc := range errorCases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := intArgOptional(map[string]interface{}{"id": tc.val}, "id")
			if err == nil {
				t.Fatalf("expected error for %v", tc.val)
			}
			if !strings.Contains(err.Error(), `"id"`) || !strings.Contains(err.Error(), "integer") {
				t.Fatalf("expected integer type error, got: %v", err)
			}
		})
	}

	t.Run("absent returns zero without error", func(t *testing.T) {
		got, err := intArgOptional(map[string]interface{}{}, "id")
		if err != nil || got != 0 {
			t.Fatalf("got (%d, %v), want (0, nil)", got, err)
		}
	})
}

// TestIntRequiredArg pins the accepted value shapes for required id
// arguments (todo done/remove, board id actions): the same coercions as
// intArgOptional, plus zero/negative rejection (ids start at 1) and a
// missing-argument error for an absent key.
func TestIntRequiredArg(t *testing.T) {
	cases := []struct {
		name string
		val  interface{}
		want int
	}{
		{"float64 whole", float64(3), 3},
		{"int", 3, 3},
		{"int64", int64(3), 3},
		{"quoted number", "3", 3},
		{"quoted with whitespace", "  3  ", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := intRequiredArg(map[string]interface{}{"id": tc.val}, "id")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}

	errorCases := []struct {
		name string
		val  interface{}
	}{
		{"zero", float64(0)},
		{"quoted zero", "0"},
		{"negative", -1},
		{"quoted negative", "-1"},
		{"float fraction", float64(3.5)},
		{"non-numeric text", "abc"},
		{"empty string", ""},
		{"bool", true},
	}
	for _, tc := range errorCases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := intRequiredArg(map[string]interface{}{"id": tc.val}, "id")
			if err == nil {
				t.Fatalf("expected error for %v", tc.val)
			}
		})
	}

	t.Run("absent reports missing", func(t *testing.T) {
		_, err := intRequiredArg(map[string]interface{}{}, "id")
		if err == nil || !strings.Contains(err.Error(), `missing required argument "id"`) {
			t.Fatalf("got %v, want missing-argument error", err)
		}
	})
}

package agent

import (
	"testing"
)

func TestParseDiffLineCountRejectsNegative(t *testing.T) {
	for _, part := range []string{"-1", "-1,2"} {
		if _, err := parseDiffLineCount(part); err == nil {
			t.Fatalf("expected error for %q", part)
		}
	}
}

func TestParseDiffLineCountAllowsZeroForNewFiles(t *testing.T) {
	got, err := parseDiffLineCount("0,0")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("got %d want 0", got)
	}
}

func TestParseDiffLineCountAcceptsPositive(t *testing.T) {
	got, err := parseDiffLineCount("5,3")
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("got %d want 5", got)
	}
}

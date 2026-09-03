package buildinfo

import (
	"regexp"
	"testing"
)

var userAgentRE = regexp.MustCompile(`^gogen/([0-9a-f]{7}|dev)$`)

func TestUserAgentFormat(t *testing.T) {
	t.Parallel()
	ua := UserAgent()
	if !userAgentRE.MatchString(ua) {
		t.Fatalf("UserAgent() = %q, want gogen/<7-hex-revision> or gogen/dev", ua)
	}
}

func TestUserAgentStable(t *testing.T) {
	t.Parallel()
	first := UserAgent()
	for i := 0; i < 10; i++ {
		if got := UserAgent(); got != first {
			t.Fatalf("UserAgent() changed between calls: %q -> %q", first, got)
		}
	}
}

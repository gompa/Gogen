package llm

import (
	"net/http"
	"testing"
)

func TestNewSSEHTTPClientDisablesCompression(t *testing.T) {
	t.Parallel()
	c := newSSEHTTPClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", c.Transport)
	}
	if !tr.DisableCompression {
		t.Fatal("DisableCompression = false, want true")
	}
}

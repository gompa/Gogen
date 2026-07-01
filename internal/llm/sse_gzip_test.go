package llm

import (
	"net/http"
	"testing"
)

func TestSSEHTTPClientDisablesCompression(t *testing.T) {
	t.Parallel()
	tr, ok := newSSEHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if !tr.DisableCompression {
		t.Fatal("expected DisableCompression on SSE client")
	}
}

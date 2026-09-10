package llm

import (
	"testing"
)

func TestSSEHTTPClientDisablesCompression(t *testing.T) {
	t.Parallel()
	tr, ok := baseHTTPTransport(newSSEHTTPClient())
	if !ok {
		t.Fatal("expected *http.Transport under the SSE filter transport")
	}
	if !tr.DisableCompression {
		t.Fatal("expected DisableCompression on SSE client")
	}
}

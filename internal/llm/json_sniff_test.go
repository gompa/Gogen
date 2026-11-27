package llm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

// modelListJSON is a minimal OpenAI-shaped catalog payload.
const modelListJSON = `{"object":"list","data":[{"id":"model-a","object":"model"}]}`

// TestListModelsToleratesNonJSONContentType pins the reported failure: a
// server that answers GET /models with a valid JSON body under the wrong
// media type used to fail the whole catalog fetch with openai-go's opaque
// "expected destination type of 'string' or '[]byte' for responses with
// content-type ... that is not 'application/json'". A Go backend that writes
// the body without setting Content-Type gets exactly that
// "text/plain; charset=utf-8" header from net/http's content sniffing, so
// /models in the TUI showed the error instead of the model picker.
func TestListModelsToleratesNonJSONContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
	}{
		{name: "text/plain from Go content sniffing", contentType: "text/plain; charset=utf-8"},
		{name: "text/html", contentType: "text/html"},
		{name: "empty", contentType: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" {
					http.NotFound(w, r)
					return
				}
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				_, _ = w.Write([]byte(modelListJSON))
			}))
			defer srv.Close()

			models, err := listModelsFromServer(t, srv.URL)
			if err != nil {
				t.Fatalf("listModels: %v", err)
			}
			if len(models) != 1 || models[0].model.ID != "model-a" {
				t.Fatalf("unexpected models: %+v", models)
			}
		})
	}
}

// TestListModelsNonJSONBodyStillErrors pins that the normalization is
// conditional: a 200 whose body is genuinely not JSON (an HTML portal page,
// a proxy notice) is passed through and still fails the fetch rather than
// being silently coerced into an empty catalog.
func TestListModelsNonJSONBodyStillErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("upstream gateway error"))
	}))
	defer srv.Close()

	models, err := listModelsFromServer(t, srv.URL)
	if err == nil {
		t.Fatalf("expected an error for a non-JSON body, got models %+v", models)
	}
}

// TestListModelsKeepsJSONContentTypeResponseWorking guards the sniffer
// against breaking the ordinary path.
func TestListModelsKeepsJSONContentTypeResponseWorking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "model-a", "object": "model"}},
		})
	}))
	defer srv.Close()

	models, err := listModelsFromServer(t, srv.URL)
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(models) != 1 || models[0].model.ID != "model-a" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

// TestListModelsToleratesUnrequestedGzip pins the second quirk of the same
// class of endpoint (ai.h-bomb.nl): the catalog body is gzipped even though
// the client never advertised Accept-Encoding, and served under a non-JSON
// media type. The catalog client must advertise gzip (so the standard library
// decodes it) and normalize the media type, otherwise the fetch fails with
// openai-go's content-type error.
func TestListModelsToleratesUnrequestedGzip(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": "model-a", "object": "model"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var sawAcceptEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(payload)
		_ = gz.Close()
	}))
	defer srv.Close()

	models, err := listModelsFromServer(t, srv.URL)
	if err != nil {
		t.Fatalf("listModels: %v (Accept-Encoding sent: %q)", err, sawAcceptEncoding)
	}
	if len(models) != 1 || models[0].model.ID != "model-a" {
		t.Fatalf("unexpected models: %+v", models)
	}
	if !strings.Contains(sawAcceptEncoding, "gzip") {
		t.Fatalf("Accept-Encoding = %q, want gzip advertised so the transport decodes it", sawAcceptEncoding)
	}
}

// listModelsFromServer builds a provider the way production does (through
// newClientPair, so the catalog client carries the real transport stack) and
// lists its models.
func listModelsFromServer(t *testing.T, baseURL string) ([]catalogModel, error) {
	t.Helper()
	stream, catalog := newClientPair(baseURL+"/", "test", nil)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", stream: stream, catalog: catalog}},
		modelClient: make(map[ModelRef]*openai.Client),
	}
	return p.listModels(context.Background())
}

// stubRoundTripper returns a fixed response, letting the sniff transport be
// exercised directly (no network, and no 4 MiB round trip for the cap cases).
type stubRoundTripper struct{ resp *http.Response }

func (s stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return s.resp, nil }

// TestJSONSniffBoundsBufferedBody pins the read bound on the catalog
// transport: the base URL is user/provider configuration, so an endpoint that
// answers GET /models with a non-JSON media type and an endless body could
// otherwise buffer it to the heap limit and OOM the process. A body under the
// cap is still normalized; one over it is passed through untouched (header,
// ContentLength, and the full body remain readable), so the decode error the
// request would have produced without this transport is preserved.
func TestJSONSniffBoundsBufferedBody(t *testing.T) {
	// jsonPayload pads valid JSON out to size with trailing whitespace, which
	// json.Valid accepts.
	jsonPayload := func(size int) []byte {
		const base = `{"object":"list","data":[]}`
		out := make([]byte, size)
		copy(out, base)
		for i := len(base); i < size; i++ {
			out[i] = ' '
		}
		return out
	}
	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantType    string
		wantBody    []byte
	}{
		{
			name:        "small valid JSON is normalized",
			contentType: "text/plain; charset=utf-8",
			body:        jsonPayload(64),
			wantType:    "application/json",
		},
		{
			name:        "small non-JSON keeps its header",
			contentType: "text/html",
			body:        []byte("<html>gateway error</html>"),
			wantType:    "text/html",
		},
		{
			name:        "oversized body is passed through untouched",
			contentType: "text/plain; charset=utf-8",
			body:        jsonPayload(jsonSniffMaxBody + 1024),
			wantType:    "text/plain; charset=utf-8",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := make([]byte, len(tt.body))
			copy(orig, tt.body)
			resp := &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{tt.contentType}},
				Body:          io.NopCloser(bytes.NewReader(orig)),
				ContentLength: int64(len(orig)),
			}
			tr := &jsonSniffTransport{base: stubRoundTripper{resp: resp}}
			req, err := http.NewRequest(http.MethodGet, "https://catalog.example/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			if ct := got.Header.Get("Content-Type"); ct != tt.wantType {
				t.Fatalf("Content-Type = %q, want %q", ct, tt.wantType)
			}
			if got.ContentLength != int64(len(orig)) {
				t.Fatalf("ContentLength = %d, want %d", got.ContentLength, len(orig))
			}
			body, err := io.ReadAll(got.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !bytes.Equal(body, orig) {
				t.Fatalf("body not preserved: got %d bytes, want %d", len(body), len(orig))
			}
			if err := got.Body.Close(); err != nil {
				t.Fatalf("close body: %v", err)
			}
		})
	}
}

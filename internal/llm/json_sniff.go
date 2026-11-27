package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// jsonSniffTransport normalizes the Content-Type of a response that carries a
// JSON body under a non-JSON media type.
//
// OpenAI-compatible catalogs routinely mislabel GET /models: a Go backend
// that writes its JSON body without setting Content-Type gets net/http's
// content sniffing, which reports "text/plain; charset=utf-8" for JSON
// (DetectContentType does not recognize it), and proxies often drop or rewrite
// the header. openai-go's requestconfig refuses to decode a struct from a
// non-JSON content type and fails the whole catalog fetch with the opaque
// "expected destination type of 'string' or '[]byte' for responses with
// content-type ... that is not 'application/json'". Rewriting the header to
// application/json when — and only when — the body is valid JSON keeps those
// endpoints usable; a body that really is not JSON passes through untouched,
// so genuinely broken/HTML responses still fail (with the original error).
//
// Only the catalog client wraps this transport (see newCatalogHTTPClient): the
// stream client's bodies are SSE, which must never be buffered here. That
// client leaves compression enabled, so a gzipped body has already been
// decoded by the standard library below this wrapper — the bytes inspected
// here are the real payload.
type jsonSniffTransport struct {
	base http.RoundTripper
}

// jsonSniffMaxBody bounds how much of a non-JSON-typed response this transport
// will buffer to inspect. The catalog base URL is user/provider configuration,
// so a broken or hostile endpoint answering GET /models with a non-JSON media
// type and an endless body must not be able to grow this process's heap: a
// real model catalog is a few hundred KiB, so 4 MiB is generous.
const jsonSniffMaxBody = 4 << 20

func (t *jsonSniffTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Header == nil {
		return resp, err
	}
	// Below 400 openai-go decodes the body according to the Content-Type;
	// error responses are parsed from their raw body regardless (via gjson),
	// so there is nothing to normalize for them.
	if resp.StatusCode >= 400 || isJSONContentType(resp.Header.Get("Content-Type")) {
		return resp, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jsonSniffMaxBody+1))
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if len(body) > jsonSniffMaxBody {
		// Too large to judge. Fail OPEN and non-destructively, the same way
		// apiErrorBodyTransport handles an oversize error body: splice the
		// bytes already read back in front of the unread remainder and leave
		// the Content-Type and ContentLength untouched, so the request behaves
		// exactly as it would without this transport (the existing
		// content-type decode error path stays reachable) instead of turning
		// into a new kind of failure or a truncated body.
		resp.Body = splicedBody{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		return resp, nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if json.Valid(bytes.TrimSpace(body)) {
		resp.Header.Set("Content-Type", "application/json")
	}
	return resp, nil
}

// isJSONContentType mirrors openai-go's media-type test (see
// requestconfig.Execute): a media type containing "application/json" or ending
// in "+json" is decoded as JSON and needs no normalization.
func isJSONContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.Contains(mediaType, "application/json") || strings.HasSuffix(mediaType, "+json")
}

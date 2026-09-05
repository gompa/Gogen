package llm

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// SessionHeaderName is the HTTP header that tags requests to OpenCode
// endpoints with the session they belong to.
const SessionHeaderName = "X-Opencode-Session"

// sessionHeaderNamespace is the fixed UUIDv5 namespace the
// X-Opencode-Session value is derived from (RFC 4122 §4.3). It is a
// gogen-specific constant: the same session ID derives the same UUID on
// every machine and every process start, so a resumed session presents a
// stable header across restarts without persisting anything. Changing this
// constant changes every derived header (a deliberate, one-time migration).
var sessionHeaderNamespace = [16]byte{
	0xfb, 0x66, 0xed, 0x97, 0x26, 0x51, 0x15, 0x71,
	0x97, 0xb2, 0x6b, 0x78, 0xac, 0x36, 0xde, 0xd8,
}

// uuidV5 returns the canonical dashed UUIDv5 (RFC 4122 §4.3) derived from
// name under the given 16-byte namespace. sha1 is the RFC-mandated
// digest for version 5 — a derivation, not a security context.
func uuidV5(namespace [16]byte, name string) string {
	sum := sha1.Sum(append(append(make([]byte, 0, 16+len(name)), namespace[:]...), []byte(name)...))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5 (SHA-1)
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SessionUUID derives the X-Opencode-Session value for sessionID: a UUIDv5
// under sessionHeaderNamespace, so the value is deterministic — a resumed
// session sends the same header after a restart. An empty (or
// whitespace-only) session ID returns "" — callers omit the header then.
func SessionUUID(sessionID string) string {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return ""
	}
	return uuidV5(sessionHeaderNamespace, id)
}

// sessionHeaderPair is the (session ID, derived UUID) pair published
// atomically by sessionHeaderState.
type sessionHeaderPair struct {
	id   string
	uuid string
}

// emptySessionHeader is the published pair when no session is set.
var emptySessionHeader = &sessionHeaderPair{}

// sessionHeaderState holds the provider's current session ID and the UUIDv5
// derived from it. The pair is computed once per Set (not per request) and
// published with a single atomic store, so request-path readers never block
// and no lock is taken — there is no lock-ordering interaction with the
// provider's other mutexes.
type sessionHeaderState struct {
	current atomic.Pointer[sessionHeaderPair]
}

func newSessionHeaderState() *sessionHeaderState {
	st := &sessionHeaderState{}
	st.current.Store(emptySessionHeader)
	return st
}

// SetSessionID publishes id (and its derived UUID) as the current session.
// An empty id clears the header. Safe for concurrent use.
func (st *sessionHeaderState) SetSessionID(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		st.current.Store(emptySessionHeader)
		return
	}
	st.current.Store(&sessionHeaderPair{id: id, uuid: SessionUUID(id)})
}

// uuid returns the current session's derived UUID, or "" when no session is
// set.
func (st *sessionHeaderState) uuid() string {
	return st.current.Load().uuid
}

// sessionHeaderRoundTripper injects the X-Opencode-Session header into every
// request when a session is set on st. It wraps the endpoint client's base
// transport (see newClientPair); openai-go builds a fresh http.Request per
// call, so mutating req.Header here is safe.
type sessionHeaderRoundTripper struct {
	base http.RoundTripper
	st   *sessionHeaderState
}

func (rt *sessionHeaderRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if uuid := rt.st.uuid(); uuid != "" {
		req.Header.Set(SessionHeaderName, uuid)
	}
	return rt.base.RoundTrip(req)
}

// SessionIDSetter is implemented by providers that tag their requests with
// a per-session identifier (OpenAIProvider sends X-Opencode-Session to
// OpenCode endpoints). It is an optional capability: agents sync through a
// type assertion, so mock providers need not implement it.
type SessionIDSetter interface {
	SetSessionID(id string)
}

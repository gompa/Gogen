package server

import (
	"bytes"
	"crypto/tls"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// bootstrapServer returns a token-authenticated Server with an installed
// pairing code and no rate limiter (tests exercise the limiter separately).
func bootstrapServer(token, code string) *Server {
	s := &Server{authToken: token}
	if code != "" {
		s.SetPairingCode(code, time.Now().Add(PairingTTL))
	}
	return s
}

// getCookie extracts the auth cookie from a recorded response.
func getCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookieName {
			return c
		}
	}
	t.Fatal("response did not set the auth cookie")
	return nil
}

// pairingNavRequest builds a GET that looks like a real browser
// navigation to a pairing endpoint over a trustworthy origin
// (Sec-Fetch-Mode: navigate), which is required for the exchange to
// consume the code.
func pairingNavRequest(target string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	return req
}

// phoneNavRequest builds the GET exactly as the phone's browser sent it in
// the failing scan trace: a real top-level navigation over plain HTTP,
// where browsers send no Sec-Fetch-* at all (fetch metadata is withheld
// from non-trustworthy origins) and instead carry the classic navigation
// fingerprint — Accept preferring text/html and
// Upgrade-Insecure-Requests: 1.
func phoneNavRequest(target string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Mobile Safari/537.36")
	return req
}

// TestSetAuthCookieContract verifies the exact Set-Cookie attributes the
// pairing exchange relies on: host-only (no Domain), Path=/, HttpOnly,
// SameSite=Strict, 30-day Max-Age, and Secure only when the request was
// TLS/proxy-https. The phone's Brave sent zero cookies back, so this pins
// down what the server actually emitted on the wire.
func TestSetAuthCookieContract(t *testing.T) {
	s := bootstrapServer("secret", "code")

	// Plain-HTTP scan (the QR form): cookie must NOT carry Secure.
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/pair/code"))
	c := getCookie(t, rec)
	if c.Value != "secret" {
		t.Fatalf("value=%q, want %q", c.Value, "secret")
	}
	if c.Path != "/" {
		t.Fatalf("Path=%q, want /", c.Path)
	}
	if !c.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("SameSite=%v, want Strict", c.SameSite)
	}
	if c.MaxAge != authCookieMaxAgeSecs {
		t.Fatalf("MaxAge=%d, want %d", c.MaxAge, authCookieMaxAgeSecs)
	}
	if c.Domain != "" {
		t.Fatalf("Domain=%q, want host-only (no Domain attribute)", c.Domain)
	}
	if c.Secure {
		t.Fatal("plain-HTTP exchange must not set Secure")
	}

	// TLS request: the same exchange must set Secure.
	req := pairingNavRequest("/pair/code")
	req.TLS = &tls.ConnectionState{}
	recTLS := httptest.NewRecorder()
	s.HandleStatic(recTLS, req)
	ct := getCookie(t, recTLS)
	if !ct.Secure {
		t.Fatal("TLS exchange must set Secure")
	}
}

func TestHandleStaticTokenBootstrap(t *testing.T) {
	s := bootstrapServer("secret", "")

	rec := httptest.NewRecorder()
	s.HandleStatic(rec, httptest.NewRequest("GET", "/?token=secret", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if c := getCookie(t, rec); c.Value != "secret" {
		t.Fatalf("cookie value=%q, want %q", c.Value, "secret")
	}
	// The redirect strips the query; the cookie authenticates the next hit.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(getCookie(t, rec))
	rec2 := httptest.NewRecorder()
	s.HandleStatic(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("follow-up status=%d, want 200", rec2.Code)
	}
}

func TestHandleStaticTokenBootstrapWrong(t *testing.T) {
	s := bootstrapServer("secret", "")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, httptest.NewRequest("GET", "/?token=wrong", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
}

func TestHandleStaticPairingBootstrap(t *testing.T) {
	s := bootstrapServer("secret", "paircode")

	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/?pair=paircode"))
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if c := getCookie(t, rec); c.Value != "secret" {
		t.Fatalf("cookie value=%q, want the auth token %q", c.Value, "secret")
	}
	// The code is multi-use within its TTL (bounded by maxPairingUses) so
	// the "click the link, then scan the QR" flow works: a second exchange
	// with the same code still succeeds.
	rec2 := httptest.NewRecorder()
	s.HandleStatic(rec2, pairingNavRequest("/?pair=paircode"))
	if rec2.Code != http.StatusFound {
		t.Fatalf("second exchange status=%d, want 302", rec2.Code)
	}
}

// TestPairingCodeNotConsumedByNonNavigation verifies that programmatic
// requests — camera-app link previews (Sec-Fetch-Mode: no-cors) and
// non-GET probes — get the minimal preview response and do NOT consume a
// pairing use, set a cookie, or redirect, so the code survives for the
// real navigation that opens the link in the browser.
func TestPairingCodeNotConsumedByNonNavigation(t *testing.T) {
	s := bootstrapServer("secret", "code")

	// Camera-app preview fetch: no-cors GET. Must not consume, no cookie,
	// no redirect — just the plain "open in browser" text.
	preview := httptest.NewRequest("GET", "/pair/code", nil)
	preview.Header.Set("Sec-Fetch-Mode", "no-cors")
	preview.Header.Set("Sec-Fetch-Dest", "empty")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, preview)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview fetch status=%d, want 200", rec.Code)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("preview fetch must not set a cookie")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("preview fetch must not redirect, got Location %q", loc)
	}
	if !strings.Contains(rec.Body.String(), "opened as a page in a web browser") {
		t.Fatalf("preview response should say to open in a browser, got: %s", rec.Body.String())
	}
	// The preview response is the full sign-in page, never a dead end: a
	// misclassified real browser can still type the code.
	assertPairingForm(t, rec)

	// HEAD probe: also refused without consuming.
	recHead := httptest.NewRecorder()
	s.HandleStatic(recHead, httptest.NewRequest("HEAD", "/pair/code", nil))
	if recHead.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d, want 200", recHead.Code)
	}
	if recHead.Header().Get("Set-Cookie") != "" {
		t.Fatal("HEAD must not set a cookie")
	}

	// Same for the query form of the exchange.
	previewQ := httptest.NewRequest("GET", "/?pair=code", nil)
	previewQ.Header.Set("Sec-Fetch-Mode", "no-cors")
	recQ := httptest.NewRecorder()
	s.HandleStatic(recQ, previewQ)
	if recQ.Code != http.StatusOK {
		t.Fatalf("query preview fetch status=%d, want 200", recQ.Code)
	}
	if recQ.Header().Get("Set-Cookie") != "" {
		t.Fatal("query preview fetch must not set a cookie")
	}

	// The code must still be consumable by a real navigation afterwards.
	nav := httptest.NewRequest("GET", "/pair/code", nil)
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	recNav := httptest.NewRecorder()
	s.HandleStatic(recNav, nav)
	if recNav.Code != http.StatusFound {
		t.Fatalf("navigation after skips: status=%d, want 302", recNav.Code)
	}
	if c := getCookie(t, recNav); c.Value != "secret" {
		t.Fatalf("cookie value=%q, want %q", c.Value, "secret")
	}
}

// TestPairingCodeSkippedWithoutSecFetchMetadata verifies that a request
// with no navigation fingerprint whatsoever — the camera-app preview that
// broke QR login sends exactly this (its own raw HTTP client: no
// Sec-Fetch-*, no Accept preference) — is treated as a non-navigation and
// does NOT consume the code. (A real browser over plain HTTP also sends no
// Sec-Fetch-*, but it DOES send an HTML-first Accept — see
// TestPairingPlainHTTPScanConsumesCode and TestPairingPlainHTTPScanWithoutUIR.)
func TestPairingCodeSkippedWithoutSecFetchMetadata(t *testing.T) {
	s := bootstrapServer("secret", "code")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, httptest.NewRequest("GET", "/pair/code", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (preview response)", rec.Code)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("must not set a cookie")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("must not redirect, got Location %q", loc)
	}
	if !strings.Contains(rec.Body.String(), "opened as a page in a web browser") {
		t.Fatalf("body should say to open in a browser, got: %s", rec.Body.String())
	}
	assertPairingForm(t, rec)
	// The code must still be consumable by a real navigation.
	recNav := httptest.NewRecorder()
	s.HandleStatic(recNav, pairingNavRequest("/pair/code"))
	if recNav.Code != http.StatusFound {
		t.Fatalf("navigation after skip: status=%d, want 302", recNav.Code)
	}
	if c := getCookie(t, recNav); c.Value != "secret" {
		t.Fatalf("cookie value=%q, want %q", c.Value, "secret")
	}
}

// TestPairingPlainHTTPScanConsumesCode reproduces the reported failure
// byte-for-byte: the QR encodes http://<lan-ip>:<port>/pair/<code>, and
// browsers send NO Sec-Fetch-* headers to plain-HTTP origins, so the
// phone's real Android Chrome navigation arrives header-less. It must
// still consume the code, set the cookie, and log the phone in.
func TestPairingPlainHTTPScanConsumesCode(t *testing.T) {
	s := bootstrapServer("secret", "code")

	rec := httptest.NewRecorder()
	s.HandleStatic(rec, phoneNavRequest("/pair/code"))
	if rec.Code != http.StatusFound {
		t.Fatalf("plain-HTTP scan status=%d, want 302", rec.Code)
	}
	if c := getCookie(t, rec); c.Value != "secret" {
		t.Fatalf("cookie value=%q, want %q", c.Value, "secret")
	}
	// The cookie authenticates the app the redirect lands on.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(getCookie(t, rec))
	rec2 := httptest.NewRecorder()
	s.HandleStatic(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("follow-up status=%d, want 200", rec2.Code)
	}

	// The manual-entry form (a GET form on the sign-in page) is the same
	// kind of header-less navigation over plain HTTP and must work too.
	s2 := bootstrapServer("secret", "code")
	recForm := httptest.NewRecorder()
	s2.HandleStatic(recForm, phoneNavRequest("/?pair=code"))
	if recForm.Code != http.StatusFound {
		t.Fatalf("plain-HTTP form exchange status=%d, want 302", recForm.Code)
	}
	if c := getCookie(t, recForm); c.Value != "secret" {
		t.Fatalf("form cookie value=%q, want %q", c.Value, "secret")
	}
}

// TestPairingPlainHTTPScanWithoutUIR pins the availability guarantee of
// the plain-HTTP fallback: Upgrade-Insecure-Requests is a client
// preference that real browsers may omit (browser configuration, forks,
// proxies stripping it), so a navigation-shaped request without it must
// still consume the code and log in. Requiring it is what locked real
// browsers out of QR login.
func TestPairingPlainHTTPScanWithoutUIR(t *testing.T) {
	s := bootstrapServer("secret", "code")
	req := httptest.NewRequest("GET", "/pair/code", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	// Deliberately no Upgrade-Insecure-Requests, no Sec-Fetch-*.
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("scan without UIR status=%d, want 302", rec.Code)
	}
	if c := getCookie(t, rec); c.Value != "secret" {
		t.Fatalf("cookie value=%q, want %q", c.Value, "secret")
	}
}

// TestPairingPlainHTTPPreviewFetchesSkipped verifies that header-less
// (no Sec-Fetch-*) requests are only treated as navigations when they
// carry the browser navigation fingerprint: camera-app preview fetchers
// (Accept: */* or no Accept at all) and WebView containers
// (X-Requested-With) must not consume the code.
func TestPairingPlainHTTPPreviewFetchesSkipped(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{
			name: "camera-app raw client, accept anything",
			headers: map[string]string{
				"Accept": "*/*",
			},
		},
		{
			name:    "raw client, no headers at all",
			headers: map[string]string{},
		},
		{
			name: "webview container with x-requested-with",
			headers: map[string]string{
				"Accept":                    "text/html,application/xhtml+xml",
				"Upgrade-Insecure-Requests": "1",
				"X-Requested-With":          "com.example.camera",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := bootstrapServer("secret", "code")
			req := httptest.NewRequest("GET", "/pair/code", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.HandleStatic(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200 (preview response)", rec.Code)
			}
			if rec.Header().Get("Set-Cookie") != "" {
				t.Fatal("preview fetch must not set a cookie")
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("preview fetch must not redirect, got Location %q", loc)
			}
			if !strings.Contains(rec.Body.String(), "opened as a page in a web browser") {
				t.Fatalf("body should say to open in a browser, got: %s", rec.Body.String())
			}
			// The skipped request gets the sign-in page (manual fallback),
			// never a dead end.
			assertPairingForm(t, rec)
			// The code must survive for the real navigation.
			recNav := httptest.NewRecorder()
			s.HandleStatic(recNav, phoneNavRequest("/pair/code"))
			if recNav.Code != http.StatusFound {
				t.Fatalf("navigation after skip: status=%d, want 302", recNav.Code)
			}
		})
	}
}

// TestPairingCookieNotKeptDiagnosis verifies the decisive diagnosis for
// the browser-side cookie failure: an accepted exchange from the same IP
// followed (within the window) by a credential-less page request swaps
// the generic "enter the code" prompt for an explicit explanation — the
// exchange set the cookie but the browser dropped or withheld it, so
// typing the code cannot work until cookies are allowed for this address.
func TestPairingCookieNotKeptDiagnosis(t *testing.T) {
	s := bootstrapServer("secret", "code")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	// A successful scan, then the redirect follow with no cookie
	// stored/sent — the exact shape of the reported failure.
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, phoneNavRequest("/pair/code"))
	if rec.Code != http.StatusFound {
		t.Fatalf("scan status=%d, want 302", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	s.HandleStatic(rec2, httptest.NewRequest("GET", "/", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("follow-up status=%d, want 401", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "did not keep the sign-in cookie") {
		t.Fatalf("body should explain the dropped cookie, got: %s", rec2.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, "pairing was accepted from 192.0.2.1") {
		t.Fatalf("log should contain the cookie-loss diagnosis, got: %s", out)
	}

	// Outside the window the generic prompt returns: the diagnosis must
	// not stick to the IP forever.
	s.pairMu.Lock()
	s.lastPairAt = time.Now().Add(-time.Hour)
	s.pairMu.Unlock()
	rec3 := httptest.NewRecorder()
	s.HandleStatic(rec3, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec3.Body.String(), "Enter the pairing code") {
		t.Fatalf("body should fall back to the generic prompt, got: %s", rec3.Body.String())
	}
}

func TestHandleStaticPairingExpired(t *testing.T) {
	s := &Server{authToken: "secret"}
	s.SetPairingCode("oldcode", time.Now().Add(-time.Minute))
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/?pair=oldcode"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("body should explain the code expired, got: %s", rec.Body.String())
	}
	assertPairingForm(t, rec)
}

func TestHandleStaticPairingExhausted(t *testing.T) {
	s := bootstrapServer("secret", "code")
	for i := 0; i < maxPairingUses; i++ {
		if s.consumePairingCode("code") != pairingAccepted {
			t.Fatalf("pre-consumption use %d should succeed", i+1)
		}
	}
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/?pair=code"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already been used") {
		t.Fatalf("body should explain the code is exhausted, got: %s", rec.Body.String())
	}
	assertPairingForm(t, rec)
}

func TestHandleStaticPairingWrong(t *testing.T) {
	s := bootstrapServer("secret", "code")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/?pair=nope"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid") {
		t.Fatalf("body should say the code is invalid, got: %s", rec.Body.String())
	}
	assertPairingForm(t, rec)
}

func TestHandleStaticPairingPathBootstrap(t *testing.T) {
	// The QR encodes /pair/<code> — the path form survives phone camera
	// apps and in-app browsers that strip query strings.
	s := bootstrapServer("secret", "pathcode")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/pair/pathcode"))
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location=%q, want /", loc)
	}
	if c := getCookie(t, rec); c.Value != "secret" {
		t.Fatalf("cookie value=%q, want the auth token", c.Value)
	}
	// The cookie now authenticates the app.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(getCookie(t, rec))
	rec2 := httptest.NewRecorder()
	s.HandleStatic(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("follow-up status=%d, want 200", rec2.Code)
	}
}

func TestHandleStaticPairingPathTrailingSlash(t *testing.T) {
	// Some browsers and camera apps normalize scanned URLs to a directory
	// form; the path parser tolerates the trailing slash.
	s := bootstrapServer("secret", "pathcode")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/pair/pathcode/"))
	if rec.Code != http.StatusFound {
		t.Fatalf("trailing-slash scan status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location=%q, want /", loc)
	}
}

func TestHandleStaticPairingPathWrong(t *testing.T) {
	s := bootstrapServer("secret", "pathcode")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/pair/nope"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid") {
		t.Fatalf("body should say the code is invalid, got: %s", rec.Body.String())
	}
	assertPairingForm(t, rec)
}

func TestHandleStaticPairingPathExpired(t *testing.T) {
	s := &Server{authToken: "secret"}
	s.SetPairingCode("oldcode", time.Now().Add(-time.Minute))
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/pair/oldcode"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("body should explain the code expired, got: %s", rec.Body.String())
	}
	assertPairingForm(t, rec)
}

func TestHandleStaticPairingPathMalformed(t *testing.T) {
	// Path shapes that are not /pair/<code> are not codes: the request
	// falls through to the unauthenticated flow (enter-code page).
	s := bootstrapServer("secret", "pathcode")
	for _, p := range []string{"/pair/", "/pair//", "/pair/abc/def", "/pair/..", "/pair/" + strings.Repeat("a", 200)} {
		rec := httptest.NewRecorder()
		s.HandleStatic(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%q: status=%d, want 401", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Enter the pairing code") {
			t.Fatalf("%q: body should prompt for the code, got: %s", p, rec.Body.String())
		}
	}
}

// TestHandleStaticPairingPageFallback verifies that a request with no
// credentials at all (e.g. a phone scanner that stripped the ?pair= query)
// gets the pairing page with a form instead of a bare 401.
func TestHandleStaticPairingPageFallback(t *testing.T) {
	s := bootstrapServer("secret", "code")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Enter the pairing code") {
		t.Fatalf("body should prompt for the pairing code, got: %s", rec.Body.String())
	}
	assertPairingForm(t, rec)
}

// TestHandleStaticPairingPageLogDiagnostic verifies the credential-less
// prompt path logs a diagnostic line — the only pairing prompt that is
// otherwise silent — so a "pairing: accepted" line followed by a prompt can
// be diagnosed server-side: cookie absent (browser never stored/sent it)
// vs cookie present but rejected (token rotated / different instance).
func TestHandleStaticPairingPageLogDiagnostic(t *testing.T) {
	s := bootstrapServer("secret", "code")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	// No cookie at all → the diagnostic must report the cookie as absent.
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	// A stale cookie (right name, wrong token) → the diagnostic must
	// report the cookie as present, distinguishing "never stored" from
	// "token rotated / different instance".
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "stale-token"})
	rec2 := httptest.NewRecorder()
	s.HandleStatic(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("stale-cookie status=%d, want 401", rec2.Code)
	}

	out := buf.String()
	if !strings.Contains(out, "auth: unauthenticated page request") {
		t.Fatalf("expected diagnostic log line, got: %s", out)
	}
	if !strings.Contains(out, "auth cookie present: false") {
		t.Fatalf("diagnostic should record the absent cookie, got: %s", out)
	}
	if !strings.Contains(out, "auth cookie present: true") {
		t.Fatalf("diagnostic should record the stale cookie as present, got: %s", out)
	}
	if !strings.Contains(out, "cookies sent: 0") {
		t.Fatalf("diagnostic should record the cookie count, got: %s", out)
	}
	if !strings.Contains(out, "sec-fetch: ") {
		t.Fatalf("diagnostic should record the Sec-Fetch classification, got: %s", out)
	}
}

// TestPairingPageDoesNotEchoCode verifies the failure page never reflects
// the submitted code back (no stored-XSS/reflection surface).
func TestPairingPageDoesNotEchoCode(t *testing.T) {
	s := bootstrapServer("secret", "code")
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, pairingNavRequest("/?pair=supersecret-wrong-code"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "supersecret-wrong-code") {
		t.Fatal("failure page must not echo the submitted code")
	}
}

// assertPairingForm checks that the response body contains the pairing
// form (input + submit), i.e. the user can type the code as a fallback,
// and that no unreplaced template placeholder leaked to the client.
func assertPairingForm(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	if !strings.Contains(body, `name="pair"`) {
		t.Fatalf("body should contain the pairing-code input, got: %s", body)
	}
	if !strings.Contains(body, "Sign in") {
		t.Fatalf("body should contain the submit button, got: %s", body)
	}
	if strings.Contains(body, "{{MESSAGE}}") {
		t.Fatal("body must not contain the unreplaced {{MESSAGE}} placeholder")
	}
}

func TestHandleStaticNoBootstrapWithoutToken(t *testing.T) {
	// No auth token configured (loopback bind): the bootstrap paths are
	// skipped entirely and the index page is served.
	s := &Server{}
	rec := httptest.NewRecorder()
	s.HandleStatic(rec, httptest.NewRequest("GET", "/?pair=whatever&token=whatever", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
}

func TestHandleStaticBootstrapRateLimit(t *testing.T) {
	// Burst 1: the first secret check passes the limiter, the second is
	// throttled regardless of the secret's validity.
	s := &Server{authToken: "secret", bootstrapLimiter: newIPLimiter(1, 1)}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.HandleStatic(rec, httptest.NewRequest("GET", "/?token=wrong", nil))
		want := http.StatusUnauthorized
		if i == 1 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("request %d status=%d, want %d", i+1, rec.Code, want)
		}
	}
}

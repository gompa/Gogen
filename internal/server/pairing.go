package server

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// PairingTTL is how long a printed onboarding pairing code stays valid.
// Long enough to pick up a phone and scan (or type the code) after fixing
// network issues; short enough that a photographed code goes dead quickly.
// A fresh code is installed on every server start, so a code photographed
// from an old terminal window goes dead as soon as the server restarts.
const PairingTTL = 15 * time.Minute

// maxPairingUses bounds how many sessions one pairing code can mint. The
// TTL is the primary control (a leaked code dies in PairingTTL); the cap
// stops one photographed code from onboarding an unbounded number of
// devices inside that window while still covering the normal "click the
// link, then scan the QR" flow.
const maxPairingUses = 5

// pairingFailReason classifies why a pairing exchange was rejected, so the
// failure page can tell the user what to do instead of a bare 401.
type pairingFailReason int

const (
	pairingAccepted pairingFailReason = iota
	// pairingWrongCode: the candidate did not match the installed code.
	pairingWrongCode
	// pairingInvalid: no usable code is installed — it expired, or the
	// server was (re)started since the printed code was minted.
	pairingInvalid
	// pairingExhausted: the installed code has already minted its maximum
	// number of sessions.
	pairingExhausted
)

// String renders the rejection reason for logs (pairingFailReason is an
// int enum; %v would print a bare number).
func (r pairingFailReason) String() string {
	switch r {
	case pairingAccepted:
		return "accepted"
	case pairingWrongCode:
		return "wrong code"
	case pairingInvalid:
		return "invalid (expired / earlier boot / not installed)"
	case pairingExhausted:
		return "exhausted (max uses reached)"
	default:
		return fmt.Sprintf("unknown(%d)", int(r))
	}
}

// User-facing messages for the pairing failure page. PairingTTL and
// maxPairingUses are woven in at render time so the text cannot drift from
// the constants.
const (
	pairingMsgWrong        = "That pairing code is invalid — it may be from an earlier server start. Check the code printed in the terminal: a fresh link and QR are printed at every server start."
	pairingMsgEnterCode    = "Enter the pairing code shown in the terminal where gogen was started."
	pairingMsgExhaustedFmt = "This pairing code has already been used %d times. Use the link and QR printed when the server last started."
	pairingMsgInvalidFmt   = "This pairing code expired (codes last %.0f minutes) or belongs to an earlier server start. A fresh link and QR are printed at every server start — use those, or type the current code below."
	pairingMsgPreview      = "This sign-in link has to be opened as a page in a web browser. If a camera app is showing a preview, open the link in the browser instead — or enter the pairing code below."
	pairingMsgCookieLost   = "A pairing code from this device was accepted moments ago, but this browser did not keep the sign-in cookie — so every sign-in, including typing the code below, silently fails. Either the scan opened in a different browser than this one (camera app / in-app browser), or this browser is blocking cookies for this address (e.g. Brave Shields, a private tab). Allow cookies for this address — or open the pairing link in the browser you are using right now — and sign in again."
)

// SetPairingCode installs the onboarding pairing code for this server run.
// The code is short-lived by design (see PairingTTL): it is what the
// printed link and QR code carry, never the long-lived auth token, so a
// shoulder-surfed QR cannot mint sessions beyond its expiry.
func (s *Server) SetPairingCode(code string, expiry time.Time) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	s.pairCode = code
	s.pairExpiry = expiry
	s.pairMinted = time.Now()
	s.pairUses = 0
}

// PairingCode returns the current pairing code and its expiry. Callers use
// it to print the onboarding link/QR before the server starts listening.
func (s *Server) PairingCode() (string, time.Time) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	return s.pairCode, s.pairExpiry
}

// consumePairingCode validates a candidate against the installed pairing
// code and atomically consumes one use on success. The comparison is
// constant-time (via tokenMatches) on lowercased forms — the code is random
// hex, so case never matters — and does not leak timing information about
// the code value.
func (s *Server) consumePairingCode(candidate string) pairingFailReason {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.pairCode == "" {
		// Production installs a code whenever auth is active (runWeb); a
		// bare test/embed server can hit this. Log so a misconfigured
		// build surfaces instead of a silent "expired" page.
		log.Printf("pairing: no pairing code installed while auth is active")
		return pairingInvalid
	}
	if time.Now().After(s.pairExpiry) {
		return pairingInvalid
	}
	if s.pairUses >= maxPairingUses {
		return pairingExhausted
	}
	if !tokenMatches(strings.ToLower(candidate), strings.ToLower(s.pairCode)) {
		return pairingWrongCode
	}
	s.pairUses++
	return pairingAccepted
}

// notePairingAccept records the most recent accepted pairing exchange
// (source IP + time) under pairMu. The unauthenticated-page path uses it
// to recognize a browser-side cookie failure: an exchange succeeded from
// this device moments ago, yet the next request arrives with no cookie.
func (s *Server) notePairingAccept(ip string) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	s.lastPairIP = ip
	s.lastPairAt = time.Now()
}

// recentPairingAccept reports when a pairing exchange was last accepted
// from ip, and whether that was within the diagnosis window. The window is
// generous (minutes, not seconds) because the user typically fumbles with
// settings or rescans before the follow-up request arrives; a stale match
// is harmless — the worst case is a diagnostic hint on a page the user
// was going to see anyway.
func (s *Server) recentPairingAccept(ip string) (time.Time, bool) {
	const window = 5 * time.Minute
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.lastPairIP != ip || s.lastPairAt.IsZero() {
		return time.Time{}, false
	}
	elapsed := time.Since(s.lastPairAt)
	return s.lastPairAt, elapsed >= 0 && elapsed <= window
}

// pairingFailureMessage renders the failure-page text for a rejected
// pairing exchange.
func pairingFailureMessage(reason pairingFailReason) string {
	switch reason {
	case pairingWrongCode:
		return pairingMsgWrong
	case pairingExhausted:
		return fmt.Sprintf(pairingMsgExhaustedFmt, maxPairingUses)
	default:
		return fmt.Sprintf(pairingMsgInvalidFmt, PairingTTL.Minutes())
	}
}

// pairingDiagnostic returns an optional, state-derived sentence appended to
// the failure page so the user can tell a stale QR from an expired or
// exhausted code. It never contains the code itself; empty when there is no
// useful detail (e.g. no code installed).
func (s *Server) pairingDiagnostic(reason pairingFailReason) string {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	switch reason {
	case pairingWrongCode:
		if !s.pairMinted.IsZero() {
			return fmt.Sprintf(" (The current code was printed at %s — check that you scanned that one.)", s.pairMinted.Local().Format("15:04:05"))
		}
	case pairingInvalid:
		if !s.pairMinted.IsZero() {
			if time.Now().After(s.pairExpiry) {
				return fmt.Sprintf(" (It expired at %s.)", s.pairExpiry.Local().Format("15:04:05"))
			}
			return fmt.Sprintf(" (The current code was printed at %s — a restart mints a new one.)", s.pairMinted.Local().Format("15:04:05"))
		}
	}
	return ""
}

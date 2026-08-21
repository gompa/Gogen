package server

import (
	"strings"
	"testing"
	"time"
)

func TestConsumePairingCode(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		code      string
		expiry    time.Time
		uses      int
		candidate string
		want      pairingFailReason
	}{
		{"valid", "code123", now.Add(time.Hour), 0, "code123", pairingAccepted},
		{"uppercase hex candidate", "code123", now.Add(time.Hour), 0, "CODE123", pairingAccepted},
		{"wrong code", "code123", now.Add(time.Hour), 0, "nope", pairingWrongCode},
		{"empty candidate", "code123", now.Add(time.Hour), 0, "", pairingWrongCode},
		{"expired", "code123", now.Add(-time.Minute), 0, "code123", pairingInvalid},
		{"no code installed", "", now.Add(time.Hour), 0, "code123", pairingInvalid},
		{"exhausted", "code123", now.Add(time.Hour), maxPairingUses, "code123", pairingExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{pairCode: tc.code, pairExpiry: tc.expiry, pairUses: tc.uses}
			if got := s.consumePairingCode(tc.candidate); got != tc.want {
				t.Fatalf("consumePairingCode(%q) = %v, want %v", tc.candidate, got, tc.want)
			}
		})
	}
}

func TestConsumePairingCodeBoundedUses(t *testing.T) {
	s := &Server{pairCode: "code", pairExpiry: time.Now().Add(time.Hour)}
	for i := 0; i < maxPairingUses; i++ {
		if got := s.consumePairingCode("code"); got != pairingAccepted {
			t.Fatalf("use %d = %v, want accepted", i+1, got)
		}
	}
	if got := s.consumePairingCode("code"); got != pairingExhausted {
		t.Fatalf("use beyond maxPairingUses = %v, want pairingExhausted", got)
	}
}

func TestPairingCodeRoundTrip(t *testing.T) {
	s := &Server{}
	expiry := time.Now().Add(PairingTTL)
	s.SetPairingCode("abc", expiry)
	code, got := s.PairingCode()
	if code != "abc" || !got.Equal(expiry) {
		t.Fatalf("PairingCode() = (%q, %v), want (%q, %v)", code, got, "abc", expiry)
	}
	// SetPairingCode resets the use counter.
	s.pairUses = maxPairingUses
	s.SetPairingCode("abc", expiry)
	if got := s.consumePairingCode("abc"); got != pairingAccepted {
		t.Fatal("fresh pairing code should be consumable")
	}
}

func TestPairingFailureMessages(t *testing.T) {
	if msg := pairingFailureMessage(pairingWrongCode); !strings.Contains(msg, "invalid") {
		t.Fatalf("wrong-code message = %q, want it to mention the code is invalid", msg)
	}
	if msg := pairingFailureMessage(pairingExhausted); !strings.Contains(msg, "already been used") {
		t.Fatalf("exhausted message = %q, want it to mention the use cap", msg)
	}
	if msg := pairingFailureMessage(pairingInvalid); !strings.Contains(msg, "15 minutes") {
		t.Fatalf("invalid message = %q, want it to state the TTL from PairingTTL", msg)
	}
}

func TestPairingDiagnostic(t *testing.T) {
	s := &Server{}
	if got := s.pairingDiagnostic(pairingWrongCode); got != "" {
		t.Fatalf("no code installed: diagnostic = %q, want empty", got)
	}
	s.SetPairingCode("abc", time.Now().Add(time.Hour))
	if got := s.pairingDiagnostic(pairingWrongCode); !strings.Contains(got, "printed at") {
		t.Fatalf("wrong-code diagnostic should mention the mint time, got %q", got)
	}
	s.SetPairingCode("abc", time.Now().Add(-time.Minute))
	if got := s.pairingDiagnostic(pairingInvalid); !strings.Contains(got, "expired at") {
		t.Fatalf("invalid diagnostic should mention the expiry time, got %q", got)
	}
}

package server

import (
	"net/http/httptest"
	"testing"
)

func TestCheckWSOrigin(t *testing.T) {
	allowed := parseAllowedOrigins("")
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"empty origin", "", "127.0.0.1:8080", true},
		{"localhost", "http://localhost:8080", "127.0.0.1:8080", true},
		{"127.0.0.1", "http://127.0.0.1:8080", "127.0.0.1:8080", true},
		{"same host", "http://192.168.1.5:8080", "192.168.1.5:8080", true},
		{"evil", "https://evil.example", "127.0.0.1:8080", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+tc.host+"/ws", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := checkWSOrigin(req, allowed); got != tc.want {
				t.Fatalf("checkWSOrigin() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseAllowedOriginsCustom(t *testing.T) {
	allowed := parseAllowedOrigins("https://app.example.com, staging.local")
	if _, ok := allowed["app.example.com"]; !ok {
		t.Fatal("expected app.example.com")
	}
	if _, ok := allowed["staging.local"]; !ok {
		t.Fatal("expected staging.local")
	}
}

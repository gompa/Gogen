package agent

import (
	"strings"
	"testing"
)

// TestIsPrivateHostLiterals pins the private-host predicate for literals,
// including IPv6 literals. Regression guard for the bug where the port was
// stripped with strings.LastIndex(host, ":") and the mangled (bracketed)
// result was handed to net.LookupIP — which rejects brackets — so every
// IPv6-literal URL was wrongly reported as a private/internal host.
//
// The predicate is now purely syntactic: it must classify these literals
// without any DNS resolution (the "example.com" case would previously have
// required a resolver round-trip).
func TestIsPrivateHostLiterals(t *testing.T) {
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"empty", "", true},
		{"localhost", "localhost", true},
		{"localhost uppercase", "LOCALHOST", true},
		{"localhost.localdomain", "localhost.localdomain", true},

		{"ipv4 loopback", "127.0.0.1", true},
		{"ipv4 private 10/8", "10.1.2.3", true},
		{"ipv4 private 172.16/12", "172.16.0.1", true},
		{"ipv4 private 192.168/16", "192.168.1.1", true},
		{"ipv4 link-local / metadata", "169.254.169.254", true},
		{"ipv4 unspecified", "0.0.0.0", true},
		{"ipv4 public", "8.8.8.8", false},

		{"ipv6 loopback bare", "::1", true},
		{"ipv6 loopback bracketed", "[::1]", true},
		{"ipv6 loopback bracketed with port", "[::1]:443", true},
		{"ipv6 link-local bare", "fe80::1", true},
		{"ipv6 link-local bracketed", "[fe80::1]", true},
		{"ipv6 link-local bracketed with port", "[fe80::1]:80", true},
		{"ipv6 unspecified", "::", true},
		{"ipv6 public bare", "2606:4700:4700::1111", false},
		{"ipv6 public bracketed", "[2606:4700:4700::1111]", false},
		{"ipv6 public bracketed with port", "[2606:4700:4700::1111]:443", false},

		{"hostname not resolved here", "example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrivateHost(tc.host); got != tc.want {
				t.Errorf("isPrivateHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestValidateFetchURLIPv6Literals is the end-to-end guard for web_fetch and
// download_file: a public IPv6-literal URL must validate (previously the host
// was mangled before resolution and the URL was rejected), while a loopback or
// link-local IPv6 literal must stay blocked. It performs no network I/O.
func TestValidateFetchURLIPv6Literals(t *testing.T) {
	publicURL := "https://[2606:4700:4700::1111]/dns-query"
	if _, err := validateFetchURL(publicURL); err != nil {
		t.Fatalf("public IPv6 literal URL rejected: %v", err)
	}

	blocked := []string{
		"https://[::1]:8443/admin",
		"https://[::1]/admin",
		"https://[fe80::1]/",
	}
	for _, raw := range blocked {
		u, err := validateFetchURL(raw)
		if err == nil {
			t.Fatalf("validateFetchURL(%q) = %v, want private/internal block", raw, u)
		}
		if !strings.Contains(err.Error(), "private/internal hosts are blocked") {
			t.Fatalf("validateFetchURL(%q) error = %v, want private/internal block", raw, err)
		}
	}
}

package server

import (
	"net"
	"testing"
)

func ipv4Net(ip string) net.Addr {
	return &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(24, 32)}
}

func ipv6Net(ip string) net.Addr {
	return &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(64, 128)}
}

func TestFirstLANIPv4From(t *testing.T) {
	lan := ifaceAddrs{addrs: []net.Addr{ipv4Net("192.168.1.7")}}
	lo := ifaceAddrs{loopback: true, addrs: []net.Addr{ipv4Net("127.0.0.1")}}
	linkLocal := ifaceAddrs{addrs: []net.Addr{ipv4Net("169.254.3.4")}}
	lan2 := ifaceAddrs{addrs: []net.Addr{ipv4Net("10.0.0.2")}}
	ipv6Only := ifaceAddrs{addrs: []net.Addr{ipv6Net("fe80::1")}}
	lan3 := ifaceAddrs{addrs: []net.Addr{ipv4Net("192.168.0.9")}}
	badAddrs := ifaceAddrs{addrs: []net.Addr{ipv4Net("127.0.0.1"), ipv4Net("0.0.0.0")}}
	multi := ifaceAddrs{addrs: []net.Addr{ipv4Net("10.1.2.3"), ipv4Net("192.168.5.5")}}

	cases := []struct {
		name   string
		ifaces []ifaceAddrs
		want   string
	}{
		{"no interfaces", nil, ""},
		{"loopback only", []ifaceAddrs{lo}, ""},
		{"single lan", []ifaceAddrs{lo, lan}, "192.168.1.7"},
		{"skips link-local", []ifaceAddrs{linkLocal, lan2}, "10.0.0.2"},
		{"skips ipv6-only", []ifaceAddrs{ipv6Only, lan3}, "192.168.0.9"},
		{"skips loopback and unspecified addrs", []ifaceAddrs{badAddrs}, ""},
		{"prefers first usable", []ifaceAddrs{multi}, "10.1.2.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstLANIPv4From(tc.ifaces); got != tc.want {
				t.Fatalf("firstLANIPv4From() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLANHost(t *testing.T) {
	if got := LANHost("127.0.0.1:8080"); got != "" {
		t.Fatalf("loopback bind: LANHost() = %q, want empty", got)
	}
	if got := LANHost("localhost:8080"); got != "" {
		t.Fatalf("localhost bind: LANHost() = %q, want empty", got)
	}
	if got := LANHost("[::1]:8080"); got != "" {
		t.Fatalf("ipv6 loopback bind: LANHost() = %q, want empty", got)
	}
	if got := LANHost("192.168.1.5:8080"); got != "192.168.1.5" {
		t.Fatalf("concrete bind: LANHost() = %q, want 192.168.1.5", got)
	}
	// Unspecified binds resolve via the machine's real interfaces; assert
	// the invariant that any result is a usable non-loopback IPv4.
	for _, addr := range []string{"0.0.0.0:8080", ":8080"} {
		got := LANHost(addr)
		if got == "" {
			continue // no LAN interface on this machine — acceptable
		}
		ip := net.ParseIP(got)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
			t.Fatalf("LANHost(%q) = %q, want a usable IPv4 address", addr, got)
		}
	}
}

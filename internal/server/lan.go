package server

import (
	"net"
	"strings"
)

// LANHost returns the host to embed in a phone-reachable URL for the given
// listen address, or "" when no usable LAN address exists (a loopback-only
// bind, or a machine with no non-loopback IPv4 interface).
//
// A concrete non-loopback bind (e.g. "192.168.1.5:8080") is used as-is; an
// unspecified bind ("0.0.0.0", "::", ":port") resolves the default-route
// interface's IPv4, falling back to the first non-loopback IPv4 across all
// interfaces.
func LANHost(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		// ":port" form: all interfaces → discover below.
	} else if strings.EqualFold(host, "localhost") {
		// "localhost" is a loopback name — a phone can never reach it.
		return ""
	} else if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsLoopback():
			return ""
		case ip.IsUnspecified():
			// 0.0.0.0 / :: → discover below.
		default:
			return host
		}
	}
	if h, ok := defaultRouteIPv4(); ok {
		return h
	}
	return firstLANIPv4()
}

// defaultRouteIPv4 returns the IPv4 address of the interface that owns the
// default route. net.Dial on a UDP socket performs only a route lookup (no
// packets are sent), so this works offline and never touches the network.
func defaultRouteIPv4() (string, bool) {
	c, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "", false
	}
	defer c.Close()
	host, _, err := net.SplitHostPort(c.LocalAddr().String())
	if err != nil {
		return "", false
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return "", false
	}
	return ip.String(), true
}

// ifaceAddrs is the interface data firstLANIPv4From needs, decoupled from
// net.Interface so tests can inject a fixed set (net.Interface.Addrs is a
// method backed by the OS, not a field).
type ifaceAddrs struct {
	loopback bool
	addrs    []net.Addr
}

// firstLANIPv4 returns the first non-loopback, non-link-local IPv4 address
// across the machine's interfaces, or "" if there is none.
func firstLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	probe := make([]ifaceAddrs, 0, len(ifaces))
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			probe = append(probe, ifaceAddrs{loopback: true})
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		probe = append(probe, ifaceAddrs{addrs: addrs})
	}
	return firstLANIPv4From(probe)
}

// firstLANIPv4From scans ifaces (kernel order) for the first usable IPv4
// address. It skips loopback interfaces, loopback/unspecified addresses,
// IPv6-only addresses, and 169.254.0.0/16 link-local addresses (which are
// not reachable from other hosts).
func firstLANIPv4From(ifaces []ifaceAddrs) string {
	for _, ifc := range ifaces {
		if ifc.loopback {
			continue
		}
		for _, a := range ifc.addrs {
			ip := addrIP(a)
			if ip == nil {
				continue
			}
			v4 := ip.To4()
			if v4 == nil || v4.IsLoopback() || v4.IsUnspecified() {
				continue
			}
			if v4[0] == 169 && v4[1] == 254 {
				continue
			}
			return v4.String()
		}
	}
	return ""
}

// addrIP extracts the IP from the concrete net.Addr types interfaces
// report. Unknown types return nil.
func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}

// Package netutil has small networking helpers SwarmDialer needs to be
// deployable on an arbitrary fresh VPS without the user having to look up
// and type in networking details by hand.
package netutil

import (
	"fmt"
	"net"
)

// DetectLocalIP determines which local IP address this machine would use
// to reach remoteHost (a "host" or "host:port" string) — the address to
// advertise in SIP Contact headers when registering extensions on that
// PBXware server.
//
// This works by asking the OS to pick a route via a UDP "dial" and reading
// back the local address it chose. UDP dial doesn't actually transmit
// anything — it's a routing-table lookup, not a network probe — so this is
// safe to call even if remoteHost is unreachable or the port is wrong.
func DetectLocalIP(remoteHost string) (string, error) {
	host := remoteHost
	if h, _, err := net.SplitHostPort(remoteHost); err == nil {
		host = h
	}

	conn, err := net.Dial("udp", net.JoinHostPort(host, "5060"))
	if err != nil {
		return "", fmt.Errorf("determining local route to %s: %w", remoteHost, err)
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected local address type %T", conn.LocalAddr())
	}
	return addr.IP.String(), nil
}

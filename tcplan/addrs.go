// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tcplan

import (
	"net"
	"net/netip"
	"strconv"
)

// localCandidates returns this host's dialable LAN "host:port" addresses: the
// private (RFC1918 / unique-local) and link-local unicast IPs of its up,
// non-loopback interfaces, each paired with our listener port.
//
// We deliberately advertise only LAN-scoped addresses. This transport is for
// peers that share a local network; a public address would just duplicate what
// magicsock's direct-UDP path already discovers, and we never want tcplan to
// attempt the open internet. Tailscale 100.64.0.0/10 (CGNAT) addresses are
// excluded too: dialing a peer over its Tailscale IP would tunnel this
// transport through WireGuard itself.
func (m *manager) localCandidates() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		m.logf("tcplan: enumerate interfaces: %v", err)
		return nil
	}
	port := strconv.Itoa(m.localPort)
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if !isLANCandidate(ip) {
				continue
			}
			out = append(out, net.JoinHostPort(ip.String(), port))
		}
	}
	return out
}

// tailscaleCGNAT is the 100.64.0.0/10 range Tailscale assigns node IPs from.
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// isLANCandidate reports whether ip is a sensible LAN dial target: private or
// link-local unicast, but not a Tailscale CGNAT address (which would route this
// transport back through WireGuard).
func isLANCandidate(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if tailscaleCGNAT.Contains(ip) {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

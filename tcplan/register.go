// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tcplan

import (
	"tailscale.com/wgengine/magicsock"
)

// transportName is the name this transport registers under. It is the
// transport's identity: it selects the transport's magic-IP slot locally and is
// carried in [disco.TransportSignal] so the receiving side dispatches signaling
// back to this transport. The "pluggable-transports" feature capability is
// recorded once by magicsock in RegisterTransport.
const transportName = "tcplan"

func init() {
	// Rank above a direct UDP path: on a shared LAN, especially on OSes whose
	// TCP stack is far better optimized than their UDP path (e.g. macOS-to-macOS
	// on 10 GbE), this TCP stream is expected to outperform direct UDP. magicsock
	// still only promotes it on disco pong evidence and falls back otherwise.
	magicsock.RegisterTransport(transportName, newManager, magicsock.WithPriorityOverDirect())
}

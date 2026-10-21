// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package webrtc

import (
	"tailscale.com/wgengine/magicsock"
)

// transportName is the name this transport registers under. It is the
// transport's identity: it selects the transport's magic-IP slot locally and is
// carried in [disco.TransportSignal] so the receiving side dispatches signaling
// back to this transport. It is distinct from the "pluggable-transports"
// feature capability, which magicsock records once in RegisterTransport.
const transportName = "webrtc"

func init() {
	magicsock.RegisterTransport(transportName, newManager)
}

// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package webrtc

// This file implements the send side of WebRTC signaling: offers, answers, and
// ICE candidates are JSON-encoded and sent to the peer as pluggable-transport
// signaling messages over DERP (via the magicsock transport host). Incoming
// messages arrive separately, through magicsock's disco dispatch into
// [manager.HandleSignal].
//
// magicsock's [disco.TransportSignal] carries only an opaque payload for this
// transport, so the offer/answer/candidate distinction lives in a 1-byte kind
// prefix that this package owns (see [signalKind] and the payload wire format:
// [kind byte][JSON]).

import (
	"encoding/json"
	"fmt"

	"github.com/pion/webrtc/v4"
	"tailscale.com/types/key"
)

// signalKind identifies which kind of WebRTC signaling message a payload
// carries. It is the first byte of the opaque transport-signal payload.
type signalKind uint8

const (
	signalOffer        signalKind = 0 // JSON payload is a webrtc.SessionDescription
	signalAnswer       signalKind = 1 // JSON payload is a webrtc.SessionDescription
	signalICECandidate signalKind = 2 // JSON payload is a webrtc.ICECandidateInit
)

func (k signalKind) String() string {
	switch k {
	case signalOffer:
		return "offer"
	case signalAnswer:
		return "answer"
	case signalICECandidate:
		return "ice-candidate"
	}
	return "unknown"
}

// sendOffer sends an SDP offer to the peer with the given disco key.
func (m *manager) sendOffer(to key.DiscoPublic, offer *webrtc.SessionDescription) error {
	return sendSignal(m, signalOffer, to, offer)
}

// sendAnswer sends an SDP answer to the peer with the given disco key.
func (m *manager) sendAnswer(to key.DiscoPublic, answer *webrtc.SessionDescription) error {
	return sendSignal(m, signalAnswer, to, answer)
}

// sendCandidate sends an ICE candidate to the peer with the given disco key.
func (m *manager) sendCandidate(to key.DiscoPublic, candidate *webrtc.ICECandidateInit) error {
	return sendSignal(m, signalICECandidate, to, candidate)
}

// sendSignal JSON-encodes msg, prefixes it with the kind byte, and sends the
// resulting payload to the peer via the transport host. It is the shared body
// of the three send methods above, which differ only in msg's type and the kind.
func sendSignal[T any](m *manager, kind signalKind, to key.DiscoPublic, msg *T) error {
	j, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("webrtc: marshal %s: %w", kind, err)
	}
	payload := make([]byte, 1+len(j))
	payload[0] = byte(kind)
	copy(payload[1:], j)
	return m.b.SendSignal(to, payload)
}

// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tcplan

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"

	"go4.org/mem"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/magicsock"
)

var errBadHello = errors.New("tcplan: bad hello")

// The signaling protocol is intentionally tiny, since both sides are on the
// same LAN and the only thing to negotiate is "where do I dial you".
//
// It is a single message kind, [offer], sent over disco (TypeTransportSignal)
// via DERP. When a peer's traffic is flowing over DERP, magicsock calls
// EnsureConnecting; we respond by sending that peer an offer listing our LAN
// candidate host:ports. The peer dials one of them, and over the raw TCP
// stream sends a [hello] line identifying itself. No answer travels back over
// disco — the TCP connection succeeding IS the answer.
//
// Glare (both peers dialing each other at once) is resolved the same way the
// webrtc transport breaks its offer glare: by disco-key comparison. Only the
// peer with the lexicographically smaller disco key sends offers / dials; the
// larger-keyed peer only listens. This guarantees exactly one TCP stream.

// offer is the JSON payload of a tcplan transport signal.
type offer struct {
	// Candidates are the sender's dialable LAN "host:port" addresses.
	Candidates []string `json:"candidates"`
}

// handshakeTimeout bounds the post-dial hello exchange.
const handshakeTimeout = 3 * time.Second

// EnsureConnecting already filtered for eligibility; handleStart does the work
// on the run loop: decide dial role, enumerate candidates, send an offer.
func (m *manager) handleStart(p magicsock.TransportPeer) {
	dk, ok := p.DiscoKey()
	if !ok {
		return
	}
	if !p.DERPReady() {
		// Signaling rides DERP; without a DERP path the offer can't be sent.
		// A later send still on DERP will re-trigger EnsureConnecting.
		return
	}
	// Glare tiebreak: only the smaller-keyed side initiates. The larger-keyed
	// side waits to receive the offer and dials in response.
	if m.host.LocalDiscoKey().Compare(dk) >= 0 {
		return
	}
	m.sendOffer(dk)
}

// sendOffer advertises our LAN candidates to the peer over disco.
func (m *manager) sendOffer(dst key.DiscoPublic) {
	cands := m.localCandidates()
	if len(cands) == 0 {
		m.logf("tcplan: no LAN candidates to offer %v", dst.ShortString())
		return
	}
	payload, err := json.Marshal(offer{Candidates: cands})
	if err != nil {
		m.logf("tcplan: marshal offer: %v", err)
		return
	}
	if err := m.host.SendSignal(dst, payload); err != nil {
		m.logf("tcplan: send offer to %v: %v", dst.ShortString(), err)
	}
}

// HandleSignal implements [magicsock.Transport]. The payload is a JSON [offer];
// we dial the advertised candidates and, on the first that connects and
// completes the hello handshake, register the stream.
func (m *manager) HandleSignal(fromDisco key.DiscoPublic, payload []byte) {
	if m.host.Disabled() {
		return
	}
	var o offer
	if err := json.Unmarshal(payload, &o); err != nil {
		m.logf("tcplan: bad offer from %v: %v", fromDisco.ShortString(), err)
		return
	}
	peer, ok := m.host.PeerForDisco(fromDisco)
	if !ok {
		m.logf("tcplan: offer from unknown peer %v", fromDisco.ShortString())
		return
	}
	if peer.IsJS() {
		return // can't happen for a LAN TCP peer, but stay consistent with eligibility
	}
	// Already connected? Nothing to do.
	m.mu.Lock()
	existing, have := m.peersByDisco[fromDisco]
	live := have && existing.isLive()
	m.mu.Unlock()
	if live {
		return
	}
	// Dial on a separate goroutine so we don't block disco dispatch.
	go m.dialCandidates(peer, fromDisco, o.Candidates)
}

// dialCandidates tries each advertised candidate in turn; the first to connect
// and complete the hello handshake wins.
func (m *manager) dialCandidates(peer magicsock.TransportPeer, remoteDisco key.DiscoPublic, candidates []string) {
	localDisco := m.host.LocalDiscoKey()

	for _, cand := range candidates {
		if m.listenerCtx.Err() != nil {
			return
		}
		ap, err := netip.ParseAddrPort(cand)
		if err != nil {
			continue
		}
		d := net.Dialer{Timeout: dialTimeout}
		conn, err := d.DialContext(m.listenerCtx, "tcp", ap.String())
		if err != nil {
			continue
		}
		// Send hello identifying ourselves so the listener can map this stream
		// to our disco key (and thus our endpoint).
		if err := writeHello(conn, localDisco); err != nil {
			conn.Close()
			continue
		}
		pc := &peerConn{
			remoteDisco:   remoteDisco,
			remoteNodeKey: peer.NodeKey(),
			remoteAddr:    conn.RemoteAddr().String(),
			conn:          conn,
		}
		m.registerConn(pc, peer)
		return
	}
	m.logf("tcplan: no candidate reachable for %v", remoteDisco.ShortString())
}

// acceptLoop accepts inbound TCP streams, reads the dialer's hello to learn its
// disco key, resolves the corresponding endpoint, and registers the stream.
func (m *manager) acceptLoop() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			if m.listenerCtx.Err() != nil {
				return // listener closed
			}
			m.logf("tcplan: accept: %v", err)
			continue
		}
		go m.handleInbound(conn)
	}
}

func (m *manager) handleInbound(conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	remoteDisco, err := readHello(conn)
	if err != nil {
		m.logf("tcplan: inbound hello: %v", err)
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{}) // clear; frames have no deadline

	peer, ok := m.host.PeerForDisco(remoteDisco)
	if !ok {
		m.logf("tcplan: inbound from unknown disco %v", remoteDisco.ShortString())
		conn.Close()
		return
	}
	pc := &peerConn{
		remoteDisco:   remoteDisco,
		remoteNodeKey: peer.NodeKey(),
		remoteAddr:    conn.RemoteAddr().String(),
		conn:          conn,
	}
	m.registerConn(pc, peer)
}

// hello wire format: the 4-byte magic "TCPL", then the 32-byte disco public
// key of the dialer. Fixed-size, so readHello needs no length negotiation. The
// dialer does not send its node key: the listener derives it from the disco key
// via PeerForDisco, exactly as magicsock does for DERP-delivered packets.
var helloMagic = [4]byte{'T', 'C', 'P', 'L'}

func writeHello(conn net.Conn, localDisco key.DiscoPublic) error {
	buf := make([]byte, 0, len(helloMagic)+32)
	buf = append(buf, helloMagic[:]...)
	buf = localDisco.AppendTo(buf)
	conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetWriteDeadline(time.Time{})
	_, err := conn.Write(buf)
	return err
}

func readHello(conn net.Conn) (key.DiscoPublic, error) {
	var buf [len(helloMagic) + 32]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		return key.DiscoPublic{}, err
	}
	if [4]byte(buf[:4]) != helloMagic {
		return key.DiscoPublic{}, errBadHello
	}
	return key.DiscoPublicFromRaw32(mem.B(buf[4:])), nil
}

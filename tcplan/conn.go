// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tcplan

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"tailscale.com/types/key"
	"tailscale.com/wgengine/magicsock"
)

// peerConn is one TCP stream to a peer, plus the metadata magicsock needs. A
// peerConn is created either by the dialer (handleStart) or the accept loop,
// and lives until its read loop sees an error or the manager closes.
type peerConn struct {
	remoteDisco   key.DiscoPublic
	remoteNodeKey key.NodePublic
	remoteAddr    string // the dialed-or-accepted TCP remote, for status display

	// writeMu serializes frame writes so concurrent senders can't interleave a
	// length prefix with another packet's body on the stream.
	writeMu sync.Mutex
	conn    net.Conn

	live atomic.Bool // false once the read loop exits or close() runs
}

func (pc *peerConn) isLive() bool { return pc != nil && pc.live.Load() }

func (pc *peerConn) close() {
	if pc.live.CompareAndSwap(true, false) {
		pc.conn.Close()
	}
}

// Framing on the wire is a 4-byte big-endian length followed by that many
// bytes of payload. Unlike the webrtc datachannel, a TCP stream has no message
// boundaries, so every packet (or batch) must be explicitly length-delimited.
//
// maxFrame bounds a single frame so a corrupt or hostile length prefix can't
// make us allocate unbounded memory. A WireGuard batch of 64 packets is well
// under 100 KiB; 1 MiB is generous headroom.
const (
	frameHeaderLen = 4
	maxFrame       = 1 << 20
)

// SendPacket implements [magicsock.Transport]. b is a single WireGuard packet
// or a [magicsock.TransportBatchMagic]-framed batch; either way it is written
// as one length-delimited frame and de-framed on the far side by readLoop.
func (m *manager) SendPacket(dst key.DiscoPublic, b []byte) error {
	m.mu.Lock()
	pc, ok := m.peersByDisco[dst]
	m.mu.Unlock()
	if !ok || !pc.isLive() {
		return errors.New("tcplan: no connection")
	}
	if len(b) > maxFrame {
		return errors.New("tcplan: frame too large")
	}

	var hdr [frameHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))

	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()
	if _, err := pc.conn.Write(hdr[:]); err != nil {
		pc.close()
		return err
	}
	if _, err := pc.conn.Write(b); err != nil {
		pc.close()
		return err
	}
	return nil
}

// GetRemoteAddr implements [magicsock.Transport].
func (m *manager) GetRemoteAddr(dst key.DiscoPublic) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pc, ok := m.peersByDisco[dst]; ok && pc.isLive() {
		return pc.remoteAddr
	}
	return ""
}

// readLoop reads length-delimited frames off pc.conn and hands each one to the
// receive pipeline. It de-frames a TransportBatchMagic batch the same way the
// webrtc transport does, so a single frame may deliver many WireGuard packets.
func (m *manager) readLoop(pc *peerConn) {
	defer func() {
		pc.close()
		m.mu.Lock()
		// Only delete if we're still the current conn for this disco key; a
		// newer conn may have replaced us during a reconnect.
		if cur, ok := m.peersByDisco[pc.remoteDisco]; ok && cur == pc {
			delete(m.peersByDisco, pc.remoteDisco)
		}
		m.mu.Unlock()
	}()

	var hdr [frameHeaderLen]byte
	buf := make([]byte, maxFrame)
	for {
		if _, err := io.ReadFull(pc.conn, hdr[:]); err != nil {
			if !isExpectedCloseErr(err) {
				m.logf("tcplan: read header from %v: %v", pc.remoteDisco.ShortString(), err)
			}
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 {
			continue // keepalive / empty frame
		}
		if n > maxFrame {
			m.logf("tcplan: oversize frame (%d) from %v; closing", n, pc.remoteDisco.ShortString())
			return
		}
		if _, err := io.ReadFull(pc.conn, buf[:n]); err != nil {
			if !isExpectedCloseErr(err) {
				m.logf("tcplan: read body from %v: %v", pc.remoteDisco.ShortString(), err)
			}
			return
		}
		m.deliverFrame(pc, buf[:n])
	}
}

// deliverFrame de-frames one received frame and delivers each WireGuard packet
// to magicsock. It mirrors the webrtc transport's deliverMsg so both transports
// speak the identical batch wire format with magicsock's send side.
func (m *manager) deliverFrame(pc *peerConn, data []byte) {
	if len(data) == 0 {
		return
	}
	if data[0] == magicsock.TransportBatchMagic {
		data = data[1:]
		for len(data) >= 2 {
			pktLen := int(binary.BigEndian.Uint16(data))
			data = data[2:]
			if pktLen > len(data) {
				m.logf("tcplan: batch framing error from %v", pc.remoteDisco.ShortString())
				return
			}
			m.host.DeliverPacket(data[:pktLen], pc.remoteNodeKey)
			data = data[pktLen:]
		}
		return
	}
	m.host.DeliverPacket(data, pc.remoteNodeKey)
}

// registerConn installs pc as the live connection for its disco key, closing
// and replacing any existing one, marks the transport channel ready, and starts
// its read loop. Used by both the dialer and the accept loop.
func (m *manager) registerConn(pc *peerConn, peer magicsock.TransportPeer) {
	pc.live.Store(true)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		pc.close()
		return
	}
	if old, ok := m.peersByDisco[pc.remoteDisco]; ok && old != pc {
		old.close()
	}
	m.peersByDisco[pc.remoteDisco] = pc
	m.mu.Unlock()

	// Tell magicsock the path is ready; it will probe it with disco and promote
	// it over DERP if pongs confirm. Liveness remains pong-driven.
	if peer != nil {
		m.host.SetChannelReady(peer, true)
	}
	m.logf("tcplan: connected to %v (%s)", pc.remoteDisco.ShortString(), pc.remoteAddr)

	go m.readLoop(pc)
}

func isExpectedCloseErr(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF)
}

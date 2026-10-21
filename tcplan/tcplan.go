// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package tcplan implements a TCP LAN pluggable transport for Tailscale's
// magicsock.
//
// It carries WireGuard traffic over a plain TCP connection between two peers
// that share a LAN. It exists for the case the WebRTC transport does not serve:
// native (non-browser) peers on the same high-speed local network, on operating
// systems whose TCP stack is far better optimized than their UDP path (e.g.
// macOS-to-macOS on 10 GbE). On such links a single long-lived TCP stream can
// beat both direct UDP and DERP.
//
// Where the webrtc transport is datachannel + ICE + browser-oriented, tcplan is
// deliberately its structural opposite:
//
//   - Medium: a reliable, length-framed TCP stream, not an unreliable datachannel.
//   - Discovery: it enumerates the host's own LAN (RFC1918 / link-local)
//     interface addresses and advertises them; the peer dials them directly. No
//     STUN, no ICE, no relay.
//   - Signaling: a single [offer] over disco carrying the lister's node/disco
//     identity and its candidate host:port list. The dialer connects, sends a
//     one-line handshake identifying itself, and the stream is up.
//   - Eligibility: it only attempts peers that are NOT js (a browser has no
//     listening TCP socket on a LAN), the mirror of webrtc's js-only rule.
//
// magicsock has no compile-time dependency on this package; importing it is
// what links the transport in (its init calls [magicsock.RegisterTransport]).
package tcplan

import (
	"context"
	"net"
	"sync"
	"time"

	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/magicsock"
)

// dialTimeout bounds how long we wait for a single TCP connect to a candidate
// LAN address before moving on. LAN RTTs are sub-millisecond; a candidate that
// doesn't answer quickly is almost certainly unreachable (wrong subnet,
// firewall) and not worth blocking on.
const dialTimeout = 2 * time.Second

// retryInterval is how often the run loop re-attempts peers whose connection
// failed or closed, mirroring the webrtc transport's retry ticker.
const retryInterval = 15 * time.Second

// manager is the per-[magicsock.Conn] TCP LAN transport. It implements
// [magicsock.Transport].
type manager struct {
	logf logger.Logf
	host magicsock.TransportHost

	// ln is the shared TCP listener all peers dial into. localPort is its port,
	// advertised in offers. Both are set once in newManager.
	ln        net.Listener
	localPort int

	// listenerCtx is canceled on Close to stop the accept loop and all peer
	// read loops.
	listenerCtx    context.Context
	listenerCancel context.CancelFunc

	mu           sync.Mutex
	peersByDisco map[key.DiscoPublic]*peerConn // active or in-progress, keyed by remote disco
	closed       bool

	// startCh feeds the run loop peers that want a connection attempt, off the
	// hot send path.
	startCh chan magicsock.TransportPeer
	closeCh chan struct{}
	stopCh  chan struct{} // closed when the run loop has returned
	once    sync.Once
}

// newManager is the [magicsock.RegisterTransport] constructor. It opens the
// shared TCP listener, starts the accept and run loops, and returns the manager
// as a [magicsock.Transport]. On failure it returns an untyped nil interface
// (not a typed nil *manager) so magicsock's nil check behaves correctly.
func newManager(host magicsock.TransportHost) magicsock.Transport {
	// Listen on all interfaces, kernel-chosen port. We advertise only the
	// specific LAN addresses we enumerate, but a single wildcard socket serves
	// them all.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		host.Logf("tcplan: failed to open listener: %v", err)
		return nil
	}
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	m := &manager{
		logf:           host.Logf,
		host:           host,
		ln:             ln,
		localPort:      port,
		listenerCtx:    ctx,
		listenerCancel: cancel,
		peersByDisco:   make(map[key.DiscoPublic]*peerConn),
		startCh:        make(chan magicsock.TransportPeer, 16),
		closeCh:        make(chan struct{}),
		stopCh:         make(chan struct{}),
	}
	m.logf("tcplan: listening on :%d", port)
	go m.acceptLoop()
	go m.runLoop()
	return m
}

// Close implements [magicsock.Transport]. It is idempotent.
func (m *manager) Close() error {
	m.once.Do(func() {
		close(m.closeCh)
		m.listenerCancel()
		m.ln.Close()

		select {
		case <-m.stopCh:
		case <-time.After(2 * time.Second):
			m.logf("tcplan: close timed out waiting for run loop")
		}

		m.mu.Lock()
		m.closed = true
		for _, pc := range m.peersByDisco {
			pc.close()
		}
		m.peersByDisco = nil
		m.mu.Unlock()
	})
	return nil
}

// runLoop serializes connection starts and periodically retries peers whose
// connection has dropped, so a transient LAN hiccup doesn't strand a peer on
// DERP for the rest of the session.
func (m *manager) runLoop() {
	defer close(m.stopCh)
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case p := <-m.startCh:
			m.handleStart(p)
		case <-ticker.C:
			m.retryIdlePeers()
		case <-m.closeCh:
			return
		}
	}
}

// EnsureConnecting implements [magicsock.Transport]. It queues a connection
// attempt to p if we don't already have a live stream. Safe on the hot path.
func (m *manager) EnsureConnecting(p magicsock.TransportPeer) {
	if m.host.Disabled() {
		return
	}
	// A browser peer has no LAN TCP listener; never attempt it. This is the
	// mirror image of the webrtc transport, which only attempts js peers.
	if p.IsJS() {
		return
	}
	dk, ok := p.DiscoKey()
	if !ok {
		return
	}
	m.mu.Lock()
	pc, exists := m.peersByDisco[dk]
	connecting := exists && pc.isLive()
	m.mu.Unlock()
	if connecting {
		return
	}
	select {
	case m.startCh <- p:
	case <-m.closeCh:
	default:
		// Run loop busy; the retry ticker will pick this peer up.
	}
}

// retryIdlePeers re-queues peers whose connection has closed.
func (m *manager) retryIdlePeers() {
	m.mu.Lock()
	dead := make([]key.DiscoPublic, 0)
	for dk, pc := range m.peersByDisco {
		if !pc.isLive() {
			dead = append(dead, dk)
		}
	}
	for _, dk := range dead {
		delete(m.peersByDisco, dk)
	}
	m.mu.Unlock()
	// The next send that still routes over DERP will call EnsureConnecting
	// again; nothing more to do here beyond pruning.
}

// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package tcplan

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"testing"

	"tailscale.com/types/key"
	"tailscale.com/wgengine/magicsock"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("bad test addr %q: %v", s, err)
	}
	return a
}

// fakeHost is a test magicsock.TransportHost that records delivered packets.
type fakeHost struct {
	delivered [][]byte
}

func (f *fakeHost) LocalDiscoKey() key.DiscoPublic                               { return key.DiscoPublic{} }
func (f *fakeHost) PeerForDisco(key.DiscoPublic) (magicsock.TransportPeer, bool) { return nil, false }
func (f *fakeHost) SendSignal(key.DiscoPublic, []byte) error                     { return nil }
func (f *fakeHost) SetChannelReady(magicsock.TransportPeer, bool)                {}
func (f *fakeHost) Disabled() bool                                               { return false }
func (f *fakeHost) Logf(string, ...any)                                          {}
func (f *fakeHost) DeliverPacket(b []byte, _ key.NodePublic) {
	f.delivered = append(f.delivered, append([]byte(nil), b...))
}

// encodeBatch builds the multi-packet wire framing shared with magicsock's
// sendTransportBatch: [magicsock.TransportBatchMagic][2-byte BE len][pkt]...
func encodeBatch(pkts ...[]byte) []byte {
	out := []byte{magicsock.TransportBatchMagic}
	for _, p := range pkts {
		out = binary.BigEndian.AppendUint16(out, uint16(len(p)))
		out = append(out, p...)
	}
	return out
}

func TestDeliverFrame(t *testing.T) {
	pktA := []byte("alpha")
	pktB := []byte("bravo")

	tests := []struct {
		name string
		in   []byte
		want [][]byte
	}{
		{"empty", nil, nil},
		{"single_unframed", pktA, [][]byte{pktA}},
		{"single_element_batch", encodeBatch(pktA), [][]byte{pktA}},
		{"multi_element_batch", encodeBatch(pktA, pktB), [][]byte{pktA, pktB}},
		{"declared_len_exceeds_remaining_dropped",
			append([]byte{magicsock.TransportBatchMagic, 0x00, 0xFF}, pktA...), nil},
		{"unframed_starting_0x01", []byte{0x01, 0x02, 0x03}, [][]byte{{0x01, 0x02, 0x03}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fh := &fakeHost{}
			m := &manager{host: fh, logf: func(string, ...any) {}}
			m.deliverFrame(&peerConn{}, tt.in)
			got := fh.delivered
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("deliverFrame(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsLANCandidate(t *testing.T) {
	cases := map[string]bool{
		"192.168.1.5": true,
		"10.0.0.3":    true,
		"172.16.4.1":  true,
		"169.254.1.1": true,  // link-local
		"100.96.0.1":  false, // Tailscale CGNAT
		"8.8.8.8":     false, // public
		"127.0.0.1":   false, // loopback
		"0.0.0.0":     false, // unspecified
	}
	for s, want := range cases {
		if got := isLANCandidate(mustAddr(t, s)); got != want {
			t.Errorf("isLANCandidate(%s) = %v, want %v", s, got, want)
		}
	}
}

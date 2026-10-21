# Building a new transport

A transport is a connectivity path magicsock can send WireGuard packets over
besides direct UDP and DERP. You implement one Go package that registers itself
with magicsock; magicsock has no knowledge of it until something imports it.

Use [`tcplan`](./tcplan) as the reference — it's the smallest complete one.

## The shape

magicsock defines three interfaces in `tailscale.com/wgengine/magicsock`:

- **`Transport`** — what you implement.
- **`TransportHost`** — what magicsock gives you (your handle back into it).
- **`TransportPeer`** — one remote endpoint; opaque, comparable, use as a map key.

You never see magicsock's concrete types; everything flows through these.

## Wire it up

```go
func init() {
    magicsock.RegisterTransport("myname", newManager)
}

func newManager(host magicsock.TransportHost) magicsock.Transport { ... }
```

The name is your identity: it picks your magic-IP slot locally and rides in the
disco signal so the far side dispatches back to the same transport. Don't call
`feature.Register` — magicsock records the shared `pluggable-transports`
capability for you. Return an untyped `nil` (not a typed nil `*manager`) on
construction failure.

By default your path ranks below a direct UDP path (`Direct > you > DERP`).
If your path is expected to beat direct UDP when available — e.g. a LAN
transport on a host with a faster TCP than UDP stack — register with
`magicsock.WithPriorityOverDirect()` (`you > Direct > DERP`). Either way,
magicsock only promotes your path on disco pong evidence and falls back on its
own; the option just sets where you sit in the ranking.

## What to implement (`Transport`)

- `EnsureConnecting(peer)` — called from the hot send path whenever a peer's
  traffic is still going over DERP. Decide if the peer is eligible, then kick
  off a connection attempt **without blocking** (queue to a run loop). This is
  where you gate on `peer.IsJS()`, reachability, etc.
- `HandleSignal(fromDisco, payload)` — your inbound signaling. `payload` is
  whatever your send side handed to `host.SendSignal`; magicsock doesn't touch
  it. Parse it, continue your handshake.
- `SendPacket(dst, b)` — write one packet to the connected peer. `b` may be a
  single WireGuard packet or a batch (see framing below) — either way, send it
  as one unit; don't inspect it.
- `GetRemoteAddr(dst)` — the real remote `host:port` for status display, or `""`.
- `Close()` — tear everything down. Make it idempotent.

## What the host gives you (`TransportHost`)

- `SendSignal(dst, payload)` — send opaque signaling bytes to a peer over DERP.
- `DeliverPacket(b, srcNodeKey)` — hand a received WireGuard packet up to
  magicsock. This is the receive counterpart of `SendPacket`.
- `SetChannelReady(peer, ready)` — tell magicsock your path to `peer` is up (or
  gone). While ready, magicsock disco-pings your path and promotes it over DERP
  **if the pongs confirm it**. You assert the channel exists; magicsock decides
  whether it's actually the best path. Call `false` when it drops.
- `PeerForDisco(discoKey) -> peer` — resolve an inbound signal/connection back
  to a `TransportPeer`.
- `LocalDiscoKey()` — your own disco key (handy for glare tiebreaks).
- `Disabled()` — honor it: return early from `EnsureConnecting`/`HandleSignal`
  so traffic stays on DERP.
- `Logf(...)`.

## Framing (match the others)

`SendPacket`'s `b` can be a batch, framed as
`[magicsock.TransportBatchMagic][2-byte BE len][pkt]...`. On receive, de-frame
and call `host.DeliverPacket` once per packet; a non-magic first byte is a
single unframed packet. Copy `tcplan`'s `deliverFrame` (or webrtc's
`deliverMsg`) verbatim — it's the shared contract with magicsock's send side. If
your medium is a byte stream (no message boundaries), add your own length
delimiter around each `SendPacket` call on top of this.

## How to think about it (rough progression)

1. **Transport, not protocol.** magicsock keeps doing path selection, disco,
   liveness. You only move bytes and negotiate how to reach a peer.
2. **Eligibility first.** When should this path even be tried? webrtc: at least
   one side is a browser. tcplan: both sides native, same LAN. Encode that in
   `EnsureConnecting` so you don't churn on peers you can't serve.
3. **Signaling is yours.** magicsock gives you a reliable DERP pipe
   (`SendSignal`/`HandleSignal`) carrying opaque bytes. Design the smallest
   handshake that gets a connection up; put the kind/format inside your payload.
4. **Readiness, not selection.** Your job ends at `SetChannelReady(peer, true)`.
   Never force your path — magicsock promotes it only on pong evidence and falls
   back automatically. Be honest about `false`.
5. **Glare.** If both sides can initiate, pick one deterministically, e.g.
   `host.LocalDiscoKey().Compare(remote)`.
6. **Keep deps yours.** Your module's `go.mod` carries your third-party deps;
   importers who don't use your transport never pull them in.

## Minimum vs. optional

- **Must:** `init` + `RegisterTransport`, all `Transport` methods, batch
  de-framing on receive, `SetChannelReady` on up/down, honor `Disabled()`.
- **Optional:** retry loop for dropped connections, platform build tags, a
  meaningful `GetRemoteAddr`, your own debug env knobs.

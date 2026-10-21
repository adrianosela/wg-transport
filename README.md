# wg-transport

Pluggable [transports](https://pkg.go.dev/tailscale.com/wgengine/magicsock#RegisterTransport)
for Tailscale's `magicsock` — alternative connectivity paths that carry
WireGuard traffic between peers alongside direct UDP and DERP.

Each transport is its own package under this module and registers itself with
`magicsock` via `magicsock.RegisterTransport` when imported. `magicsock` itself
has **no** compile-time dependency on any of them (or on their third-party
deps): a path exists only when a program imports that transport's package.

## Transports

| Module | Import | What it is |
| --- | --- | --- |
| [`webrtc`](./webrtc) | `github.com/adrianosela/wg-transport/webrtc` | WebRTC data channel over ICE; disco/DERP signaling, no external server. For peers where at least one side is a browser/Wasm (`GOOS=js`) node with no UDP socket. |
| [`tcplan`](./tcplan) | `github.com/adrianosela/wg-transport/tcplan` | Direct TCP stream over the LAN; advertises local interface addresses, peer dials them. For native same-LAN peers on OSes with a faster TCP than UDP path (e.g. macOS-to-macOS on 10 GbE). |

<!-- Future transports (e.g. ble) slot in as sibling modules here. -->

## Use

Blank-import the transport(s) you want anywhere in your program (each `init`
does the registration); everything else is ordinary Tailscale/tsnet code. Any
number can be imported and all are active at once: each gets its own path, and
magicsock ranks them per-peer alongside direct UDP and DERP, picking the best
one a given peer can actually use.

```go
import (
	_ "github.com/adrianosela/wg-transport/tcplan"
	_ "github.com/adrianosela/wg-transport/webrtc"
)
```

See [`_examples_/`](./_examples_) for complete `tsnet` hello-world HTTP servers,
one per transport. Each also exposes the node's LocalAPI on a unix socket (so
the stock `tailscale` CLI can inspect it) and a loopback `/dial` endpoint to
generate traffic to a peer on demand — enough to watch a transport path get
discovered and promoted. Run a node on two machines:

```sh
cd _examples_/tcplan
TS_AUTHKEY=tskey-auth-... go run . -hostname=tcplan-a   # machine A
TS_AUTHKEY=tskey-auth-... go run . -hostname=tcplan-b   # machine B

# from machine A: make it talk to B, then watch which path magicsock picks
curl 'localhost:8088/dial?peer=tcplan-b'
tailscale --socket=./tailscaled.sock status             # peer line shows "tcplan <addr>" once promoted
```

## Status

The generic pluggable-transport API in `magicsock`
(`RegisterTransport`, `Transport`, `TransportHost`, `TransportPeer`) is not yet
in a tagged `tailscale.com` release, so `go.mod` pins `tailscale.com` to the
commit that introduced it. Update that pin once the API lands upstream.

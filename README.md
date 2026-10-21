# wg-transport

Pluggable [transports](https://pkg.go.dev/tailscale.com/wgengine/magicsock#RegisterTransport)
for Tailscale's `magicsock` — alternative connectivity paths that carry
WireGuard traffic between peers alongside direct UDP and DERP.

Each transport is its own package under this module and registers itself with
`magicsock` via `magicsock.RegisterTransport` when imported. `magicsock` itself
has **no** compile-time dependency on any of them (or on their third-party
deps): a path exists only when a program imports that transport's package.

## Transports

| Package | Import | Status |
| --- | --- | --- |
| [`webrtc`](./webrtc) | `github.com/adrianosela/wg-transport/webrtc` | WebRTC data channel; disco/DERP signaling, no external server. Most useful for browser/Wasm (`GOOS=js`) peers. |

<!-- Future transports (e.g. ble) slot in as sibling packages here. -->

## Use

Blank-import the transport(s) you want anywhere in your program (each `init`
does the registration); everything else is ordinary Tailscale/tsnet code:

```go
import _ "github.com/adrianosela/wg-transport/webrtc"
```

See [`_examples_/webrtc/`](./_examples_/webrtc) for a complete `tsnet`
hello-world HTTP server with the WebRTC transport linked in:

```sh
cd _examples_/webrtc
TS_AUTHKEY=tskey-auth-... go run . -hostname=wg-transport-hello
# then visit http://wg-transport-hello/ from another node on your tailnet
```

## Status

The generic pluggable-transport API in `magicsock`
(`RegisterTransport`, `Transport`, `TransportHost`, `TransportPeer`) is not yet
in a tagged `tailscale.com` release, so `go.mod` pins `tailscale.com` to the
commit that introduced it. Update that pin once the API lands upstream.

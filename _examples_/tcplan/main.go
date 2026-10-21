// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Command tcplan-example is a minimal tsnet application that serves a
// hello-world HTTP page, with the TCP LAN pluggable transport linked in.
//
// The only thing that wires the TCP LAN path into magicsock is the blank import
// of github.com/adrianosela/wg-transport/tcplan below: its init calls
// magicsock.RegisterTransport, so every tsnet.Server (and its magicsock Conn)
// created afterward can carry WireGuard traffic to a same-LAN peer over a
// direct TCP stream in addition to direct UDP and DERP. Remove the blank import
// and the exact same program runs without it.
//
// Run it on two native nodes on the same LAN:
//
//	TS_AUTHKEY=tskey-auth-... go run . -hostname=tcplan-a   # on node A
//	TS_AUTHKEY=tskey-auth-... go run . -hostname=tcplan-b   # on node B
//
// Then from node A, generate traffic to node B so magicsock discovers and
// promotes the transport path, and watch it with the Tailscale CLI:
//
//	curl 'localhost:8088/dial?peer=tcplan-b'               # trigger connectivity
//	tailscale --socket=./tailscaled.sock status            # watch the path
//
// The status line for the peer shows which path is in use; once the TCP LAN
// transport is promoted it reads "tcplan <addr>" instead of "direct"/"relay".
package main

import (
	"context"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/safesocket"
	"tailscale.com/tsnet"

	// Registering the TCP LAN transport is a side effect of importing the
	// package; nothing else in this program references it directly.
	_ "github.com/adrianosela/wg-transport/tcplan"
)

var (
	addr       = flag.String("addr", ":80", "address to serve the hello-world page on (over the tailnet)")
	hostname   = flag.String("hostname", "tcplan-hello", "hostname to use on the tailnet")
	socketPath = flag.String("tsnet-socket", "./tailscaled.sock", "unix socket to expose this node's LocalAPI on, for `tailscale --socket=... status`")
	adminAddr  = flag.String("admin", "127.0.0.1:8088", "loopback address for the debug control endpoint (/dial)")
)

func main() {
	flag.Parse()

	s := &tsnet.Server{
		Hostname: *hostname,
	}
	defer s.Close()

	ln, err := s.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	lc, err := s.LocalClient()
	if err != nil {
		log.Fatal(err)
	}

	// Expose the LocalAPI on a real unix socket so the stock `tailscale` CLI can
	// talk to this node: `tailscale --socket=<path> status`.
	if *socketPath != "" {
		if err := serveLocalAPIProxy(lc, *socketPath); err != nil {
			log.Fatalf("localapi proxy: %v", err)
		}
		defer os.Remove(*socketPath)
		log.Printf("LocalAPI on %s — run: tailscale --socket=%s status", *socketPath, *socketPath)
	}

	// Loopback control endpoint to trigger connectivity to a peer on demand.
	if *adminAddr != "" {
		go serveAdmin(s, *adminAddr)
		log.Printf("debug control on http://%s — trigger with: curl 'http://%s/dial?peer=<peer-hostname>'", *adminAddr, *adminAddr)
	}

	log.Printf("serving hello-world on http://%s%s (TCP LAN transport linked in)", *hostname, *addr)
	log.Fatal(http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><h1>Hello, world!</h1>\n")
		fmt.Fprintf(w, "<p>You are <b>%s</b> from <b>%s</b> (%s)</p>\n",
			html.EscapeString(who.UserProfile.LoginName),
			html.EscapeString(firstLabel(who.Node.ComputedName)),
			html.EscapeString(r.RemoteAddr))
		fmt.Fprintf(w, "</body></html>\n")
	})))
}

// serveLocalAPIProxy listens on a unix socket and splices each connection to
// this node's in-memory LocalAPI (reached via the LocalClient's dialer). The
// stock `tailscale` CLI speaks plain HTTP over this socket, so pointing it at
// the socket with --socket lets `tailscale status`, `netcheck`, etc. inspect
// this tsnet node. It returns once the listener is up; accepting runs in the
// background.
func serveLocalAPIProxy(lc *local.Client, socketPath string) error {
	// A stale socket from a previous run would make Listen fail with EADDRINUSE.
	os.Remove(socketPath)
	ln, err := safesocket.Listen(socketPath)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed on shutdown
			}
			go proxyToLocalAPI(lc, c)
		}
	}()
	return nil
}

func proxyToLocalAPI(lc *local.Client, down net.Conn) {
	defer down.Close()
	// "local-tailscaled.sock:80" is the fixed address tsnet's in-memory LocalAPI
	// listener answers on; lc.Dial is that in-process dialer.
	up, err := lc.Dial(context.Background(), "tcp", "local-tailscaled.sock:80")
	if err != nil {
		return
	}
	defer up.Close()
	go io.Copy(up, down)
	io.Copy(down, up)
}

// serveAdmin runs a loopback-only HTTP control endpoint. GET /dial?peer=<host>
// makes this node open a connection to that peer's hello-world server over the
// tailnet, which generates WireGuard traffic and so causes magicsock to attempt
// (and, if the pongs confirm, promote) a pluggable-transport path to the peer.
// It's the "send a message to a peer running this same code" trigger.
func serveAdmin(s *tsnet.Server, addr string) {
	mux := http.NewServeMux()
	client := &http.Client{
		Transport: &http.Transport{DialContext: s.Dial},
		Timeout:   10 * time.Second,
	}
	mux.HandleFunc("/dial", func(w http.ResponseWriter, r *http.Request) {
		peer := r.URL.Query().Get("peer")
		if peer == "" {
			http.Error(w, "missing ?peer=<hostname>", http.StatusBadRequest)
			return
		}
		url := "http://" + net.JoinHostPort(peer, "80") + "/"
		resp, err := client.Get(url)
		if err != nil {
			fmt.Fprintf(w, "dial %s: ERROR: %v\n", peer, err)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		fmt.Fprintf(w, "dial %s: %s\n%s\n", peer, resp.Status, body)
	})
	// Loopback only; this is a debug surface, not meant to be exposed.
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("admin control server exited: %v", err)
	}
}

func firstLabel(s string) string {
	s, _, _ = strings.Cut(s, ".")
	return s
}

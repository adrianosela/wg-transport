// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Command example is a minimal tsnet application that serves a hello-world HTTP
// page, with the WebRTC pluggable transport linked in.
//
// The only thing that wires WebRTC into magicsock is the blank import of
// github.com/adrianosela/wg-transport/webrtc below: its init calls
// magicsock.RegisterTransport, so every tsnet.Server (and its magicsock Conn)
// created afterward can negotiate a WebRTC data-channel path to peers in
// addition to direct UDP and DERP. Remove the blank import and the exact same
// program runs without WebRTC — that is the whole point of a pluggable
// transport.
//
// Run it like any other tsnet app:
//
//	TS_AUTHKEY=tskey-auth-... go run . -hostname=webrtc-hello
//
// then visit http://webrtc-hello/ from another node on your tailnet.
package main

import (
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"

	"tailscale.com/tsnet"

	// Registering the WebRTC transport is a side effect of importing the
	// package; nothing else in this program references it directly.
	_ "github.com/adrianosela/wg-transport/webrtc"
)

var (
	addr     = flag.String("addr", ":80", "address to listen on")
	hostname = flag.String("hostname", "webrtc-hello", "hostname to use on the tailnet")
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

	log.Printf("serving hello-world on http://%s%s (WebRTC transport linked in)", *hostname, *addr)
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

func firstLabel(s string) string {
	s, _, _ = strings.Cut(s, ".")
	return s
}

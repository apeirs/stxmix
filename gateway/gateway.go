// Package gateway is a local Stacks API endpoint for wallets and tools. Point
// Leather ("Stacks API URL" of a custom network), stacks.js
// (createNetwork({ client: { baseUrl } })), Clarinet or curl at
//
//	http://127.0.0.1:3999            the default network (mainnet)
//	http://127.0.0.1:3999/testnet    testnet (also /mainnet, and any --add network)
//
// and every request (/extended/..., /v2/..., /v3/..., POST /v2/transactions)
// is forwarded to a public Stacks API through the Nym mixnet.
package gateway

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/apeirs/stxmix/stacks"
)

// Gateway serves the routes above.
type Gateway struct {
	Client  *stacks.Client
	Nets    *stacks.Registry
	Default string   // network served at the root ("" = none)
	Hosts   []string // extra accepted Host values
	Origins []string // extra accepted Origin values ("*" = any web page)
	Timeout time.Duration
	Log     func(string, ...any)

	Total, Failed, WS atomic.Int64
}

func (g *Gateway) logf(f string, a ...any) {
	if g.Log != nil {
		g.Log(f, a...)
	}
}

func (g *Gateway) hostOK(h string) bool {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	for _, x := range g.Hosts {
		if x == h {
			return true
		}
	}
	return false
}

// originOK: no Origin (CLI tools, node scripts, Clarinet), a browser
// extension (Leather, Xverse), or an origin the user allowed. Any other web
// page is refused, so a site you visit can't use your gateway.
func (g *Gateway) originOK(o string) bool {
	if o == "" {
		return true
	}
	for _, p := range []string{"chrome-extension://", "moz-extension://", "safari-web-extension://"} {
		if strings.HasPrefix(o, p) {
			return true
		}
	}
	for _, x := range g.Origins {
		if x == "*" || x == o {
			return true
		}
	}
	return false
}

type route struct {
	net *stacks.Network
	sub string // escaped, cleaned path below the upstream base
}

func (g *Gateway) route(escaped string) (route, bool) {
	// An encoded dot segment would be decoded upstream and could step out of
	// an upstream's base path.
	if strings.Contains(strings.ToLower(escaped), "%2e") {
		return route{}, false
	}
	trimmed := strings.TrimPrefix(escaped, "/")
	first, after, _ := strings.Cut(trimmed, "/")
	clean := func(p string) string {
		c := path.Clean("/" + p)
		if strings.HasSuffix(p, "/") && c != "/" {
			c += "/" // socket.io and some API routes care about the trailing slash
		}
		return c
	}
	if n, ok := g.Nets.Get(first); ok && first != "" {
		return route{n, clean(after)}, true
	}
	n, ok := g.Nets.Get(g.Default)
	if !ok || g.Default == "" {
		return route{}, false
	}
	return route{n, clean(trimmed)}, true
}

// bodyType maps the caller's Content-Type to the one sent upstream. Stacks
// clients post JSON (call-read, fee estimates, {"tx": hex}) or raw
// transaction bytes (application/octet-stream).
func bodyType(ct string) (string, bool) {
	if ct == "" {
		return "application/octet-stream", true
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", false
	}
	switch mt {
	case "application/json", "application/octet-stream", "text/plain":
		return mt, true
	}
	return "", false
}

// headerList keeps a CORS request-header list to plain header tokens.
func headerList(s string) string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		h = strings.TrimSpace(h)
		if h != "" && len(h) < 64 && strings.Trim(strings.ToLower(h), "abcdefghijklmnopqrstuvwxyz0123456789-") == "" {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return "content-type"
	}
	return strings.Join(out, ", ")
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// DNS-rebinding guard: a web page must not reach this through a rebound name.
	if !g.hostOK(r.Host) {
		http.Error(w, "bad Host header", http.StatusForbidden)
		return
	}
	origin := r.Header.Get("Origin")
	if !g.originOK(origin) {
		http.Error(w, "origin not allowed (see --allow-origin)", http.StatusForbidden)
		return
	}
	h := w.Header()
	if origin != "" {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
	}
	if r.Method == "OPTIONS" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		// Whatever the caller asks for is allowed: none of it is forwarded.
		h.Set("Access-Control-Allow-Headers", headerList(r.Header.Get("Access-Control-Request-Headers")))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	escaped := r.URL.EscapedPath()
	if escaped == "/" && r.Method == "GET" && r.Header.Get("Upgrade") == "" {
		g.help(w)
		return
	}
	rt, ok := g.route(escaped)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		g.help(w)
		return
	}
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		return
	}
	if len(rt.net.API) == 0 {
		http.Error(w, fmt.Sprintf("network %s has no upstream (add one with --add)", rt.net.Name), http.StatusNotFound)
		return
	}
	label := stacks.Label(rt.sub)
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		g.websocket(w, r, rt, label)
		return
	}
	var body []byte
	var ct string
	if r.Method == "POST" {
		var ok bool
		if ct, ok = bodyType(r.Header.Get("Content-Type")); !ok {
			http.Error(w, "POST body must be application/json, application/octet-stream or text/plain", http.StatusUnsupportedMediaType)
			return
		}
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "could not read the request body", http.StatusBadRequest)
			return
		}
		if body == nil {
			body = []byte{}
		}
	}
	g.Total.Add(1)
	tmo := g.Timeout
	if tmo == 0 {
		tmo = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), tmo)
	defer cancel()
	t0 := time.Now()
	resp, err := g.Client.Do(ctx, rt.net.API, stacks.Request{Method: r.Method, Path: rt.sub, RawQuery: r.URL.RawQuery,
		Body: body, ContentType: ct, Label: r.Method + " " + label})
	if err != nil {
		g.Failed.Add(1)
		g.logf("%s %s %s FAILED after %.1fs: %v", rt.net.Name, r.Method, label, time.Since(t0).Seconds(), err)
		g.fail(w, err)
		return
	}
	defer resp.Body.Close()
	// Only the content type comes back: no Set-Cookie, no Location, no server
	// headers that would tempt the caller to talk to the upstream directly.
	if resp.ContentType != "" {
		h.Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(resp.Status)
	n, _ := io.Copy(w, resp.Body)
	g.logf("%s %s %s -> mixnet -> %s HTTP %d, %d bytes in %.1fs", rt.net.Name, r.Method, label, stacks.HostOf(resp.Upstream), resp.Status, n, time.Since(t0).Seconds())
}

// fail answers 502 in the Stacks API's own error shape.
func (g *Gateway) fail(w http.ResponseWriter, err error) {
	msg := fmt.Sprintf("stxmix: all upstreams failed over the mixnet (no clearnet fallback): %v", err)
	js, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `{"error":%s,"reason":"MixnetUnavailable"}`, js)
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// websocket splices a client websocket (Hiro's /extended/v1/ws, socket.io) to
// the upstream over the mixnet. The handshake is redone upstream with only
// the websocket headers; frames pass through untouched.
func (g *Gateway) websocket(w http.ResponseWriter, r *http.Request, rt route, label string) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != "GET" || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "bad websocket handshake", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket not supported here", http.StatusInternalServerError)
		return
	}
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	up, err := g.Client.Websocket(ctx, rt.net.API, rt.sub, r.URL.RawQuery, key, r.Header.Get("Sec-WebSocket-Protocol"))
	cancel()
	if err != nil {
		g.Failed.Add(1)
		g.logf("%s websocket %s FAILED after %.1fs: %v", rt.net.Name, label, time.Since(t0).Seconds(), err)
		g.fail(w, err)
		return
	}
	defer up.Close()
	sum := sha1.Sum([]byte(key + wsGUID))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if up.Accept != want {
		g.logf("%s websocket: upstream sent a wrong Sec-WebSocket-Accept, closing", rt.net.Name)
		http.Error(w, "upstream websocket handshake invalid", http.StatusBadGateway)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + want + "\r\n"
	if up.Protocol != "" {
		resp += "Sec-WebSocket-Protocol: " + up.Protocol + "\r\n"
	}
	if _, err := io.WriteString(conn, resp+"\r\n"); err != nil {
		return
	}
	g.WS.Add(1)
	g.logf("%s websocket %s -> mixnet -> %s open in %.1fs", rt.net.Name, label, stacks.HostOf(up.Upstream), time.Since(t0).Seconds())
	done := make(chan struct{}, 2)
	go func() { io.Copy(up.Conn, brw.Reader); done <- struct{}{} }()
	go func() { io.Copy(conn, up.R); done <- struct{}{} }()
	<-done
	g.logf("%s websocket closed after %.0fs", rt.net.Name, time.Since(t0).Seconds())
}

func (g *Gateway) help(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "stxmix gateway: the Stacks API over the Nym mixnet.\n")
	if g.Default != "" {
		fmt.Fprintf(w, "  /extended/... /v2/... /v3/...   %s (the default network)\n", g.Default)
	}
	fmt.Fprintf(w, "  /<network>/...                  that network, e.g. /testnet/v2/info\n")
	fmt.Fprintf(w, "networks:\n")
	for _, n := range g.Nets.All() {
		fmt.Fprintf(w, "  %-10s upstreams %d\n", n.Name, len(n.API))
	}
	fmt.Fprintf(w, "requests so far: %d, failed: %d, websockets: %d\n", g.Total.Load(), g.Failed.Load(), g.WS.Load())
}

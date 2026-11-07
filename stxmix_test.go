package stxmix_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apeirs/stxmix/gateway"
	"github.com/apeirs/stxmix/internal/fakes"
	"github.com/apeirs/stxmix/mixnet"
	"github.com/apeirs/stxmix/stacks"
)

const (
	testAddr     = "SP3FBR2AGK5H9QBDH3EEN6DF8EK8JY7RX8QJ5SVTE"
	testContract = "SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4"
	testFn       = "get-balance-available"
	testArg      = "0x0516d1a0a3e1c2b4c6d8e0f1a2b3c4d5e6f708192a3b"
	testTx       = "\x00\x00\x00\x00\x01\x04\x00\xde\xad\xbe\xef\xca\xfe"
	testKey      = "hiro-key-0123456789"
)

// seen is one request as a fake upstream received it.
type seen struct {
	Host, Method, Path, RawQuery, Body string
	Header                             http.Header
}

type world struct {
	socks *fakes.SOCKS
	d     *mixnet.Dialer
	cl    *stacks.Client
	mu    sync.Mutex
	got   []seen
	logs  []string
}

func (w *world) record(r *http.Request) seen {
	b, _ := io.ReadAll(r.Body)
	host, _, _ := net.SplitHostPort(r.Host)
	if host == "" {
		host = r.Host
	}
	s := seen{host, r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()}
	w.mu.Lock()
	w.got = append(w.got, s)
	w.mu.Unlock()
	return s
}

func (w *world) last(host string) seen {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.got) - 1; i >= 0; i-- {
		if w.got[i].Host == host {
			return w.got[i]
		}
	}
	return seen{}
}

func (w *world) count(host string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, s := range w.got {
		if s.Host == host {
			n++
		}
	}
	return n
}

func (w *world) logf(f string, a ...any) {
	w.mu.Lock()
	w.logs = append(w.logs, fmt.Sprintf(f, a...))
	w.mu.Unlock()
}

func (w *world) allLogs() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.logs, "\n")
}

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// The fake world: api.hiro.so is a good Stacks API, bad.hiro.so answers 500,
// busy.hiro.so 429, slow.hiro.so never answers in time, other.test is a good
// non-Hiro API, tn.hiro.so is testnet. All are TLS, reachable only through
// the fake SOCKS proxy, with names known only to the fake DoH resolver.
func newWorld(t *testing.T) *world {
	t.Helper()
	ca := fakes.NewCA()
	w := &world{}
	serve := func(h http.HandlerFunc, name string) string {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.Leaf(name)}, NextProtos: []string{"http/1.1"}})
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		return ln.Addr().String()
	}
	api := func(network int) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				w.record(r)
				conn, brw, err := rw.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAccept(r.Header.Get("Sec-WebSocket-Key")))
				io.Copy(conn, brw) // echo
				return
			}
			s := w.record(r)
			rw.Header().Set("Set-Cookie", "track=1")
			rw.Header().Set("X-Upstream-Secret", "1")
			p := s.Path
			switch {
			case p == "/redirect-plain":
				http.Redirect(rw, r, "http://api.hiro.so/v2/info", http.StatusFound)
				return
			case p == "/redirect-other":
				http.Redirect(rw, r, "https://other.test/v2/info", http.StatusPermanentRedirect)
				return
			case p == "/redirect-hiro":
				http.Redirect(rw, r, "https://api.hiro.so/v2/info", http.StatusPermanentRedirect)
				return
			}
			rw.Header().Set("Location", "https://api.hiro.so/elsewhere")
			rw.Header().Set("Content-Type", "application/json")
			switch {
			case p == "/v2/info":
				fmt.Fprintf(rw, `{"network_id":%d,"stacks_tip_height":123,"burn_block_height":900,"server_version":"fake"}`, network)
			case strings.HasPrefix(p, "/extended/v1/address/"):
				io.WriteString(rw, `{"stx":{"balance":"1500000"},"fungible_tokens":{}}`)
			case p == "/v2/transactions" && r.Method == "POST":
				rw.WriteHeader(400)
				io.WriteString(rw, `{"error":"transaction rejected","reason":"Deserialization","txid":"ab"}`)
			case strings.HasPrefix(p, "/v2/contracts/call-read/") && r.Method == "POST":
				io.WriteString(rw, `{"okay":true,"result":"0x070100000000000000000000000000000840"}`)
			default:
				rw.WriteHeader(404)
				io.WriteString(rw, `{"error":"not found"}`)
			}
		}
	}
	good := serve(api(1), "api.hiro.so")
	other := serve(api(1), "other.test")
	tn := serve(api(2147483648), "tn.hiro.so")
	bad := serve(func(rw http.ResponseWriter, r *http.Request) { w.record(r); http.Error(rw, "boom", 500) }, "bad.hiro.so")
	busy := serve(func(rw http.ResponseWriter, r *http.Request) { w.record(r); http.Error(rw, "slow down", 429) }, "busy.hiro.so")
	slow := serve(func(rw http.ResponseWriter, r *http.Request) {
		w.record(r)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}, "slow.hiro.so")
	doh := fakes.DoH(ca, "1.1.1.1", map[string][]string{
		"api.hiro.so": {"10.0.0.9"}, "other.test": {"10.0.0.10"}, "bad.hiro.so": {"10.0.0.8"},
		"busy.hiro.so": {"10.0.0.7"}, "slow.hiro.so": {"10.0.0.6"}, "tn.hiro.so": {"10.0.0.5"}})
	w.socks = fakes.NewSOCKS(map[string]string{"1.1.1.1:443": doh, "10.0.0.9:443": good, "10.0.0.10:443": other,
		"10.0.0.8:443": bad, "10.0.0.7:443": busy, "10.0.0.6:443": slow, "10.0.0.5:443": tn})
	t.Cleanup(w.socks.Close)
	w.d = &mixnet.Dialer{Proxy: w.socks.Addr, DNS: mixnet.DNSDoH, RootCAs: ca.Pool, Resolvers: []string{"1.1.1.1"}, Timeout: 5 * time.Second}
	w.cl = &stacks.Client{D: w.d, Timeout: 10 * time.Second, Log: w.logf}
	return w
}

func (w *world) registry(t *testing.T) *stacks.Registry {
	reg := stacks.NewRegistry()
	if err := reg.Add("mainnet", []string{"https://bad.hiro.so", "https://api.hiro.so"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("testnet", []string{"https://busy.hiro.so", "https://tn.hiro.so"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("devnet", []string{"https://other.test"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func (w *world) gateway(t *testing.T, def string) *httptest.Server {
	g := &gateway.Gateway{Client: w.cl, Nets: w.registry(t), Default: def, Log: w.logf}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// assertOnlyMixnet: every connection went through the SOCKS proxy, the only
// name lookups were DoH over it, and no hostname was resolved locally or
// handed to the exit (the fake names don't exist anywhere else).
func assertOnlyMixnet(t *testing.T, w *world, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, r := range w.socks.Seen() {
		got[r.Host] = true
		if r.ATYP != 1 {
			t.Errorf("SOCKS CONNECT %v: DoH mode must connect by IPv4 literal", r)
		}
	}
	for _, h := range append(want, "1.1.1.1") {
		if !got[h] {
			t.Errorf("SOCKS never saw %s (saw %v)", h, got)
		}
	}
}

// Hiro API and node RPC requests pass through untouched: the path and query
// string of a GET, and the body of a POST byte for byte with its type. The
// first upstream answers 500, the second one serves.
func TestPassThroughOverMixnet(t *testing.T) {
	w := newWorld(t)
	srv := w.gateway(t, "mainnet")
	resp, body := do(t, "GET", srv.URL+"/extended/v1/address/"+testAddr+"/balances?unanchored=true&until_block=9", nil, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"1500000"`) {
		t.Fatalf("balances: %d %s", resp.StatusCode, body)
	}
	s := w.last("api.hiro.so")
	if s.Path != "/extended/v1/address/"+testAddr+"/balances" || s.RawQuery != "unanchored=true&until_block=9" {
		t.Fatalf("upstream saw %q ? %q", s.Path, s.RawQuery)
	}
	if w.count("bad.hiro.so") == 0 {
		t.Fatal("first upstream was never tried")
	}
	// Broadcast: raw tx bytes, application/octet-stream; the node's 400 comes back as it is.
	resp, body = do(t, "POST", srv.URL+"/v2/transactions", map[string]string{"Content-Type": "application/octet-stream"}, testTx)
	if resp.StatusCode != 400 || !strings.Contains(body, "Deserialization") {
		t.Fatalf("broadcast: %d %s", resp.StatusCode, body)
	}
	if s := w.last("api.hiro.so"); s.Body != testTx || s.Method != "POST" || s.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("upstream got %s %q %q", s.Method, s.Body, s.Header.Get("Content-Type"))
	}
	// call-read, JSON body byte for byte.
	cr := `{"sender":"` + testAddr + `","arguments":["` + testArg + `"]}`
	resp, body = do(t, "POST", srv.URL+"/v2/contracts/call-read/"+testContract+"/sbtc-token/"+testFn, map[string]string{"Content-Type": "application/json; charset=utf-8"}, cr)
	if resp.StatusCode != 200 || !strings.Contains(body, `"okay":true`) {
		t.Fatalf("call-read: %d %s", resp.StatusCode, body)
	}
	if s := w.last("api.hiro.so"); s.Body != cr || s.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("call-read upstream got %q %q", s.Body, s.Header.Get("Content-Type"))
	}
	// An unsupported body type is refused before anything goes out.
	before := len(w.socks.Seen())
	if resp, _ = do(t, "POST", srv.URL+"/v2/transactions", map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, "x"); resp.StatusCode != 415 {
		t.Fatalf("multipart: %d", resp.StatusCode)
	}
	if len(w.socks.Seen()) != before {
		t.Fatal("refused body still opened a stream")
	}
	assertOnlyMixnet(t, w, "10.0.0.8", "10.0.0.9")
}

// /testnet, /mainnet and custom networks on one port, and a second listener
// with testnet at the root.
func TestNetworkRoutes(t *testing.T) {
	w := newWorld(t)
	main := w.gateway(t, "mainnet")
	tnRoot := w.gateway(t, "testnet")
	for _, c := range []struct{ url, host, want string }{
		{main.URL + "/v2/info", "api.hiro.so", `"network_id":1,`},
		{main.URL + "/mainnet/v2/info", "api.hiro.so", `"network_id":1,`},
		{main.URL + "/testnet/v2/info", "tn.hiro.so", `"network_id":2147483648`},
		{tnRoot.URL + "/v2/info", "tn.hiro.so", `"network_id":2147483648`},
		{main.URL + "/devnet/v2/info", "other.test", `"network_id":1,`},
	} {
		resp, body := do(t, "GET", c.url, nil, "")
		if resp.StatusCode != 200 || !strings.Contains(body, c.want) || w.last(c.host).Path != "/v2/info" {
			t.Fatalf("%s: %d %s (upstream %s saw %q)", c.url, resp.StatusCode, body, c.host, w.last(c.host).Path)
		}
	}
	if w.count("busy.hiro.so") == 0 {
		t.Fatal("testnet 429 upstream never tried")
	}
	// A trailing slash is kept (socket.io needs it); /testnet alone is the API root.
	do(t, "GET", main.URL+"/testnet/extended/", nil, "")
	if p := w.last("tn.hiro.so").Path; p != "/extended/" {
		t.Fatalf("trailing slash: %q", p)
	}
	if _, body := do(t, "GET", main.URL+"/", nil, ""); !strings.Contains(body, "/testnet/v2/info") {
		t.Fatalf("help: %s", body)
	}
	n := len(w.got)
	for _, p := range []string{"/v2/%2e%2e/x", "/testnet/%2E%2E/v2/info"} {
		if resp, _ := do(t, "GET", main.URL+p, nil, ""); resp.StatusCode != 404 {
			t.Fatalf("%s: want 404, got %d", p, resp.StatusCode)
		}
	}
	if len(w.got) != n {
		t.Fatalf("an encoded dot segment was forwarded: %v", w.got[n:])
	}
	if resp, _ := do(t, "PUT", main.URL+"/v2/info", nil, "x"); resp.StatusCode != 405 {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
}

// Nothing about the local caller reaches the upstream (not even its own API
// key), and nothing but the content type comes back.
func TestHeaderStripping(t *testing.T) {
	w := newWorld(t)
	srv := w.gateway(t, "mainnet")
	resp, _ := do(t, "GET", srv.URL+"/v2/info", map[string]string{
		"Origin": "chrome-extension://ldinpeekobnhjjdofggfgjlcehhmanlj", "Cookie": "session=1", "Referer": "https://dapp.example/",
		"User-Agent": "Leather/6.0", "X-Forwarded-For": "192.0.2.7", "Authorization": "Bearer x", "Accept-Language": "da-DK",
		"X-Api-Key": "callers-own-key", "X-Hiro-Product": "stacksjs",
	}, "")
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	h := w.last("api.hiro.so").Header
	for _, k := range []string{"Origin", "Cookie", "Referer", "X-Forwarded-For", "Authorization", "Accept-Language", "X-Api-Key", "X-Hiro-Product"} {
		if h.Get(k) != "" {
			t.Errorf("%s leaked upstream: %q", k, h.Get(k))
		}
	}
	if h.Get("User-Agent") != stacks.UserAgent {
		t.Errorf("User-Agent upstream = %q", h.Get("User-Agent"))
	}
	for _, k := range []string{"Set-Cookie", "Location", "X-Upstream-Secret"} {
		if resp.Header.Get(k) != "" {
			t.Errorf("upstream header %s leaked back: %v", k, resp.Header)
		}
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "chrome-extension://ldinpeekobnhjjdofggfgjlcehhmanlj" {
		t.Errorf("extension origin not allowed: %v", resp.Header)
	}
	// CORS preflight from the extension, with stacks.js' own headers.
	resp, _ = do(t, "OPTIONS", srv.URL+"/v2/transactions", map[string]string{"Origin": "chrome-extension://abc",
		"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type, x-hiro-product, bad header"}, "")
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Headers") != "content-type, x-hiro-product" {
		t.Errorf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

// The Hiro API key goes to *.hiro.so only, also across redirects, and never
// into a log line.
func TestAPIKeyOnlyToHiro(t *testing.T) {
	w := newWorld(t)
	w.cl.APIKey = testKey
	ctx := context.Background()
	get := func(base, path string) {
		t.Helper()
		var v map[string]any
		if _, err := w.cl.JSON(ctx, []string{base}, stacks.Request{Path: path}, &v); err != nil {
			t.Fatalf("%s%s: %v", base, path, err)
		}
	}
	get("https://api.hiro.so", "/v2/info")
	if k := w.last("api.hiro.so").Header.Get("X-Api-Key"); k != testKey {
		t.Fatalf("hiro did not get the key: %q", k)
	}
	get("https://other.test", "/v2/info")
	if k := w.last("other.test").Header.Get("X-Api-Key"); k != "" {
		t.Fatalf("non-Hiro upstream got the key")
	}
	// Hiro redirects elsewhere: net/http would copy the header; it must not.
	get("https://api.hiro.so", "/redirect-other")
	if s := w.last("other.test"); s.Path != "/v2/info" || s.Header.Get("X-Api-Key") != "" {
		t.Fatalf("key followed a redirect off Hiro: %q %q", s.Path, s.Header.Get("X-Api-Key"))
	}
	// A non-Hiro name redirecting to Hiro (stacks-node-api.mainnet.stacks.co does): key added there.
	get("https://other.test", "/redirect-hiro")
	if w.last("other.test").Header.Get("X-Api-Key") != "" || w.last("api.hiro.so").Header.Get("X-Api-Key") != testKey {
		t.Fatal("redirect to Hiro: key on the wrong hop")
	}
	// Websocket upgrade too.
	ws, err := w.cl.Websocket(ctx, []string{"https://other.test", "https://api.hiro.so"}, "/extended/v1/ws", "", "dGhlIHNhbXBsZSBub25jZQ==", "")
	if err != nil {
		t.Fatal(err)
	}
	ws.Close()
	if w.last("other.test").Header.Get("X-Api-Key") != "" {
		t.Fatal("websocket to non-Hiro upstream got the key")
	}
	ws, err = w.cl.Websocket(ctx, []string{"https://api.hiro.so"}, "/extended/v1/ws", "", "dGhlIHNhbXBsZSBub25jZQ==", "")
	if err != nil {
		t.Fatal(err)
	}
	ws.Close()
	if w.last("api.hiro.so").Header.Get("X-Api-Key") != testKey {
		t.Fatal("websocket to Hiro lacks the key")
	}
	if strings.Contains(w.allLogs(), testKey) {
		t.Fatalf("key in logs:\n%s", w.allLogs())
	}
}

func TestHostAndOriginGuard(t *testing.T) {
	w := newWorld(t)
	srv := w.gateway(t, "mainnet")
	before := len(w.socks.Seen())
	for _, host := range []string{"evil.example", "evil.example:3999", "127.0.0.1.evil.example"} {
		if resp, _ := do(t, "GET", srv.URL+"/v2/info", map[string]string{"Host": host}, ""); resp.StatusCode != 403 {
			t.Fatalf("rebinding Host %s accepted: %d", host, resp.StatusCode)
		}
	}
	for _, o := range []string{"https://evil.example", "null", "http://127.0.0.1:8080"} {
		if resp, _ := do(t, "POST", srv.URL+"/v2/transactions", map[string]string{"Origin": o, "Content-Type": "application/octet-stream"}, testTx); resp.StatusCode != 403 {
			t.Fatalf("web page origin %s accepted: %d", o, resp.StatusCode)
		}
	}
	if resp, _ := do(t, "OPTIONS", srv.URL+"/v2/transactions", map[string]string{"Origin": "https://evil.example"}, ""); resp.StatusCode != 403 {
		t.Fatalf("preflight from a web page accepted: %d", resp.StatusCode)
	}
	if n := len(w.socks.Seen()); n != before {
		t.Fatalf("refused requests still opened %d mixnet streams", n-before)
	}
	g := &gateway.Gateway{Client: w.cl, Nets: w.registry(t), Default: "mainnet", Origins: []string{"https://wallet.example"}}
	s2 := httptest.NewServer(g)
	defer s2.Close()
	if resp, _ := do(t, "GET", s2.URL+"/v2/info", map[string]string{"Origin": "https://wallet.example"}, ""); resp.StatusCode != 200 {
		t.Fatalf("allowed origin refused: %d", resp.StatusCode)
	}
}

// Mixnet client down: 502 in the Stacks API's error shape, and no direct
// connection is ever attempted.
func TestNoClearnetFallback(t *testing.T) {
	w := newWorld(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	cl := &stacks.Client{D: &mixnet.Dialer{Proxy: dead, Timeout: time.Second}, Timeout: 3 * time.Second}
	srv := httptest.NewServer(&gateway.Gateway{Client: cl, Nets: w.registry(t), Default: "mainnet"})
	defer srv.Close()
	resp, body := do(t, "GET", srv.URL+"/v2/info", nil, "")
	if resp.StatusCode != 502 || !strings.Contains(body, `"reason":"MixnetUnavailable"`) || !strings.Contains(body, "no SOCKS5 proxy") {
		t.Fatalf("want 502 MixnetUnavailable, got %d %s", resp.StatusCode, body)
	}
	resp, _ = do(t, "POST", srv.URL+"/v2/transactions", map[string]string{"Content-Type": "application/octet-stream"}, testTx)
	if resp.StatusCode != 502 {
		t.Fatalf("broadcast: want 502, got %d", resp.StatusCode)
	}
	if len(w.got) != 0 {
		t.Fatalf("an upstream was reached without the mixnet: %v", w.got)
	}
}

// Failure paths log no params either (Go's url.Error embeds the full URL).
func TestFailureLogsCarryNoParams(t *testing.T) {
	w := newWorld(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	cl := &stacks.Client{D: &mixnet.Dialer{Proxy: dead, Timeout: time.Second}, Timeout: 3 * time.Second, Log: w.logf, APIKey: testKey}
	srv := httptest.NewServer(&gateway.Gateway{Client: cl, Nets: w.registry(t), Default: "mainnet", Log: w.logf})
	defer srv.Close()
	resp, body := do(t, "GET", srv.URL+"/extended/v1/address/"+testAddr+"/balances?unanchored=true", nil, "")
	_, body2 := do(t, "POST", srv.URL+"/v2/contracts/call-read/"+testContract+"/sbtc-token/"+testFn, map[string]string{"Content-Type": "application/json"}, `{"arguments":["`+testArg+`"]}`)
	all := w.allLogs()
	for _, secret := range []string{testAddr, testContract, testFn, testArg, "unanchored", testKey} {
		if strings.Contains(all+body+body2, secret) {
			t.Errorf("%q in failure log or answer:\n%s\n%s", secret, all, body)
		}
	}
	if resp.StatusCode != 502 || strings.Count(all, "no SOCKS5 proxy") > 4 {
		t.Fatalf("kept trying upstreams with no proxy (%d):\n%s", resp.StatusCode, all)
	}
}

func TestFailoverAndRedirects(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var v map[string]any
	// 429, then 500, then a good upstream.
	used, err := w.cl.JSON(ctx, []string{"https://busy.hiro.so", "https://bad.hiro.so", "https://api.hiro.so"}, stacks.Request{Path: "/v2/info"}, &v)
	if err != nil || used != "https://api.hiro.so" {
		t.Fatalf("failover: %v %s", err, used)
	}
	// Every upstream failing with HTTP errors: the last answer is surfaced, not hidden.
	status, _, _, err := w.cl.Fetch(ctx, []string{"https://busy.hiro.so", "https://bad.hiro.so"}, stacks.Request{Path: "/v2/info"})
	if err != nil || status != 500 {
		t.Fatalf("all-5xx: status %d err %v", status, err)
	}
	// A timeout moves on to the next upstream.
	fast := &stacks.Client{D: w.d, Timeout: 700 * time.Millisecond, Log: w.logf}
	t0 := time.Now()
	used, err = fast.JSON(ctx, []string{"https://slow.hiro.so", "https://api.hiro.so"}, stacks.Request{Path: "/v2/info"}, &v)
	if err != nil || used != "https://api.hiro.so" || time.Since(t0) > 4*time.Second {
		t.Fatalf("timeout failover: %v %s after %s", err, used, time.Since(t0))
	}
	if !strings.Contains(w.allLogs(), "no answer within") {
		t.Fatalf("timeout not logged:\n%s", w.allLogs())
	}
	// A POST is re-sent with its body to the next upstream.
	status, body, used, err := w.cl.Fetch(ctx, []string{"https://bad.hiro.so", "https://api.hiro.so"}, stacks.Request{Method: "POST", Path: "/v2/transactions", Body: []byte(testTx), ContentType: "application/octet-stream"})
	if err != nil || status != 400 || used != "https://api.hiro.so" || w.last("api.hiro.so").Body != testTx {
		t.Fatalf("POST failover: %d %s %v %q", status, body, err, w.last("api.hiro.so").Body)
	}
	// A redirect is followed over the mixnet if it stays on https (body kept
	// on a 308), refused otherwise.
	if _, err := w.cl.JSON(ctx, []string{"https://api.hiro.so"}, stacks.Request{Path: "/redirect-other"}, &v); err != nil {
		t.Fatalf("https redirect: %v", err)
	}
	if _, err := w.cl.JSON(ctx, []string{"https://api.hiro.so"}, stacks.Request{Path: "/redirect-plain"}, &v); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("plain-http redirect followed: %v", err)
	}
	assertOnlyMixnet(t, w, "10.0.0.6", "10.0.0.7", "10.0.0.8", "10.0.0.9", "10.0.0.10")
}

func TestOnlyHTTPSUpstreams(t *testing.T) {
	w := newWorld(t)
	if _, _, _, err := w.cl.Fetch(context.Background(), []string{"http://api.hiro.so"}, stacks.Request{Path: "/v2/info"}); err == nil {
		t.Fatal("plain http upstream accepted")
	}
	if len(w.socks.Seen()) != 0 {
		t.Fatal("plain http upstream opened a stream")
	}
	reg := stacks.NewRegistry()
	for _, spec := range []string{"x=http://api.hiro.so", "x=https://user:pw@api.hiro.so", "extended=https://api.hiro.so", "x=https://api.hiro.so?key=1", "x=ws://api.hiro.so"} {
		if err := reg.ParseAdd(spec); err == nil {
			t.Errorf("--add %s accepted", spec)
		}
	}
}

// Request params (principals, contract ids, function names, args, tx bytes,
// query strings) never reach a log line on success.
func TestLogsCarryNoParams(t *testing.T) {
	w := newWorld(t)
	w.cl.APIKey = testKey
	srv := w.gateway(t, "mainnet")
	do(t, "GET", srv.URL+"/extended/v1/address/"+testAddr+"/balances?unanchored=true", nil, "")
	do(t, "GET", srv.URL+"/v2/accounts/"+testAddr+"?proof=0", nil, "")
	do(t, "POST", srv.URL+"/v2/contracts/call-read/"+testContract+"/sbtc-token/"+testFn, map[string]string{"Content-Type": "application/json"}, `{"sender":"`+testAddr+`","arguments":["`+testArg+`"]}`)
	do(t, "POST", srv.URL+"/v2/transactions", map[string]string{"Content-Type": "application/octet-stream"}, testTx)
	do(t, "GET", srv.URL+"/extended/v1/tx/0xfeedface01", nil, "")
	all := w.allLogs()
	for _, secret := range []string{testAddr, testContract, "sbtc-token", testFn, testArg, "unanchored", "proof", "feedface", "\xde\xad", testKey} {
		if strings.Contains(all, secret) {
			t.Errorf("log contains %q:\n%s", secret, all)
		}
	}
	for _, want := range []string{"GET /extended/v1/address/*/balances", "POST /v2/contracts/call-read/*/*/*", "POST /v2/transactions", "GET /extended/v1/tx/*"} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q:\n%s", want, all)
		}
	}
}

// Websocket (/extended/v1/ws, socket.io): the handshake is redone upstream
// over the mixnet with only websocket headers, frames are spliced both ways.
func TestWebsocketSplice(t *testing.T) {
	w := newWorld(t)
	srv := w.gateway(t, "mainnet")
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	fmt.Fprintf(conn, "GET /socket.io/?EIO=4&transport=websocket HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nOrigin: chrome-extension://abc\r\nCookie: a=b\r\n\r\n", key)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}
	s := w.last("api.hiro.so")
	if s.Path != "/socket.io/" || s.RawQuery != "EIO=4&transport=websocket" || s.Header.Get("Origin") != "" || s.Header.Get("Cookie") != "" {
		t.Fatalf("upstream websocket request: %s ? %s %v", s.Path, s.RawQuery, s.Header)
	}
	conn.Write([]byte("frame-bytes"))
	buf := make([]byte, 11)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "frame-bytes" {
		t.Fatalf("echo: %q %v", buf, err)
	}
	assertOnlyMixnet(t, w, "10.0.0.8", "10.0.0.9")
}

func TestBuiltinNetworks(t *testing.T) {
	reg := stacks.NewRegistry()
	for _, n := range []string{"mainnet", "testnet"} {
		nw, ok := reg.Get(n)
		if !ok || len(nw.API) == 0 || nw.NetworkID == 0 {
			t.Errorf("network %s: %+v", n, nw)
			continue
		}
		for _, u := range nw.API {
			if err := stacks.CheckURL(u); err != nil {
				t.Errorf("%s: %v", n, err)
			}
		}
	}
	if m, _ := reg.Get("mainnet"); len(m.API) < 3 {
		t.Errorf("mainnet has %d upstreams", len(m.API))
	}
}

// The upstream is a real, directly reachable TLS server on localhost. With
// the mixnet client down, nothing may reach it, in either DNS mode and at any
// layer (dialer or HTTP client): a direct dial would land here and be seen.
func TestNoFallbackEvenWhenDirectWouldWork(t *testing.T) {
	ca := fakes.NewCA()
	var mu sync.Mutex
	hits := 0
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.Leaf("localhost", "127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		io.WriteString(rw, `{"network_id":1,"stacks_tip_height":1}`)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	// Accepting TCP connections without speaking SOCKS would be a different
	// failure; this port has nothing listening at all.
	dl, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := dl.Addr().String()
	dl.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	reg := stacks.NewRegistry()
	if err := reg.Add("mainnet", []string{"https://localhost:" + port, "https://127.0.0.1:" + port}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{mixnet.DNSDoH, mixnet.DNSExit} {
		cl := &stacks.Client{D: &mixnet.Dialer{Proxy: dead, DNS: mode, RootCAs: ca.Pool, Timeout: time.Second}, Timeout: 3 * time.Second}
		gw := httptest.NewServer(&gateway.Gateway{Client: cl, Nets: reg, Default: "mainnet"})
		resp, body := do(t, "GET", gw.URL+"/v2/info", nil, "")
		gw.Close()
		if resp.StatusCode != 502 || !strings.Contains(body, "no SOCKS5 proxy") {
			t.Errorf("dns=%s: want 502, got %d %s", mode, resp.StatusCode, body)
		}
		var v map[string]any
		if _, err := cl.JSON(context.Background(), reg.All()[0].API, stacks.Request{Path: "/v2/info"}, &v); err == nil {
			t.Errorf("dns=%s: CLI path answered without the mixnet", mode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the upstream was reached directly %d times", hits)
	}
}

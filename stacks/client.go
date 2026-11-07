package stacks

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/apeirs/stxmix/mixnet"
)

// UserAgent is sent upstream instead of whatever the local caller used.
const UserAgent = "Mozilla/5.0"

// KeyHeader carries the optional Hiro API key ($STXMIX_HIRO_KEY).
const KeyHeader = "X-Api-Key"

// IsHiro reports whether host (no port) is a Hiro host, the only ones that
// ever get the API key.
func IsHiro(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "hiro.so" || strings.HasSuffix(host, ".hiro.so")
}

// Client sends requests to upstream Stacks APIs, always via the mixnet
// Dialer. There is no clearnet fallback.
type Client struct {
	D       *mixnet.Dialer
	Timeout time.Duration // per upstream attempt, until the response headers arrive
	// APIKey is sent as X-Api-Key, and only to *.hiro.so (also after a
	// redirect). It is never logged. Empty = keyless (Hiro's per-IP limits).
	APIKey string
	Log    func(format string, args ...any)

	once  sync.Once
	http  *http.Client
	Calls atomic.Int64
}

func (c *Client) setKey(h http.Header, host string) {
	if c.APIKey != "" && IsHiro(host) {
		h.Set(KeyHeader, c.APIKey)
	} else {
		h.Del(KeyHeader)
	}
}

func (c *Client) client() *http.Client {
	c.once.Do(func() {
		// Keep-alives on: one mixnet stream + TLS handshake is expensive, reuse it.
		c.http = &http.Client{
			Transport: &http.Transport{
				Proxy:               nil, // never an environment proxy
				DialContext:         c.D.DialContext,
				TLSHandshakeTimeout: 90 * time.Second,
				TLSClientConfig:     &tls.Config{RootCAs: c.D.RootCAs},
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 4,
			},
			// Follow redirects ourselves (over the mixnet) and only to https:
			// a Location header must never reach the caller, who would follow
			// it over the clearnet.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Scheme != "https" {
					return fmt.Errorf("refusing redirect to non-https %s", req.URL.Host)
				}
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				// net/http copies custom headers to any redirect target. The
				// key goes to Hiro hosts only, and is dropped everywhere else.
				c.setKey(req.Header, req.URL.Hostname())
				return nil
			},
		}
	})
	return c.http
}

func (c *Client) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

// Request is one HTTP request relative to an upstream base URL.
type Request struct {
	Method      string // GET or POST
	Path        string // escaped path below the upstream base, e.g. "/v2/info"
	RawQuery    string // passed through untouched
	Body        []byte
	ContentType string
	Label       string // for logs only; must never contain request params
}

// Response is an upstream answer. The caller closes Body.
type Response struct {
	Status      int
	ContentType string
	Body        io.ReadCloser
	Upstream    string
}

// Join appends an escaped path and query to an upstream base URL.
func Join(base, path, rawQuery string) (*url.URL, error) {
	if err := CheckURL(base); err != nil {
		return nil, err
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = rawQuery
	return u, nil
}

// HostOf returns the host of an upstream URL, for logs.
func HostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

// retryable: provider-side trouble worth trying the next upstream for.
func retryable(status int) bool { return status >= 500 || status == 429 }

// Do sends req to the first upstream that answers without a server error.
// 2xx-4xx answers are returned as they are (a node's 400 for a rejected tx is
// an answer). If every upstream answered 5xx/429, the last such answer is
// returned.
func (c *Client) Do(ctx context.Context, urls []string, req Request) (*Response, error) {
	if len(urls) == 0 {
		return nil, errors.New("no upstream URL configured")
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	var errs []error
	var fallback *Response
	for _, base := range urls {
		u, err := Join(base, req.Path, req.RawQuery)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		host := u.Host
		tmo := c.Timeout
		if tmo == 0 {
			tmo = 50 * time.Second
		}
		actx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(tmo, cancel)
		var body io.Reader
		if req.Body != nil {
			body = bytes.NewReader(req.Body)
		}
		hr, _ := http.NewRequestWithContext(actx, req.Method, u.String(), body)
		// Nothing about the local caller is forwarded: no Origin, Referer,
		// cookies, wallet User-Agent or the caller's own API key. Only these
		// headers go upstream.
		hr.Header = http.Header{}
		hr.Header.Set("User-Agent", UserAgent)
		hr.Header.Set("Accept", "application/json, */*;q=0.5")
		if req.Body != nil {
			ct := req.ContentType
			if ct == "" {
				ct = "application/json"
			}
			hr.Header.Set("Content-Type", ct)
		}
		c.setKey(hr.Header, u.Hostname())
		t0 := time.Now()
		resp, err := c.client().Do(hr)
		stopped := timer.Stop()
		if err != nil {
			cancel()
			// *url.Error carries the full URL (addresses, contract ids, txids
			// in the path); keep only the cause so logs and errors never hold
			// params.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			if !stopped {
				err = fmt.Errorf("no answer within %s", tmo)
			}
			c.logf("%-4s %s %s failed after %.2fs: %v", "up", host, req.Label, time.Since(t0).Seconds(), err)
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
			if ctx.Err() != nil || errors.Is(err, mixnet.ErrProxyUnreachable) {
				break // the next upstream would need the same missing proxy
			}
			continue
		}
		if retryable(resp.StatusCode) {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			c.logf("%-4s %s %s answered HTTP %d in %.2fs, trying the next upstream", "up", host, req.Label, resp.StatusCode, time.Since(t0).Seconds())
			errs = append(errs, fmt.Errorf("%s: HTTP %d", host, resp.StatusCode))
			fallback = &Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: io.NopCloser(bytes.NewReader(b)), Upstream: base}
			continue
		}
		c.Calls.Add(1)
		c.logf("%-4s %s %s answered HTTP %d in %.2fs", "up", host, req.Label, resp.StatusCode, time.Since(t0).Seconds())
		return &Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
			Body: cancelBody{resp.Body, cancel}, Upstream: base}, nil
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, errors.Join(errs...)
}

// Fetch is Do with the answer read whole (at most 32 MiB).
func (c *Client) Fetch(ctx context.Context, urls []string, req Request) (int, []byte, string, error) {
	if req.Label == "" {
		req.Label = Label(req.Path)
	}
	resp, err := c.Do(ctx, urls, req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return resp.Status, b, resp.Upstream, err
}

// APIError is a non-2xx answer from a Stacks API.
type APIError struct {
	Status   int
	Upstream string
	Msg      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d from %s: %s", e.Status, HostOf(e.Upstream), e.Msg)
}

// ErrorText extracts the message of a Stacks API / node error body.
func ErrorText(b []byte) string {
	var e struct {
		Error   string `json:"error"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Cause   string `json:"cause"`
	}
	if json.Unmarshal(b, &e) == nil {
		var parts []string
		for _, s := range []string{e.Error, e.Reason, e.Message, e.Cause} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ": ")
		}
	}
	// Plain text (stacks-node answers a rejected broadcast this way).
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "(empty body)"
	}
	if len(t) > 300 {
		t = t[:300] + "..."
	}
	if !utf8.ValidString(t) || strings.ContainsFunc(t, func(r rune) bool { return r < 0x20 && r != '\n' }) {
		return fmt.Sprintf("%.160q", t)
	}
	return t
}

// JSON sends req and decodes a 200 answer into v.
func (c *Client) JSON(ctx context.Context, urls []string, req Request, v any) (string, error) {
	status, b, used, err := c.Fetch(ctx, urls, req)
	if err != nil {
		return used, err
	}
	if status != 200 {
		return used, &APIError{status, used, ErrorText(b)}
	}
	if err := json.Unmarshal(b, v); err != nil {
		return used, fmt.Errorf("unexpected answer from %s: %.120q", HostOf(used), b)
	}
	return used, nil
}

// ---------------------------------------------------------------- log labels

// routeWords are the path segments of the Stacks API and node RPC that are
// route names, never parameters. Every other segment is logged as "*", so an
// address, contract id, function name, txid, height or BNS name never reaches
// a log line.
var routeWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`extended v1 v2 v3 address addresses balances balance stx stx_inbound
		transactions transactions_with_transfers assets nonces mempool tx txs multiple raw events block blocks
		burn_block burn_blocks by_height by_hash by_burn_block_height by_burn_block_hash by_index_block_hash
		contract contracts by_trait call-read interface source map_entry data_var constant_val accounts info
		network fees transfer transaction pox status search names namespaces subdomains name tokens nft ft
		holdings history mints metadata microblock microblocks unanchored stacking pools delegations signers
		cycles signer stackers rosetta ws traits supply total circulating legacy health ping neighbors
		attachments headers sortitions tenures tip fork_info stats fee_rate dropped faucets btc price prices
		smart-contracts smart_contracts read_only principal latest stackerdb chunks replicas metadata_hash
		block_proposal block_simulate tenure_info tenure_blockheight blockbyheight blockbyhash
		socket.io debug broadcast unconfirmed by_address recent principals`) {
		routeWords[w] = true
	}
}

// Label turns an API path into a log-safe route template:
// /extended/v1/address/SP.../balances -> /extended/v1/address/*/balances.
func Label(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	rest := false
	for i, s := range segs {
		if rest || !routeWords[s] {
			segs[i] = "*"
		}
		// Everything after these is a contract id, function, map, variable
		// or name, even when it happens to look like a route word.
		if tailParams[s] && !rest {
			rest = true
		}
	}
	return "/" + strings.Join(segs, "/")
}

var tailParams = map[string]bool{"call-read": true, "map_entry": true, "data_var": true, "constant_val": true,
	"interface": true, "source": true, "traits": true, "names": true, "namespaces": true, "subdomains": true}

// ---------------------------------------------------------------- websocket

// WSConn is an upgraded websocket stream to an upstream API.
type WSConn struct {
	net.Conn
	R        *bufio.Reader // may hold bytes already read past the handshake
	Accept   string        // Sec-WebSocket-Accept from the upstream
	Protocol string
	Upstream string
}

// Websocket opens a websocket (/extended/v1/ws, socket.io) to the first
// upstream that accepts the upgrade, over the mixnet with TLS end to end. key
// is the caller's Sec-WebSocket-Key, so the caller can check the accept value.
func (c *Client) Websocket(ctx context.Context, urls []string, path, rawQuery, key, protocol string) (*WSConn, error) {
	var errs []error
	for _, base := range urls {
		u, err := Join(base, path, rawQuery)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		port := u.Port()
		if port == "" {
			port = "443"
		}
		t0 := time.Now()
		tmo := c.Timeout
		if tmo == 0 {
			tmo = 50 * time.Second
		}
		actx, cancel := context.WithTimeout(ctx, tmo)
		ws, err := c.wsOnce(actx, u, port, key, protocol)
		cancel()
		if err != nil {
			c.logf("%-4s %s websocket failed after %.2fs: %v", "up", u.Host, time.Since(t0).Seconds(), err)
			errs = append(errs, fmt.Errorf("%s: %w", u.Host, err))
			if ctx.Err() != nil || errors.Is(err, mixnet.ErrProxyUnreachable) {
				break
			}
			continue
		}
		ws.Upstream = base
		c.logf("%-4s %s websocket open in %.2fs", "up", u.Host, time.Since(t0).Seconds())
		return ws, nil
	}
	if len(errs) == 0 {
		return nil, errors.New("no upstream URL configured")
	}
	return nil, errors.Join(errs...)
}

func (c *Client) wsOnce(ctx context.Context, u *url.URL, port, key, protocol string) (*WSConn, error) {
	tc, err := c.D.DialTLS(ctx, net.JoinHostPort(u.Hostname(), port), &tls.Config{
		ServerName: u.Hostname(), RootCAs: c.D.RootCAs, NextProtos: []string{"http/1.1"}})
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = tc.SetDeadline(dl)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n", u.RequestURI(), u.Host)
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: %s\r\n", key, UserAgent)
	if protocol != "" {
		fmt.Fprintf(&b, "Sec-WebSocket-Protocol: %s\r\n", protocol)
	}
	if c.APIKey != "" && IsHiro(u.Hostname()) {
		fmt.Fprintf(&b, "%s: %s\r\n", KeyHeader, c.APIKey)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(tc, b.String()); err != nil {
		tc.Close()
		return nil, err
	}
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		tc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		resp.Body.Close()
		tc.Close()
		return nil, fmt.Errorf("upgrade refused: HTTP %d", resp.StatusCode)
	}
	_ = tc.SetDeadline(time.Time{})
	return &WSConn{Conn: tc, R: br, Accept: resp.Header.Get("Sec-WebSocket-Accept"), Protocol: resp.Header.Get("Sec-WebSocket-Protocol")}, nil
}

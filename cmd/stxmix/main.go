// Command stxmix: the Stacks API (Hiro API + node RPC) over the Nym mixnet.
//
//	stxmix nym setup         start nym-socks5-client on a random active exit gateway
//	stxmix serve             local gateway: http://127.0.0.1:3999 (mainnet), /testnet
//	stxmix info|balance|call-read|broadcast   talk to a public API directly over the mixnet
//	stxmix probe             which public upstreams answer Nym exit traffic, and how fast
//	stxmix ip                which IP do upstreams see? (a Nym exit gateway)
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/apeirs/stxmix/gateway"
	"github.com/apeirs/stxmix/mixnet"
	"github.com/apeirs/stxmix/stacks"
)

const version = "0.1.0"

var start = time.Now()

func tracef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%7.2fs] "+format+"\n", append([]any{time.Since(start).Seconds()}, args...)...)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "stxmix:", err)
	os.Exit(1)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error {
	if s = strings.TrimSpace(s); s != "" {
		*m = append(*m, s)
	}
	return nil
}

type common struct {
	proxy, dns, network, apiURL string
	add                         multi
	verbose, jsonOut            bool
	timeout                     time.Duration
	reg                         *stacks.Registry
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.proxy, "proxy", envOr("STXMIX_PROXY", "127.0.0.1:1087"), "nym-socks5-client SOCKS5 address ($STXMIX_PROXY)")
	fs.StringVar(&c.dns, "dns", envOr("STXMIX_DNS", mixnet.DNSDoH), "name resolution: doh (DoH over the mixnet, connect by IPv4) or exit")
	fs.StringVar(&c.network, "network", envOr("STXMIX_NETWORK", "mainnet"), "mainnet, testnet, or one added with --add")
	fs.StringVar(&c.apiURL, "api", "", "use this https API URL instead of the network's list")
	fs.Var(&c.add, "add", "network upstreams: name=https://api[|https://api2] (repeatable; mainnet=... replaces the built-in list)")
	fs.BoolVar(&c.verbose, "v", false, "trace every mixnet step to stderr")
	fs.BoolVar(&c.jsonOut, "json", false, "JSON output")
	fs.DurationVar(&c.timeout, "timeout", 5*time.Minute, "overall timeout for the command")
}

func (c *common) registry() *stacks.Registry {
	if c.reg != nil {
		return c.reg
	}
	c.reg = stacks.NewRegistry()
	for _, a := range c.add {
		if err := c.reg.ParseAdd(a); err != nil {
			die(fmt.Errorf("--add: %w", err))
		}
	}
	return c.reg
}

func (c *common) dialer() *mixnet.Dialer {
	proxy, err := mixnet.ParseProxy(c.proxy)
	if err != nil {
		die(err)
	}
	if c.dns != mixnet.DNSDoH && c.dns != mixnet.DNSExit {
		die(fmt.Errorf("--dns must be doh or exit"))
	}
	d := &mixnet.Dialer{Proxy: proxy, DNS: c.dns}
	if c.verbose {
		d.Trace = tracef
	}
	return d
}

func (c *common) client() *stacks.Client {
	// The key comes only from the environment, never from a flag, so it stays
	// out of shell history and the process list.
	cl := &stacks.Client{D: c.dialer(), APIKey: strings.TrimSpace(os.Getenv("STXMIX_HIRO_KEY"))}
	if c.verbose {
		cl.Log = tracef
	}
	return cl
}

func (c *common) net() *stacks.Network {
	n, ok := c.registry().Get(c.network)
	if !ok {
		if c.apiURL != "" {
			return &stacks.Network{Name: c.network}
		}
		die(fmt.Errorf("unknown network %q (see `stxmix networks`), or pass --api URL", c.network))
	}
	return n
}

func (c *common) urls() []string {
	if c.apiURL != "" {
		u := strings.TrimRight(c.apiURL, "/")
		if err := stacks.CheckURL(u); err != nil {
			die(err)
		}
		return []string{u}
	}
	n := c.net()
	if len(n.API) == 0 {
		die(fmt.Errorf("network %s has no upstream; pass --api URL", n.Name))
	}
	return n.API
}

func (c *common) ctx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	return ctx, func() { stop(); cancel() }
}

func (c *common) out(v any, human string) {
	if c.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}
	fmt.Println(human)
}

// parse allows flags after positional arguments. A lone "-" is positional.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `stxmix `+version+`: the Stacks API (Hiro API + node RPC) through the Nym mixnet

Mixnet client (official nym-socks5-client):
  stxmix nym setup [--binary PATH] [--port 1087]   init on a random active exit gateway, then start
  stxmix nym start | stop | status | exits

Local gateway for wallets and tools (Leather, stacks.js, Clarinet, curl ...):
  stxmix serve [--listen 127.0.0.1:3999] [--default mainnet] [--testnet-listen 127.0.0.1:3998]
               [--add name=https://api|https://api2]
     mainnet  http://127.0.0.1:3999           (/extended/..., /v2/..., /v3/...)
     testnet  http://127.0.0.1:3999/testnet   (or the root of --testnet-listen)

Talk to a public Stacks API directly over the mixnet:
  stxmix info                                   chain tip (Stacks and Bitcoin block)
  stxmix balance PRINCIPAL [--all]              STX balance, nonce and fungible tokens
  stxmix call-read CONTRACT FUNCTION [ARG...]   read-only call; ARG = 0x<clarity hex>, u1, 1, true,
                                                none, 'SP... or "ascii"  [--sender PRINCIPAL]
  stxmix broadcast TX_HEX|FILE|-                forward a signed tx (POST /v2/transactions); holds no keys
  stxmix networks                               built-in networks and upstreams
  stxmix probe [--network all]                  which upstreams work through the mixnet, and how fast
  stxmix ip                                     which IP do upstreams see? (should be a Nym exit gateway)

Common flags: --network mainnet|testnet --api URL --proxy 127.0.0.1:1087 --dns doh|exit -v --json --timeout 5m
Optional Hiro API key for higher rate limits: STXMIX_HIRO_KEY (sent to *.hiro.so only, never logged).
stxmix never holds a private key, and there is no clearnet fallback: if the
mixnet client is down, stxmix refuses to connect.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "nym":
		cmdNym(args)
	case "serve":
		cmdServe(args)
	case "info":
		cmdInfo(args)
	case "balance":
		cmdBalance(args)
	case "call-read":
		cmdCallRead(args)
	case "broadcast":
		cmdBroadcast(args)
	case "networks", "chains":
		cmdNetworks(args)
	case "probe":
		cmdProbe(args)
	case "ip":
		cmdIP(args)
	case "version", "-version", "--version":
		fmt.Println("stxmix", version)
	default:
		usage()
		os.Exit(2)
	}
}

func cmdNetworks(args []string) {
	var c common
	fs := flag.NewFlagSet("networks", flag.ExitOnError)
	c.register(fs)
	parse(fs, args)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NETWORK\tNETWORK ID\tADDRESSES\tGATEWAY PATH\tUPSTREAMS (in order)")
	for _, n := range c.registry().All() {
		id := "?"
		if n.NetworkID != 0 {
			id = fmt.Sprint(n.NetworkID)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t/%s\t%s\n", n.Name, id, n.Prefixes, n.Name, strings.Join(n.API, " "))
	}
	tw.Flush()
	if os.Getenv("STXMIX_HIRO_KEY") != "" {
		fmt.Fprintln(os.Stderr, "Hiro API key: set (STXMIX_HIRO_KEY, sent to *.hiro.so only)")
	}
}

func loopback(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}

func cmdServe(args []string) {
	var c common
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c.register(fs)
	listen := fs.String("listen", "127.0.0.1:3999", "local address: the default network at /, every network at /<name>/")
	tnListen := fs.String("testnet-listen", "", "optional second listener with testnet at the root, e.g. 127.0.0.1:3998")
	def := fs.String("default", "mainnet", "network served at the root (\"\" = none)")
	hosts := fs.String("allow-host", "", "extra Host header values to accept (comma separated)")
	origins := fs.String("allow-origin", "", "web origins allowed to use the gateway, comma separated (\"*\" = any page; browser extensions and non-browser clients are always allowed)")
	parse(fs, args)
	reg := c.registry()
	if *def != "" {
		if _, ok := reg.Get(*def); !ok {
			die(fmt.Errorf("unknown --default network %q", *def))
		}
	}
	cl := c.client()
	mk := func(defNet string) *gateway.Gateway {
		g := &gateway.Gateway{Client: cl, Nets: reg, Default: defNet, Log: logf}
		if *hosts != "" {
			g.Hosts = strings.Split(*hosts, ",")
		}
		if *origins != "" {
			g.Origins = strings.Split(*origins, ",")
		}
		return g
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	key := "none (keyless, Hiro's per-IP limits apply to the exit IP)"
	if cl.APIKey != "" {
		key = "set, sent to *.hiro.so only"
	}
	run := func(addr string, h http.Handler, defNet string) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			die(err)
		}
		if !loopback(addr) {
			fmt.Fprintf(os.Stderr, "WARNING: %s is not bound to loopback; anyone who can reach it can use your mixnet client\n", addr)
		}
		logf("stxmix gateway on http://%s  (root = %s; SOCKS5 %s; Hiro API key: %s)", ln.Addr(), defNet, c.proxy, key)
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
		go func() { <-ctx.Done(); srv.Close() }()
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			die(err)
		}
	}
	if *tnListen != "" {
		go run(*tnListen, mk("testnet"), "testnet")
	}
	run(*listen, mk(*def), *def)
}

func age(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

type nodeInfo struct {
	NetworkID     int64  `json:"network_id"`
	ServerVersion string `json:"server_version"`
	TipHeight     int64  `json:"stacks_tip_height"`
	Tip           string `json:"stacks_tip"`
	BurnHeight    int64  `json:"burn_block_height"`
	TenureHeight  int64  `json:"tenure_height"`
	Synced        *bool  `json:"is_fully_synced"`
}

func cmdInfo(args []string) {
	var c common
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	c.register(fs)
	parse(fs, args)
	ctx, cancel := c.ctx()
	defer cancel()
	cl := c.client()
	urls := c.urls()
	var info nodeInfo
	used, err := cl.JSON(ctx, urls, stacks.Request{Path: "/v2/info"}, &info)
	if err != nil {
		die(err)
	}
	if info.TipHeight == 0 {
		die(fmt.Errorf("unexpected /v2/info answer from %s (no stacks_tip_height)", stacks.HostOf(used)))
	}
	res := map[string]any{"network": c.network, "network_id": info.NetworkID, "stacks_tip_height": info.TipHeight,
		"stacks_tip": info.Tip, "burn_block_height": info.BurnHeight, "tenure_height": info.TenureHeight,
		"server_version": info.ServerVersion, "upstream": used}
	human := fmt.Sprintf("%s (network id %d): Stacks block %d, Bitcoin block %d, tenure %d", c.network, info.NetworkID,
		info.TipHeight, info.BurnHeight, info.TenureHeight)
	// The block time comes from the extended API (Hiro); a bare node has none.
	var blk struct {
		Time string `json:"block_time_iso"`
		Hash string `json:"hash"`
	}
	if _, err := cl.JSON(ctx, []string{used}, stacks.Request{Path: fmt.Sprintf("/extended/v2/blocks/%d", info.TipHeight)}, &blk); err == nil && blk.Time != "" {
		if t, err := time.Parse(time.RFC3339, blk.Time); err == nil {
			human += fmt.Sprintf(" at %s, %s old", t.UTC().Format(time.RFC3339), age(t))
			res["block_time"] = t.UTC()
		}
	}
	human += fmt.Sprintf("\n  tip %s, %s (via %s over the mixnet)", info.Tip, info.ServerVersion, stacks.HostOf(used))
	if n, ok := c.registry().Get(c.network); ok && n.NetworkID != 0 && int64(n.NetworkID) != info.NetworkID {
		human += fmt.Sprintf("\nWARNING: expected network id %d, the upstream reports %d", n.NetworkID, info.NetworkID)
	}
	if info.Synced != nil && !*info.Synced {
		human += "\nWARNING: that node reports it is not fully synced"
	}
	c.out(res, human)
}

func parsePrincipalFor(c *common, s string) stacks.Principal {
	p, err := stacks.ParsePrincipal(s)
	if err != nil {
		die(err)
	}
	if n, ok := c.registry().Get(c.network); ok && !n.AcceptsPrefix(p.Prefix()) {
		die(fmt.Errorf("%s is a %s address, not a %s one; pass --network", p.Prefix(), map[bool]string{true: "testnet", false: "mainnet"}[p.Prefix() == "ST" || p.Prefix() == "SN"], n.Name))
	}
	return p
}

// stx renders micro-STX (a decimal or 0x-hex u128) as STX.
func stx(v string) string {
	n, ok := new(big.Int), false
	if strings.HasPrefix(v, "0x") {
		n, ok = n.SetString(v[2:], 16)
	} else {
		n, ok = n.SetString(v, 10)
	}
	if !ok {
		return v + " µSTX"
	}
	r := new(big.Rat).SetFrac(n, big.NewInt(1_000_000)).FloatString(6)
	r = strings.TrimRight(strings.TrimRight(r, "0"), ".")
	return r + " STX"
}

func cmdBalance(args []string) {
	var c common
	fs := flag.NewFlagSet("balance", flag.ExitOnError)
	c.register(fs)
	all := fs.Bool("all", false, "list every fungible token (default: the first 5)")
	pos := parse(fs, args)
	if len(pos) != 1 {
		die(fmt.Errorf("usage: stxmix balance PRINCIPAL [--network mainnet] [--all]"))
	}
	p := parsePrincipalFor(&c, pos[0])
	ctx, cancel := c.ctx()
	defer cancel()
	cl := c.client()
	var acct struct {
		Balance string `json:"balance"`
		Locked  string `json:"locked"`
		Nonce   int64  `json:"nonce"`
	}
	// Node RPC first: it works on every upstream, Hiro or a bare node.
	used, err := cl.JSON(ctx, c.urls(), stacks.Request{Path: "/v2/accounts/" + url.PathEscape(p.String()), RawQuery: "proof=0"}, &acct)
	if err != nil {
		die(err)
	}
	lines := []string{fmt.Sprintf("  %s (locked %s), nonce %d", stx(acct.Balance), stx(acct.Locked), acct.Nonce)}
	res := map[string]any{"principal": p.String(), "network": c.network, "stx_micro": acct.Balance, "locked_micro": acct.Locked, "nonce": acct.Nonce, "upstream": used}
	var ext struct {
		FT map[string]struct {
			Balance string `json:"balance"`
		} `json:"fungible_tokens"`
	}
	if _, err := cl.JSON(ctx, []string{used}, stacks.Request{Path: "/extended/v1/address/" + url.PathEscape(p.String()) + "/balances"}, &ext); err == nil {
		var ids []string
		for id, b := range ext.FT {
			if b.Balance != "0" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		fts := map[string]string{}
		for i, id := range ids {
			fts[id] = ext.FT[id].Balance
			if i < 5 || *all {
				lines = append(lines, fmt.Sprintf("  %s %s (base units)", ext.FT[id].Balance, id))
			}
		}
		if len(ids) > 5 && !*all {
			lines = append(lines, fmt.Sprintf("  ... and %d more tokens (--all or --json to list them)", len(ids)-5))
		}
		res["fungible_tokens"] = fts
	} else {
		lines = append(lines, "  (no token list: the upstream has no extended API)")
	}
	c.out(res, fmt.Sprintf("%s on %s (via %s over the mixnet):\n%s", p, c.network, stacks.HostOf(used), strings.Join(lines, "\n")))
}

func cmdCallRead(args []string) {
	var c common
	fs := flag.NewFlagSet("call-read", flag.ExitOnError)
	c.register(fs)
	sender := fs.String("sender", "", "tx-sender for the call (default: the contract's deployer)")
	pos := parse(fs, args)
	if len(pos) < 2 {
		die(fmt.Errorf("usage: stxmix call-read CONTRACT_ID FUNCTION [ARG...]   e.g. SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4.sbtc-token get-total-supply"))
	}
	ct := parsePrincipalFor(&c, pos[0])
	if ct.Contract == "" {
		die(fmt.Errorf("%s is not a contract id (want ADDRESS.contract-name)", pos[0]))
	}
	fn := pos[1]
	if !stacks.ValidFunctionName(fn) {
		die(fmt.Errorf("bad function name %q", fn))
	}
	argsHex := []string{}
	for _, a := range pos[2:] {
		h, err := stacks.EncodeArg(a)
		if err != nil {
			die(err)
		}
		argsHex = append(argsHex, h)
	}
	snd := ct.Address()
	if *sender != "" {
		snd = parsePrincipalFor(&c, *sender).String()
	}
	body, _ := json.Marshal(map[string]any{"sender": snd, "arguments": argsHex})
	ctx, cancel := c.ctx()
	defer cancel()
	var r struct {
		Okay   bool   `json:"okay"`
		Result string `json:"result"`
		Cause  string `json:"cause"`
	}
	path := "/v2/contracts/call-read/" + url.PathEscape(ct.Address()) + "/" + url.PathEscape(ct.Contract) + "/" + url.PathEscape(fn)
	used, err := c.client().JSON(ctx, c.urls(), stacks.Request{Method: "POST", Path: path, Body: body, ContentType: "application/json"}, &r)
	if err != nil {
		die(err)
	}
	if !r.Okay {
		c.out(map[string]any{"okay": false, "cause": r.Cause, "upstream": used}, fmt.Sprintf("call failed: %s (via %s over the mixnet)", r.Cause, stacks.HostOf(used)))
		os.Exit(1)
	}
	val, derr := stacks.DecodeHex(r.Result)
	if derr != nil {
		val = r.Result + " (could not decode: " + derr.Error() + ")"
	}
	c.out(map[string]any{"okay": true, "result": r.Result, "value": val, "upstream": used},
		fmt.Sprintf("(%s %s%s) -> %s\n  %s (via %s over the mixnet)", fn, ct, argSuffix(pos[2:]), val, r.Result, stacks.HostOf(used)))
}

func argSuffix(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return " " + strings.Join(a, " ")
}

// chainIDs of serialized transactions (bytes 1..4 after the version byte).
var chainIDs = map[byte]uint32{0x00: 0x00000001, 0x80: 0x80000000}

func cmdBroadcast(args []string) {
	var c common
	fs := flag.NewFlagSet("broadcast", flag.ExitOnError)
	c.register(fs)
	pos := parse(fs, args)
	if len(pos) != 1 {
		die(fmt.Errorf("usage: stxmix broadcast TX_HEX | FILE | -   (a signed, serialized transaction in hex)"))
	}
	raw := pos[0]
	if raw == "-" {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
		raw = string(b)
	} else if st, err := os.Stat(raw); err == nil && !st.IsDir() {
		b, err := os.ReadFile(raw)
		if err != nil {
			die(err)
		}
		raw = string(b)
	}
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "0x")
	tx, err := hex.DecodeString(raw)
	if err != nil || len(tx) < 5 {
		die(fmt.Errorf("not a hex-encoded signed transaction"))
	}
	n := c.net()
	if want, ok := chainIDs[n.TxVersion]; ok && n.NetworkID != 0 && c.apiURL == "" {
		if tx[0] != n.TxVersion || binary.BigEndian.Uint32(tx[1:5]) != want {
			die(fmt.Errorf("this transaction is not for %s (version byte 0x%02x, chain id 0x%08x); pass --network", n.Name, tx[0], binary.BigEndian.Uint32(tx[1:5])))
		}
	}
	ctx, cancel := c.ctx()
	defer cancel()
	status, body, used, err := c.client().Fetch(ctx, c.urls(), stacks.Request{Method: "POST", Path: "/v2/transactions", Body: tx, ContentType: "application/octet-stream"})
	if err != nil {
		die(err)
	}
	if status == 200 {
		var txid string
		if json.Unmarshal(body, &txid) != nil {
			txid = strings.Trim(string(body), "\" \n")
		}
		c.out(map[string]any{"txid": txid, "upstream": used}, fmt.Sprintf("broadcast ok: 0x%s (via %s over the mixnet)", strings.TrimPrefix(txid, "0x"), stacks.HostOf(used)))
		return
	}
	var rej struct {
		Error      string          `json:"error"`
		Reason     string          `json:"reason"`
		ReasonData json.RawMessage `json:"reason_data"`
		Txid       string          `json:"txid"`
	}
	_ = json.Unmarshal(body, &rej)
	v := map[string]any{"status": status, "error": rej.Error, "reason": rej.Reason, "reason_data": rej.ReasonData, "txid": rej.Txid, "upstream": used}
	msg := stacks.ErrorText(body)
	if len(rej.ReasonData) > 0 && string(rej.ReasonData) != "null" {
		msg += " " + string(rej.ReasonData)
	}
	v["message"] = msg
	human := fmt.Sprintf("rejected by the node: HTTP %d %s", status, msg)
	if rej.Txid != "" {
		human += "\ntxid " + rej.Txid
	}
	c.out(v, human+fmt.Sprintf(" (via %s over the mixnet)", stacks.HostOf(used)))
	os.Exit(1)
}

type probeRow struct {
	network, upstream, info, ext string
	infoS, extS                  float64
}

func cmdProbe(args []string) {
	var c common
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	c.register(fs)
	par := fs.Int("parallel", 4, "upstreams probed at once")
	parse(fs, args)
	if !flagSet(fs, "network") && os.Getenv("STXMIX_NETWORK") == "" {
		c.network = "all"
	}
	ctx, cancel := c.ctx()
	defer cancel()
	cl := c.client()
	var nets []*stacks.Network
	switch {
	case c.apiURL != "":
		nets = []*stacks.Network{{Name: "custom", API: []string{strings.TrimRight(c.apiURL, "/")}}}
	case c.network == "all":
		nets = c.registry().All()
	default:
		nets = []*stacks.Network{c.net()}
	}
	type job struct {
		n *stacks.Network
		u string
		i int
	}
	var jobs []job
	for _, n := range nets {
		for _, u := range n.API {
			jobs = append(jobs, job{n, u, len(jobs)})
		}
	}
	rows := make([]probeRow, len(jobs))
	sem := make(chan struct{}, max(1, *par))
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := probeRow{network: j.n.Name, upstream: strings.TrimPrefix(j.u, "https://")}
			t0 := time.Now()
			var info nodeInfo
			if _, err := cl.JSON(ctx, []string{j.u}, stacks.Request{Path: "/v2/info"}, &info); err != nil {
				r.info = short(err)
			} else if info.TipHeight == 0 {
				r.info = "FAIL not a /v2/info answer"
			} else {
				r.info = fmt.Sprintf("tip %d", info.TipHeight)
				if j.n.NetworkID != 0 && info.NetworkID != int64(j.n.NetworkID) {
					r.info += fmt.Sprintf(" WRONG NETWORK %d", info.NetworkID)
				}
			}
			r.infoS = time.Since(t0).Seconds()
			t1 := time.Now()
			var st struct {
				Status   string `json:"status"`
				ChainTip struct {
					Height int64 `json:"block_height"`
				} `json:"chain_tip"`
			}
			if _, err := cl.JSON(ctx, []string{j.u}, stacks.Request{Path: "/extended"}, &st); err != nil {
				r.ext = short(err)
			} else {
				r.ext = fmt.Sprintf("%s, tip %d", st.Status, st.ChainTip.Height)
			}
			r.extS = time.Since(t1).Seconds()
			rows[j.i] = r
		}(j)
	}
	wg.Wait()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NETWORK\tUPSTREAM\tNODE RPC /v2/info\tTIME\tHIRO API /extended\tTIME")
	ok := 0
	for _, r := range rows {
		if strings.HasPrefix(r.info, "tip") && !strings.Contains(r.info, "WRONG") {
			ok++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1fs\t%s\t%.1fs\n", r.network, r.upstream, r.info, r.infoS, r.ext, r.extS)
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "%d of %d upstreams answered /v2/info through the mixnet\n", ok, len(rows))
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func short(err error) string {
	e := strings.ReplaceAll(err.Error(), "\n", "; ")
	if len(e) > 70 {
		e = e[:70]
	}
	return "FAIL " + e
}

func cmdIP(args []string) {
	var c common
	fs := flag.NewFlagSet("ip", flag.ExitOnError)
	c.register(fs)
	noLookup := fs.Bool("no-lookup", false, "don't look the IP up in the nym-api node list")
	parse(fs, args)
	d := c.dialer()
	ctx, cancel := c.ctx()
	defer cancel()
	showIP(ctx, &c, d, *noLookup)
}

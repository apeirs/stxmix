// Package stacks talks to public Stacks API endpoints (the Hiro Stacks
// Blockchain API under /extended and the node RPC under /v2, /v3) through the
// Nym mixnet.
package stacks

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Network is a Stacks network with public, keyless API endpoints (tried in order).
type Network struct {
	Name      string
	NetworkID int    // network_id in /v2/info (1 = mainnet, 2147483648 = testnet)
	TxVersion byte   // first byte of a serialized transaction (0x00 mainnet, 0x80 testnet)
	Prefixes  string // valid address prefixes, e.g. "SP SM"
	API       []string
}

// Networks are the built-in networks. Any other one (a devnet, your own node)
// can be added with --add name=https://host|https://host2.
//
// Every built-in upstream is run by Hiro: there is no other public, keyless
// Stacks API today. stacks-node-api.mainnet.stacks.co answers with a 308 to
// api.mainnet.hiro.so. Failover covers a failing hostname or edge, not Hiro.
var Networks = []Network{
	{"mainnet", 1, 0x00, "SP SM",
		[]string{"https://api.hiro.so", "https://api.mainnet.hiro.so", "https://stacks-node-api.mainnet.stacks.co"}},
	{"testnet", 2147483648, 0x80, "ST SN",
		[]string{"https://api.testnet.hiro.so"}},
}

// reserved names can't be network names: they are first path segments of
// the Stacks API served at the gateway root.
var reserved = map[string]bool{"extended": true, "v1": true, "v2": true, "v3": true, "socket.io": true,
	"rosetta": true, "metadata": true, "status": true, "all": true, "custom": true, "health": true}

// Registry holds the built-in networks plus custom ones.
type Registry struct {
	m map[string]*Network
}

// NewRegistry returns a registry with the built-in networks.
func NewRegistry() *Registry {
	r := &Registry{m: map[string]*Network{}}
	for i := range Networks {
		n := Networks[i]
		n.API = append([]string(nil), n.API...)
		r.m[n.Name] = &n
	}
	return r
}

// ValidName reports whether s can name a network in gateway paths.
func ValidName(s string) error {
	if s == "" || len(s) > 40 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		return fmt.Errorf("network name %q: use lowercase letters, digits, - and _", s)
	}
	if reserved[s] {
		return fmt.Errorf("network name %q is reserved (it is an API route at the gateway root)", s)
	}
	return nil
}

// CheckURL accepts only https URLs without credentials, query or fragment.
func CheckURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s: only https upstreams are allowed", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s: no credentials, query or fragment in upstream URLs", s)
	}
	return nil
}

// Add sets the upstreams of a network, creating it if new. Custom upstreams
// replace the built-in list (so --add mainnet=https://my-node works too).
func (r *Registry) Add(name string, urls []string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := ValidName(name); err != nil {
		return err
	}
	var clean []string
	for _, u := range urls {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			continue
		}
		if err := CheckURL(u); err != nil {
			return err
		}
		clean = append(clean, u)
	}
	if len(clean) == 0 {
		return fmt.Errorf("network %q: no upstream URL", name)
	}
	n, ok := r.m[name]
	if !ok {
		// A custom network is a mainnet-style network unless named after testnet.
		n = &Network{Name: name, Prefixes: "SP SM ST SN"}
		r.m[name] = n
	}
	n.API = clean
	return nil
}

// ParseAdd parses "name=https://a|https://b" into the registry.
func (r *Registry) ParseAdd(spec string) error {
	n, u, ok := strings.Cut(spec, "=")
	if !ok {
		return fmt.Errorf("want name=https://url[|https://url2], got %q", spec)
	}
	return r.Add(n, strings.Split(u, "|"))
}

// Get finds a network by name (case-insensitive).
func (r *Registry) Get(name string) (*Network, bool) {
	n, ok := r.m[strings.ToLower(strings.TrimSpace(name))]
	return n, ok
}

// All returns the networks, built-ins first, then custom ones by name.
func (r *Registry) All() []*Network {
	var out []*Network
	seen := map[string]bool{}
	for _, b := range Networks {
		out = append(out, r.m[b.Name])
		seen[b.Name] = true
	}
	var extra []string
	for n := range r.m {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		out = append(out, r.m[n])
	}
	return out
}

// AcceptsPrefix reports whether a principal with this two-letter prefix
// belongs on the network.
func (n *Network) AcceptsPrefix(p string) bool {
	for _, x := range strings.Fields(n.Prefixes) {
		if x == p {
			return true
		}
	}
	return n.Prefixes == ""
}

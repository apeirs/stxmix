package stacks

import (
	"strings"
	"testing"
)

func TestAddresses(t *testing.T) {
	for _, a := range []string{
		"SP000000000000000000002Q6VF78",             // boot contracts deployer (zero hash160)
		"SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4", // sBTC deployer
		"SP3FBR2AGK5H9QBDH3EEN6DF8EK8JY7RX8QJ5SVTE",
		"ST000000000000000000002AMW42H", // testnet boot address
	} {
		v, h, err := DecodeAddress(a)
		if err != nil {
			t.Errorf("%s: %v", a, err)
			continue
		}
		if got := EncodeAddress(v, h); got != a {
			t.Errorf("round trip %s -> %s", a, got)
		}
	}
	for _, bad := range []string{"SP000000000000000000002Q6VF79", "SP3FBR2AGK5H9QBDH3EEN6DF8EK8JY7RX8QJ5SVTF", "XP000000000000000000002Q6VF78", "SP", "SPU"} {
		if _, _, err := DecodeAddress(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	p, err := ParsePrincipal("'SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4.sbtc-token")
	if err != nil || p.Contract != "sbtc-token" || p.Prefix() != "SM" || p.String() != "SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4.sbtc-token" {
		t.Fatalf("contract principal: %+v %v", p, err)
	}
	for _, bad := range []string{"SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4.", "SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4.1abc", "SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4.a/b"} {
		if _, err := ParsePrincipal(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestClarity(t *testing.T) {
	for in, want := range map[string]string{
		"u100":                           "0x0100000000000000000000000000000064",
		"-1":                             "0x00ffffffffffffffffffffffffffffffff",
		"true":                           "0x03",
		"none":                           "0x09",
		`"hi"`:                           "0x0d000000026869",
		"'SP000000000000000000002Q6VF78": "0x05160000000000000000000000000000000000000000",
	} {
		got, err := EncodeArg(in)
		if err != nil || got != want {
			t.Errorf("EncodeArg(%s) = %s %v, want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"0xzz", "0x99", "u-1", "abc", `"é"`} {
		if _, err := EncodeArg(bad); err == nil {
			t.Errorf("EncodeArg(%q) accepted", bad)
		}
	}
	// (ok u2112) as returned by get-total-supply, a list, a contract principal
	// and a nested tuple.
	for h, want := range map[string]string{
		"0x070100000000000000000000000000000840":                                           "(ok u2112)",
		"0x0b0000000201000000000000000000000000000000010100000000000000000000000000000002": "(list u1 u2)",
		"0x0616" + strings.Repeat("00", 20) + "03706f78":                                   "'SP000000000000000000002Q6VF78.pox",
		"0x0a0c0000000201610200000002010201620804":                                         "(some {a: 0x0102, b: (err false)})",
		"0x0e00000002c3a9": `u"é"`,
	} {
		if got, err := DecodeHex(h); err != nil || got != want {
			t.Errorf("DecodeHex(%s) = %q %v, want %q", h, got, err, want)
		}
	}
	if _, err := DecodeHex("0x0300"); err == nil {
		t.Error("trailing bytes accepted")
	}
	if _, err := DecodeHex("0x0b00ffffff"); err == nil {
		t.Error("truncated list accepted")
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"/extended/v1/address/SP3FBR2AGK5H9QBDH3EEN6DF8EK8JY7RX8QJ5SVTE/balances":                       "/extended/v1/address/*/balances",
		"/v2/contracts/call-read/SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4/sbtc-token/get-total-supply": "/v2/contracts/call-read/*/*/*",
		"/v2/contracts/call-read/SP000000000000000000002Q6VF78/pox/transfer":                            "/v2/contracts/call-read/*/*/*",
		"/v2/transactions":                                               "/v2/transactions",
		"/extended/v1/tx/0x1234abcd":                                     "/extended/v1/tx/*",
		"/extended/v2/blocks/9434124":                                    "/extended/v2/blocks/*",
		"/v2/accounts/SP000000000000000000002Q6VF78":                     "/v2/accounts/*",
		"/v1/names/muneeb.btc":                                           "/v1/names/*",
		"/v2/map_entry/SP000000000000000000002Q6VF78/c/stx":              "/v2/map_entry/*/*/*",
		"/extended/v1/contract/SP000000000000000000002Q6VF78.pox/events": "/extended/v1/contract/*/events",
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNamesAndHiro(t *testing.T) {
	for _, n := range []string{"extended", "v2", "Bad Name", "", "socket.io"} {
		if ValidName(n) == nil {
			t.Errorf("%q accepted as network name", n)
		}
	}
	if ValidName("devnet") != nil {
		t.Error("devnet refused")
	}
	for h, want := range map[string]bool{"api.hiro.so": true, "API.Mainnet.Hiro.so": true, "hiro.so": true,
		"evilhiro.so": false, "api.hiro.so.evil.example": false, "stacks-node-api.mainnet.stacks.co": false} {
		if IsHiro(h) != want {
			t.Errorf("IsHiro(%s) != %v", h, want)
		}
	}
}

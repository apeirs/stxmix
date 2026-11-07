package stacks

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// c32 is Crockford base32, the alphabet of Stacks addresses (c32check).
const c32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Address versions.
const (
	VersionMainnetSingle = 22 // SP
	VersionMainnetMulti  = 20 // SM
	VersionTestnetSingle = 26 // ST
	VersionTestnetMulti  = 21 // SN
)

func c32Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	n := new(big.Int).SetBytes(b)
	var out []byte
	base := big.NewInt(32)
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, c32[mod.Int64()])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return strings.Repeat("0", zeros) + string(out)
}

func c32Decode(s string) ([]byte, error) {
	zeros := 0
	for zeros < len(s) && s[zeros] == '0' {
		zeros++
	}
	n := new(big.Int)
	for i := 0; i < len(s); i++ {
		d := strings.IndexByte(c32, s[i])
		if d < 0 {
			return nil, fmt.Errorf("bad c32 character %q", s[i])
		}
		n.Lsh(n, 5).Or(n, big.NewInt(int64(d)))
	}
	return append(make([]byte, zeros), n.Bytes()...), nil
}

func checksum(version byte, hash160 []byte) []byte {
	a := sha256.Sum256(append([]byte{version}, hash160...))
	b := sha256.Sum256(a[:])
	return b[:4]
}

// EncodeAddress builds a c32check Stacks address from a version and a hash160.
func EncodeAddress(version byte, hash160 []byte) string {
	return "S" + string(c32[version]) + c32Encode(append(append([]byte(nil), hash160...), checksum(version, hash160)...))
}

// DecodeAddress checks a standard Stacks address (checksum included) and
// returns its version and hash160.
func DecodeAddress(addr string) (byte, []byte, error) {
	if len(addr) < 5 || len(addr) > 42 || addr[0] != 'S' {
		return 0, nil, errors.New("not a Stacks address (want S + version + c32check)")
	}
	version := strings.IndexByte(c32, addr[1])
	if version < 0 {
		return 0, nil, errors.New("not a Stacks address (bad version character)")
	}
	data, err := c32Decode(addr[2:])
	if err != nil {
		return 0, nil, fmt.Errorf("not a Stacks address (%v)", err)
	}
	if len(data) != 24 {
		return 0, nil, errors.New("not a Stacks address (bad length)")
	}
	h, sum := data[:20], data[20:]
	if !bytes.Equal(sum, checksum(byte(version), h)) {
		return 0, nil, errors.New("Stacks address checksum mismatch")
	}
	if EncodeAddress(byte(version), h) != addr {
		return 0, nil, errors.New("not a canonical Stacks address")
	}
	return byte(version), h, nil
}

var contractName = regexp.MustCompile(`^[a-zA-Z]([a-zA-Z0-9]|[-_])*$`)

// Principal is a standard principal (an address) or a contract principal
// (address.contract-name).
type Principal struct {
	Version  byte
	Hash160  []byte
	Contract string // "" for a standard principal
}

func (p Principal) Address() string { return EncodeAddress(p.Version, p.Hash160) }

func (p Principal) String() string {
	if p.Contract != "" {
		return p.Address() + "." + p.Contract
	}
	return p.Address()
}

// Prefix is the two-letter address prefix (SP, SM, ST, SN).
func (p Principal) Prefix() string { return p.Address()[:2] }

// ParsePrincipal validates "SP..." or "SP....contract-name". Addresses are
// case-sensitive c32check strings; a lowercase one is upper-cased first.
func ParsePrincipal(s string) (Principal, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "'")
	addr, name, isContract := strings.Cut(s, ".")
	addr = strings.ToUpper(addr)
	v, h, err := DecodeAddress(addr)
	if err != nil {
		return Principal{}, err
	}
	if isContract {
		if len(name) == 0 || len(name) > 128 || !contractName.MatchString(name) {
			return Principal{}, fmt.Errorf("bad contract name %q", name)
		}
	}
	return Principal{Version: v, Hash160: h, Contract: name}, nil
}

// ValidFunctionName: Clarity function names (letters, digits, - _ ! ? + < > = / *).
var functionName = regexp.MustCompile(`^[a-zA-Z]([a-zA-Z0-9]|[-_!?+<>=/*])*$`)

func ValidFunctionName(s string) bool { return len(s) <= 128 && functionName.MatchString(s) }

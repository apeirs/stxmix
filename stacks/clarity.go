package stacks

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Clarity value type tags (consensus serialization).
const (
	tInt       = 0x00
	tUint      = 0x01
	tBuffer    = 0x02
	tTrue      = 0x03
	tFalse     = 0x04
	tStdPrinc  = 0x05
	tContPrinc = 0x06
	tOk        = 0x07
	tErr       = 0x08
	tNone      = 0x09
	tSome      = 0x0a
	tList      = 0x0b
	tTuple     = 0x0c
	tASCII     = 0x0d
	tUTF8      = 0x0e
)

var (
	two128 = new(big.Int).Lsh(big.NewInt(1), 128)
	two127 = new(big.Int).Lsh(big.NewInt(1), 127)
)

func put128(out []byte, n *big.Int) []byte {
	b := make([]byte, 16)
	n.FillBytes(b)
	return append(out, b...)
}

// EncodeArg turns one call-read argument into a hex Clarity value:
//
//	0x0100...           already serialized, passed through
//	u42                 uint
//	42, -7, i42         int
//	true, false, none   bool / none
//	'SP... 'SP....name  principal (the quote is optional)
//	"text"              string-ascii
func EncodeArg(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "0x"):
		b, err := hex.DecodeString(s[2:])
		if err != nil || len(b) == 0 {
			return "", fmt.Errorf("argument %q: bad hex", s)
		}
		if _, _, err := decodeValue(b, 0); err != nil {
			return "", fmt.Errorf("argument %q is not a serialized Clarity value: %v", s, err)
		}
		return s, nil
	case s == "true":
		return "0x03", nil
	case s == "false":
		return "0x04", nil
	case s == "none":
		return "0x09", nil
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		t := s[1 : len(s)-1]
		for i := 0; i < len(t); i++ {
			if t[i] < 0x20 || t[i] > 0x7e {
				return "", fmt.Errorf("argument %q: string-ascii only takes printable ASCII", s)
			}
		}
		b := binary.BigEndian.AppendUint32([]byte{tASCII}, uint32(len(t)))
		return "0x" + hex.EncodeToString(append(b, t...)), nil
	case strings.HasPrefix(s, "u"):
		n, ok := new(big.Int).SetString(s[1:], 10)
		if !ok || n.Sign() < 0 || n.Cmp(two128) >= 0 {
			return "", fmt.Errorf("argument %q: bad uint", s)
		}
		return "0x" + hex.EncodeToString(put128([]byte{tUint}, n)), nil
	case strings.HasPrefix(s, "S") || strings.HasPrefix(s, "'"):
		p, err := ParsePrincipal(s)
		if err != nil {
			return "", fmt.Errorf("argument %q: %v", s, err)
		}
		b := append([]byte{tStdPrinc, p.Version}, p.Hash160...)
		if p.Contract != "" {
			b[0] = tContPrinc
			b = append(append(b, byte(len(p.Contract))), p.Contract...)
		}
		return "0x" + hex.EncodeToString(b), nil
	default:
		n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "i"), 10)
		if !ok || n.Cmp(two127) >= 0 || n.Cmp(new(big.Int).Neg(two127)) < 0 {
			return "", fmt.Errorf("argument %q: use 0x<hex>, u<uint>, <int>, true, false, none, 'principal or \"ascii\"", s)
		}
		v := new(big.Int).Set(n)
		if v.Sign() < 0 {
			v.Add(v, two128)
		}
		return "0x" + hex.EncodeToString(put128([]byte{tInt}, v)), nil
	}
}

// DecodeHex renders a hex-serialized Clarity value in Clarity syntax.
func DecodeHex(h string) (string, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(h, "0x"))
	if err != nil {
		return "", err
	}
	s, n, err := decodeValue(b, 0)
	if err != nil {
		return "", err
	}
	if n != len(b) {
		return "", fmt.Errorf("%d trailing bytes", len(b)-n)
	}
	return s, nil
}

var errShort = errors.New("value is truncated")

func decodeValue(b []byte, depth int) (string, int, error) {
	if depth > 64 {
		return "", 0, errors.New("value nested too deeply")
	}
	if len(b) < 1 {
		return "", 0, errShort
	}
	need := func(n int) error {
		if len(b) < n {
			return errShort
		}
		return nil
	}
	u32 := func(at int) (int, error) {
		if err := need(at + 4); err != nil {
			return 0, err
		}
		v := binary.BigEndian.Uint32(b[at:])
		if v > 1<<24 {
			return 0, errors.New("length too large")
		}
		return int(v), nil
	}
	switch b[0] {
	case tInt, tUint:
		if err := need(17); err != nil {
			return "", 0, err
		}
		n := new(big.Int).SetBytes(b[1:17])
		if b[0] == tUint {
			return "u" + n.String(), 17, nil
		}
		if n.Cmp(two127) >= 0 {
			n.Sub(n, two128)
		}
		return n.String(), 17, nil
	case tBuffer:
		l, err := u32(1)
		if err != nil {
			return "", 0, err
		}
		if err := need(5 + l); err != nil {
			return "", 0, err
		}
		return "0x" + hex.EncodeToString(b[5:5+l]), 5 + l, nil
	case tTrue:
		return "true", 1, nil
	case tFalse:
		return "false", 1, nil
	case tStdPrinc, tContPrinc:
		if err := need(22); err != nil {
			return "", 0, err
		}
		addr := EncodeAddress(b[1], b[2:22])
		if b[0] == tStdPrinc {
			return "'" + addr, 22, nil
		}
		if err := need(23); err != nil {
			return "", 0, err
		}
		l := int(b[22])
		if err := need(23 + l); err != nil {
			return "", 0, err
		}
		return "'" + addr + "." + string(b[23:23+l]), 23 + l, nil
	case tOk, tErr, tSome:
		s, n, err := decodeValue(b[1:], depth+1)
		if err != nil {
			return "", 0, err
		}
		w := map[byte]string{tOk: "ok", tErr: "err", tSome: "some"}[b[0]]
		return "(" + w + " " + s + ")", 1 + n, nil
	case tNone:
		return "none", 1, nil
	case tList:
		l, err := u32(1)
		if err != nil {
			return "", 0, err
		}
		off, items := 5, []string{}
		for i := 0; i < l; i++ {
			s, n, err := decodeValue(b[off:], depth+1)
			if err != nil {
				return "", 0, err
			}
			items = append(items, s)
			off += n
		}
		return "(list " + strings.Join(items, " ") + ")", off, nil
	case tTuple:
		l, err := u32(1)
		if err != nil {
			return "", 0, err
		}
		off, items := 5, []string{}
		for i := 0; i < l; i++ {
			if err := need(off + 1); err != nil {
				return "", 0, err
			}
			nl := int(b[off])
			if err := need(off + 1 + nl); err != nil {
				return "", 0, err
			}
			name := string(b[off+1 : off+1+nl])
			off += 1 + nl
			s, n, err := decodeValue(b[off:], depth+1)
			if err != nil {
				return "", 0, err
			}
			items = append(items, name+": "+s)
			off += n
		}
		return "{" + strings.Join(items, ", ") + "}", off, nil
	case tASCII, tUTF8:
		l, err := u32(1)
		if err != nil {
			return "", 0, err
		}
		if err := need(5 + l); err != nil {
			return "", 0, err
		}
		t := string(b[5 : 5+l])
		if b[0] == tUTF8 {
			if !utf8.ValidString(t) {
				return "", 0, errors.New("bad utf-8 string")
			}
			return "u" + strconv.Quote(t), 5 + l, nil
		}
		return strconv.Quote(t), 5 + l, nil
	}
	return "", 0, fmt.Errorf("unknown Clarity type 0x%02x", b[0])
}

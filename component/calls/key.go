package calls

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeyPrefix starts every spendable key, so a leaked string is recognisable
// (by a person and by a secret scanner) as a key.
const KeyPrefix = "ck_"

// crockford is the Crockford base32 alphabet in lowercase: no i, l, o or u,
// so a key read aloud or copied by hand has no look-alike pairs.
const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

const (
	keyDataSymbols = 26 // 130 bits carrying 128 bits of key
	keySymbols     = keyDataSymbols + 2
	// keyChecksumMod is prime and above 1024/1 so two symbols (10 bits) hold
	// it. A position-weighted sum modulo a prime catches every single-symbol
	// substitution (weights 1..26 are nonzero modulo 1021) and every swap of
	// two adjacent different symbols, which a truncated hash does not
	// guarantee.
	keyChecksumMod = 1021
)

// NewKey reads 16 random bytes from r and returns them with the key's
// canonical spelling. r is a parameter so a test can pin the randomness; a
// short read is an error rather than a weaker key.
func NewKey(r io.Reader) (raw [16]byte, formatted string, err error) {
	if _, err = io.ReadFull(r, raw[:]); err != nil {
		return raw, "", fmt.Errorf("read key randomness: %w", err)
	}
	return raw, FormatKey(raw), nil
}

// keyChecksum is the weighted sum of the data symbol values, 1-based.
func keyChecksum(values [keyDataSymbols]int) int {
	sum := 0
	for i, v := range values {
		sum += v * (i + 1)
	}
	return sum % keyChecksumMod
}

// FormatKey spells raw in its one canonical form: the prefix, 26 symbols of
// the 128 bits in standard base32 bit order (the last symbol is 3 data bits
// then 2 zero bits), and 2 checksum symbols.
func FormatKey(raw [16]byte) string {
	var values [keyDataSymbols]int
	var acc, bits int
	n := 0
	for _, b := range raw {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			values[n] = (acc >> (bits - 5)) & 31
			n++
			bits -= 5
		}
		acc &= (1 << bits) - 1
	}
	if bits > 0 {
		values[n] = (acc << (5 - bits)) & 31
	}
	var sb strings.Builder
	sb.WriteString(KeyPrefix)
	for _, v := range values {
		sb.WriteByte(crockford[v])
	}
	sum := keyChecksum(values)
	sb.WriteByte(crockford[sum>>5])
	sb.WriteByte(crockford[sum&31])
	return sb.String()
}

// ParseKey reads a key as typed: case does not matter and the Crockford
// aliases (i and l for 1, o for 0) are accepted. It refuses anything that is
// not exactly one spelling of a key, naming which rule failed so a person can
// tell a typo from a wrong string.
func ParseKey(s string) ([16]byte, error) {
	var raw [16]byte
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasPrefix(s, KeyPrefix) {
		return raw, errors.New("key has the wrong prefix")
	}
	body := s[len(KeyPrefix):]
	if len(body) != keySymbols {
		return raw, fmt.Errorf("key has the wrong length: want %d symbols after the prefix, got %d", keySymbols, len(body))
	}
	var values [keySymbols]int
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case 'i', 'l':
			c = '1'
		case 'o':
			c = '0'
		}
		v := strings.IndexByte(crockford, c)
		if v < 0 {
			return raw, fmt.Errorf("key has an invalid character at position %d", i+1)
		}
		values[i] = v
	}
	// One canonical spelling per key: the 26th symbol's two padding bits are
	// zero, or two strings would name the same key.
	if values[keyDataSymbols-1]&3 != 0 {
		return raw, errors.New("key has non-zero padding bits")
	}
	var data [keyDataSymbols]int
	copy(data[:], values[:keyDataSymbols])
	sum := keyChecksum(data)
	if values[keyDataSymbols] != sum>>5 || values[keyDataSymbols+1] != sum&31 {
		return raw, errors.New("key has a bad checksum")
	}
	var acc, bits int
	n := 0
	for _, v := range data {
		acc = acc<<5 | v
		bits += 5
		if bits >= 8 {
			if n < len(raw) {
				raw[n] = byte(acc >> (bits - 8))
				n++
			}
			bits -= 8
			acc &= (1 << bits) - 1
		}
	}
	return raw, nil
}

// Digest is the lowercase hex HMAC-SHA256 of secret under pepper. Keys, links
// and session tokens are stored and looked up only by this digest, so a read
// of the store never yields a usable secret.
func Digest(pepper []byte, secret []byte) string {
	m := hmac.New(sha256.New, pepper)
	m.Write(secret)
	return hex.EncodeToString(m.Sum(nil))
}

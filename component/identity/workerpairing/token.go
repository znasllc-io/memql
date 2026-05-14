// Package workerpairing implements short-lived pairing codes for
// the computer-use enrollment flow. CoPresent's Settings -> Computer
// Use card mints a code (XXXX-XXXX, 8 chars from Crockford's Base32)
// and the user pastes it into `memql-cockpit-gui worker pair <code>`
// on their machine. The cockpit redeems the code via a gRPC stream
// authenticated with `Authorization: Pair <code>`, and gets back a
// plain `mql_wkr_<...>` worker token + cluster URL.
//
// The plain code is shown to the user ONCE in the CoPresent UI;
// only its SHA-256 hash lives in the database. Single-use, ~10
// minute TTL.
package workerpairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// codeAlphabet is the 32-char alphabet used for pairing codes.
// Picked to be unambiguous when read aloud or typed: no 0, no 1,
// no I, no O. 24 letters + 8 digits = 32 symbols, 5 bits each.
//
// 8 characters = 40 bits of entropy. With 10-minute TTLs and
// single-use semantics, that's vastly more than required to
// prevent guessing.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

const (
	// CodeRandomChars is the random body length, in alphabet symbols.
	CodeRandomChars = 8
	// DefaultTTL is the lifetime of a freshly-minted code.
	DefaultTTL = 10 * time.Minute
)

// ErrEmptyCode is returned by Hash when the input is empty.
var ErrEmptyCode = errors.New("workerpairing: empty code")

// Mint generates a fresh pairing code. Returns:
//
//	plain  -- the user-facing XXXX-XXXX code. Show this exactly
//	          ONCE; it cannot be recovered.
//	hash   -- the SHA-256 hex digest of plain (with the hyphen
//	          stripped, uppercased -- see Hash).
//
// Returns the underlying crypto/rand error on entropy failure.
func Mint() (plain, hash string, err error) {
	chars := make([]byte, CodeRandomChars)
	idx := make([]byte, CodeRandomChars)
	if _, err := rand.Read(idx); err != nil {
		return "", "", err
	}
	for i, b := range idx {
		chars[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	body := strings.ToUpper(string(chars))
	plain = body[:4] + "-" + body[4:]
	hash = Hash(plain)
	return plain, hash, nil
}

// Hash returns the SHA-256 hex digest of a pairing code. Both
// caller-supplied codes (with hyphens, lowercase, surrounding
// whitespace) and our canonical XXXX-XXXX uppercase form normalize
// to the same hash so users can paste sloppy strings without
// triggering false-negative lookups.
func Hash(plain string) string {
	canonical := Canonicalize(plain)
	if canonical == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// Canonicalize normalizes a user-supplied code: strips whitespace
// + hyphens, uppercases, and re-inserts the hyphen at position 4.
// Empty / wrong-length input returns "" so the caller can refuse
// it without trying to hash garbage.
func Canonicalize(in string) string {
	stripped := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '-' || r == '_' {
			return -1
		}
		return r
	}, strings.TrimSpace(in))
	if len(stripped) != CodeRandomChars {
		return ""
	}
	upper := strings.ToUpper(stripped)
	for _, c := range upper {
		if !strings.ContainsRune(codeAlphabet, c) && !isAcceptedAlias(c) {
			return ""
		}
	}
	return upper[:4] + "-" + upper[4:]
}

// isAcceptedAlias allows a small set of common confusions to
// canonicalize cleanly: "0" -> "O" is rejected because we don't
// have either symbol; same for "1" -> "I". Crockford's spec
// recommends accepting these aliases on input but we'd rather
// fail loud than silently accept ambiguous codes. Returning false
// always keeps us strict.
func isAcceptedAlias(_ rune) bool { return false }

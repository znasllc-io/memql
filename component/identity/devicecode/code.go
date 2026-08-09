// Package devicecode implements the two credentials of the RFC 8628
// device authorization grant (memql#3410).
//
// The grant exists for the case the loopback redirect cannot serve: a
// genuinely headless box, or a network hardened enough that the editor
// on one machine cannot be reached by a browser on another. It is a
// FALLBACK -- it costs a round trip through a second device and a code
// the human has to transcribe -- so the code here is deliberately
// small.
//
// Two credentials, two audiences:
//
//	device_code  mql_dvc_<43 base64url chars>   // 32 random bytes
//	user_code    XXXX-XXXX                      // 8 chars, 40 bits
//
// The device_code is machine-to-machine: it never leaves the polling
// client, so it is sized for a secret (256 bits) and formatted for a
// header rather than for a human. The user_code is the opposite -- it
// is read off one screen and typed into another, so it is short, drawn
// from an alphabet with no character pair a person can confuse, and
// accepted case-insensitively with or without its separator.
//
// Neither plaintext is ever persisted. The device_code is returned once
// in the POST /device/code response; the user_code is displayed by the
// requesting device. Only SHA-256 digests reach the engine.
package devicecode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
)

// DeviceCodePrefix is the literal prefix every device_code plaintext
// carries. It mirrors the mql_pat_ / mql_wkr_ convention so a leaked
// value is recognizable to a secret scanner and to a human reading a
// log line, and so the token endpoint can reject an obviously-wrong
// credential before it hashes anything.
const DeviceCodePrefix = "mql_dvc_"

// deviceCodeRandomBytes is the entropy budget for a device_code. 32
// bytes is the same budget refresh tokens and worker tokens carry; the
// device_code is a bearer credential with a 10-minute window and no
// rate limit tighter than the poll interval, so it is sized as one.
const deviceCodeRandomBytes = 32

// userCodeAlphabet is the 32-symbol alphabet the user_code is drawn
// from. It is the same alphabet component/identity/workerpairing uses,
// and for the same reason: every character pair a person confuses when
// reading a code off a screen is broken by REMOVING one of the pair
// rather than by aliasing them on input.
//
//	no 0 and no O   -- indistinguishable in most sans-serif faces
//	no 1 and no I   -- likewise
//
// 24 letters + 8 digits = 32 symbols, so each character is exactly 5
// bits with no modulo bias off a uniform byte (256 = 8 * 32).
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// UserCodeChars is the user_code body length in alphabet symbols.
// 8 symbols * 5 bits = 40 bits of entropy, which is the floor the
// issue sets. It is rendered as two groups of four (XXXX-XXXX) --
// short enough to hold in working memory while looking from one screen
// to the other, which is the only ergonomic property that matters
// here.
const UserCodeChars = 8

// UserCodeSeparator splits the two rendered groups.
const UserCodeSeparator = "-"

// DefaultTTL is the lifetime of a device authorization. RFC 8628 §3.2
// gives no fixed value; ten minutes matches the pairing-code and
// magic-link windows already in this tree and is long enough for a
// human to pick up a second device without leaving an approvable code
// lying around.
const DefaultTTL = 10 * time.Minute

// DefaultIntervalSeconds is the initial poll floor handed to the
// device in the `interval` field. RFC 8628 §3.2 says a client that
// receives no interval MUST assume 5 seconds; returning the same value
// explicitly keeps compliant and sloppy clients on the same clock.
const DefaultIntervalSeconds = 5

// SlowDownIncrementSeconds is how much a slow_down response raises the
// interval. RFC 8628 §3.5 specifies 5 seconds, and specifies that the
// increase is permanent for the remainder of the flow: a client that
// polls too fast is progressively throttled rather than merely warned.
const SlowDownIncrementSeconds = 5

// MaxIntervalSeconds caps the escalation. Without a ceiling a client
// stuck in a tight retry loop drives its own interval past the row's
// TTL, at which point the throttle has silently become a denial of the
// user's own approval.
const MaxIntervalSeconds = 60

// MintDeviceCode generates a fresh device_code. Returns the plaintext
// (returned to the requesting device exactly once) and its SHA-256 hex
// digest (the only form that is persisted).
func MintDeviceCode() (plain, hash string, err error) {
	buf := make([]byte, deviceCodeRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = DeviceCodePrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plain, HashDeviceCode(plain), nil
}

// HashDeviceCode returns the SHA-256 hex digest of a device_code.
// Whitespace is trimmed (a client that pasted the value into a form
// field is a client we can still serve) but nothing else is
// normalized -- the device_code is machine-held, so a case fold would
// only destroy entropy.
func HashDeviceCode(plain string) string {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// MintUserCode generates a fresh user_code. Returns the display form
// (XXXX-XXXX) and the SHA-256 hex digest of its canonical form.
//
// The digest is taken over the CANONICAL (separator-stripped) body, so
// a human who types the separator and a human who omits it produce the
// same lookup key. See CanonicalizeUserCode.
func MintUserCode() (display, hash string, err error) {
	idx := make([]byte, UserCodeChars)
	if _, err := rand.Read(idx); err != nil {
		return "", "", err
	}
	body := make([]byte, UserCodeChars)
	for i, b := range idx {
		// 256 is an exact multiple of 32, so the modulo is unbiased.
		body[i] = userCodeAlphabet[int(b)%len(userCodeAlphabet)]
	}
	canonical := string(body)
	return FormatUserCode(canonical), hashCanonicalUserCode(canonical), nil
}

// FormatUserCode renders a canonical body as the two-group display
// form. A body that is not exactly UserCodeChars long is returned
// unchanged rather than sliced -- callers pass canonical values, and a
// panic here would be a worse failure than an unformatted string.
func FormatUserCode(canonical string) string {
	if len(canonical) != UserCodeChars {
		return canonical
	}
	half := UserCodeChars / 2
	return canonical[:half] + UserCodeSeparator + canonical[half:]
}

// CanonicalizeUserCode normalizes a human-typed user_code to the form
// the hash is taken over: uppercase, no separator, no whitespace.
//
// What it tolerates, and why each is deliberate:
//
//   - CASE. The code is displayed uppercase but typed by whoever is at
//     the keyboard; rejecting "abcd-efgh" would be rejecting a correct
//     answer.
//   - THE SEPARATOR, present or absent, and spelled '-', ' ' or '_'.
//     The hyphen is a rendering choice, not part of the secret.
//   - SURROUNDING WHITESPACE, from a copy-paste.
//
// What it does NOT tolerate is a character outside the alphabet. There
// is no aliasing pass mapping O to 0 or I to 1: the alphabet excludes
// both members of each confusable pair precisely so that no such
// mapping is needed, and accepting a character the minter can never
// produce would only widen the input space a brute-force walks.
//
// Returns "" for anything it cannot canonicalize, so a caller can
// refuse without hashing garbage.
func CanonicalizeUserCode(in string) string {
	stripped := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '-', '_':
			return -1
		}
		return r
	}, in)
	if len(stripped) != UserCodeChars {
		return ""
	}
	upper := strings.ToUpper(stripped)
	for _, c := range upper {
		if !strings.ContainsRune(userCodeAlphabet, c) {
			return ""
		}
	}
	return upper
}

// HashUserCode canonicalizes a human-typed user_code and returns its
// SHA-256 hex digest, or "" when the input is not a well-formed code.
func HashUserCode(in string) string {
	canonical := CanonicalizeUserCode(in)
	if canonical == "" {
		return ""
	}
	return hashCanonicalUserCode(canonical)
}

func hashCanonicalUserCode(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

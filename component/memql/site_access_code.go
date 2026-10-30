package memql

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// site_access_code.go -- the access code and the session token it trades for
// (deployable access codes; decisions D1, D2, D8 and D10 of
// docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md).
//
// It lives beside site_preview_token.go for the reason that file gives.
// MINTING and PRESENTING are two ends of one protocol: integrations/siteaccess
// creates a code and stores its digest, and the bff's access handler
// normalizes and hashes what a visitor typed and looks the row up by it. Two
// implementations of one normalization would be a code that opens nothing, and
// the symptom -- the same "invalid" a stranger gets -- says nothing about the
// cause.

// An access code is a BEARER CREDENTIAL that one person hands to another. It is
// read aloud, pasted from an email and retyped, and each choice below follows
// from that sentence.
//
//   - 25 symbols of Crockford base32 from crypto/rand: 125 bits, more than the
//     122 a random UUID carries. They are shown as five groups of five
//     (K7Q4M-2XPDN-8RT3V-WJ6HC-9FBZA) because people copy a short group at a
//     time.
//   - NO FAMILY PREFIX, unlike the mql_ tokens. A prefix is there so a machine
//     can find a leak; this one is typed by hand, and eight characters of
//     prefix are eight more to mistype.
//   - ONLY THE DIGEST IS STORED. The plain code goes back to the operator once,
//     in the reply to the mint, and exists afterwards on a screen and in
//     whatever the operator sent it through -- so a row read, a backup or a log
//     line hands nobody a working code.
//   - The digest is a plain SHA-256 with no salt. A salt defends a guessable
//     secret against a table built ahead of time, and nobody builds a table
//     over 2^125 values.

// accessCodeAlphabet is Crockford's base32: the digits and the letters without
// I, L, O and U.
//
// I, L and O are missing because NormalizeAccessCode rewrites them (O to zero, I
// and L to one), and a symbol the normalizer rewrites is a second draw for a
// string another symbol already produces: O and 0 would both come out as 0. A
// code carrying one would still open, since its digest is taken of the normalized
// spelling, but it would hold fewer than 125 bits -- about 123 for a single
// swapped letter -- and nothing about it would look wrong. They are also the
// characters people confuse, so a displayed code never asks anyone to tell them
// apart. U is left out as Crockford leaves it, to make an accidental obscenity
// less likely.
//
// It is exactly 32 symbols, which is what makes drawing one by keeping five bits
// of a random byte exact: a shorter alphabet would be read past its end and a
// longer one would never have its last symbols drawn. The test beside this file
// holds the two together.
const accessCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	// accessCodeSymbols is the length of a code: 25 symbols of 5 bits, 125 bits.
	accessCodeSymbols = 25

	// accessCodeGroup is how many symbols are shown before a hyphen.
	accessCodeGroup = 5

	// accessCodeHintLen is how much of the end of a code is kept in the clear.
	accessCodeHintLen = 4

	// accessCodeSymbolMask keeps the low five bits of a random byte, which index
	// the 32 symbols of the alphabet.
	accessCodeSymbolMask = 0x1f
)

// MintAccessCode returns a fresh access code, its digest and a short hint.
//
// The three come back TOGETHER, and the caller stores the digest and the hint
// and never the code. Splitting them into separate functions would let a call
// site store the plain value by writing one line; here there is no shape of the
// call that does.
//
// The hint is the last four symbols. That gives away 20 of the 125 bits to
// anybody who reads the row and leaves 105, far past anything that can be
// guessed, and it is what lets an operator tell one code from another in a list.
func MintAccessCode() (code string, digest string, hint string, err error) {
	buf := make([]byte, accessCodeSymbols)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("memql: mint access code: %w", err)
	}

	// One random byte per symbol, keeping its low five bits. 256 is a multiple of
	// 32, so every symbol is equally likely. Reducing a byte modulo a number that
	// does not divide 256 would favour the low symbols, and nobody would see it.
	var b strings.Builder
	for i, r := range buf {
		if i > 0 && i%accessCodeGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(accessCodeAlphabet[r&accessCodeSymbolMask])
	}
	code = b.String()
	return code, AccessCodeDigest(code), code[len(code)-accessCodeHintLen:], nil
}

// NormalizeAccessCode folds the ways people write a code down onto the one
// spelling its digest is taken of: ASCII letters in upper case, O read as zero
// and I and L as one, and every character that is then not one of the 32 symbols
// dropped.
//
// DROPPING EVERYTHING ELSE, rather than listing the separators to drop, is the
// point (decision D2: a wrong-looking character must not read as a wrong code). A
// code is read aloud, pasted from an email and retyped, and what comes along with
// it is not a short list. Hyphens, spaces and line breaks, yes, but also an en
// dash that an email client put where a hyphen was, a soft hyphen, a zero-width
// space, a non-breaking space. The person often cannot see the difference, so
// cannot fix it.
//
// It costs no entropy. A real code is exactly 25 symbols of the alphabet, so what
// is left after dropping either IS those symbols in order or it is not, and a
// wrong, short, long or reordered run of symbols still matches nothing. The whole
// input is reduced to one string and hashed once -- nothing is searched for inside
// it -- so a long input is not a way to try several codes in one request.
//
// Case is folded for ASCII only. strings.ToUpper would turn a dotless i into I,
// and so into a one, and a long s into S, so two characters nobody means as a
// symbol would open the door as one. Here neither is a symbol, and both are
// dropped like any other stray character.
func NormalizeAccessCode(raw string) string {
	return strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' {
			r -= 'a' - 'A'
		}
		switch r {
		case 'O':
			return '0'
		case 'I', 'L':
			return '1'
		}
		if strings.ContainsRune(accessCodeAlphabet, r) {
			return r
		}
		return -1
	}, raw)
}

// AccessCodeDigest is the stored form of a code: lowercase hex SHA-256 of its
// NORMALIZED spelling.
//
// Hashing the normalized form rather than the typed one is what makes finding a
// code an equality on one column. "k7q4m 2xpdn ..." and "K7Q4M-2XPDN-..." are the
// same credential, so they have to be the same digest.
func AccessCodeDigest(raw string) string {
	return hexSHA256(NormalizeAccessCode(raw))
}

// The session token is what a visitor's browser holds in its cookie after a code
// has opened the door. It has the shape of a preview token, for the same
// reasons: a prefixed 256-bit token is greppable in a leak scan and
// unmistakable in a support transcript, and only its digest is stored.
//
// WHAT IS DELIBERATELY NOT HERE IS A SIGNATURE. A signed token verifies on every
// replica with no read, and the price is that it cannot be revoked. Here
// revocation is the feature: a session has to end the moment its code is
// regenerated, removed or expired, and the access decision reads the database on
// every request anyway (decision D11), so a signature would save no read and
// give up the one thing the session row is for.

// AccessSessionTokenPrefix is the family marker every access session token
// carries. It joins mql_prv_ and the other mql_ prefixes.
const AccessSessionTokenPrefix = "mql_acs_"

// accessSessionTokenBytes is the entropy behind the 43 encoded characters.
const accessSessionTokenBytes = 32

// AccessSessionMaxAge is the longest a session lasts, whatever the code's expiry
// (decision D10).
//
// A code that lasts a month should not produce a session that lasts a month on a
// machine somebody walked away from. Typing the code again once a day is a small
// cost beside that.
const AccessSessionMaxAge = 24 * time.Hour

// MintAccessSessionToken returns a fresh session token and its SHA-256 digest.
//
// The two are returned TOGETHER and the caller stores only the second, for the
// reason MintPreviewToken gives: no shape of the call stores the plain value.
func MintAccessSessionToken() (token string, digest string, err error) {
	buf := make([]byte, accessSessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("memql: mint access session token: %w", err)
	}
	token = AccessSessionTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, AccessSessionTokenDigest(token), nil
}

// AccessSessionTokenDigest is the stored form of a session token: lowercase hex
// SHA-256 of the exact string presented, prefix included.
//
// THE WHOLE STRING IS HASHED, so a caller cannot present the suffix alone and
// match. Whitespace is trimmed first because a token travels in a cookie and
// occasionally through a copy-paste, and a trailing newline must not read as a
// wrong credential.
func AccessSessionTokenDigest(token string) string {
	return hexSHA256(strings.TrimSpace(token))
}

const (
	// minAccessCodeLifetime is how far away an expiry must be, exclusively.
	minAccessCodeLifetime = time.Minute

	// maxAccessCodeLifetime is how far away an expiry may be, inclusively: 366
	// days, a year with its leap day.
	maxAccessCodeLifetime = 366 * 24 * time.Hour
)

// ValidateAccessCodeExpiry refuses an expiry a code may not have: one that is
// not more than a minute away, or that is more than 366 days away.
//
// EVERY CODE ENDS (decision D8). The lifetime is the one honest bound on a
// bearer credential, so there is no "never", and the zero time -- what an unset
// expiry looks like -- is refused as already passed rather than read as
// unlimited. That is the reading ClampPreviewGrantTTL refuses, for the same
// reason. A long engagement picks a far date, and the expiry can be moved later
// without changing the code.
//
// The minimum is there because a code that dies within a minute cannot be handed
// to anybody, which is nearly always a wrong date or a wrong time zone.
//
// `now` is passed in so the decision is made against the caller's clock, which
// is the engine's, and so a test can pin it. The refusals are sentences the OS
// shows exactly as written, so each says what to change; they carry no "memql:"
// prefix, which is for failures an operator cannot act on.
func ValidateAccessCodeExpiry(now, expiresAt time.Time) error {
	lifetime := expiresAt.Sub(now)
	switch {
	case lifetime <= 0:
		return errors.New("that expiry has already passed; choose a time more than 1 minute from now")
	case lifetime <= minAccessCodeLifetime:
		return errors.New("that expiry is too soon; choose a time more than 1 minute from now")
	case lifetime > maxAccessCodeLifetime:
		return errors.New("that expiry is more than 366 days away; choose an earlier time, and extend the expiry later if the code is still needed")
	}
	return nil
}

// AccessSessionEnd is the instant a session stops being honored: the EARLIER of
// the code's expiry and AccessSessionMaxAge after the session opened (decision
// D10).
//
// It is worked out on every request from the code's CURRENT expiry rather than
// stored on the session, so an operator who shortens or extends a code moves the
// end of every session already open on it without a write to any of them.
//
// A zero codeExpiresAt returns the zero time, which is long past. A code whose
// expiry is missing ends the session at once; it is not read as "nothing to cap
// it" and handed a full day.
func AccessSessionEnd(issuedAt, codeExpiresAt time.Time) time.Time {
	capEnd := issuedAt.Add(AccessSessionMaxAge)
	if codeExpiresAt.Before(capEnd) {
		return codeExpiresAt
	}
	return capEnd
}

// hexSHA256 is how every digest in this file is spelled: lowercase hex, because
// a stored digest and a presented one are compared as strings.
func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

package campaigns

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// digest.go -- how an address becomes a suppression key (memql#3348).
//
// The cluster-wide do-not-mail list stores a SHA-256 digest, never the
// address, so "is this address suppressed" is answerable by anyone
// holding the address and by nobody else. That only works if two
// spellings of one mailbox produce one digest, which is what
// NormalizeEmail is for.
//
// # What is normalized, and what deliberately is NOT
//
// Normalization here is CASE AND WHITESPACE ONLY:
//
//   - trim surrounding whitespace,
//   - lowercase the whole address.
//
// Lowercasing the LOCAL PART is technically over-normalization -- RFC 5321
// says the local part is case-sensitive and only the domain is not. It is
// the right call anyway, in this direction: essentially no real mail
// system treats `Alice@` and `alice@` as different people, and the two
// possible errors are not symmetric. Over-normalizing suppresses a
// message we might have been allowed to send. Under-normalizing sends a
// message to somebody who asked us not to. Only one of those is a
// compliance failure.
//
// What is NOT done, and each omission is deliberate:
//
//   - No plus-address stripping (`a+news@x` stays distinct from `a@x`).
//     Sub-addressing is how a recipient CHOOSES to separate streams, and
//     collapsing it would suppress an address the person never opted out.
//     It also is not universal -- the separator is provider-specific --
//     so a general rule would be wrong somewhere.
//   - No dot-stripping in the local part. That is a Gmail-specific
//     equivalence; applying it globally merges genuinely distinct
//     mailboxes on providers that do not share it.
//   - No Unicode / IDN folding. An internationalized domain has a
//     canonical A-label form, and mapping to it correctly needs IDNA
//     processing this does not do. Rather than fold half-correctly, the
//     digest is taken over the bytes as given -- a U-label and its
//     A-label spelling therefore suppress separately, which is a known
//     gap rather than a silent one.
//
// The rule to keep: any change here changes what an existing digest
// matches, so it retroactively un-suppresses rows already on the list.
// Treat this function as a stored format, not a helper.

// NormalizeEmail returns the canonical form an address is digested
// under, or "" when the input is not usable as an address at all.
func NormalizeEmail(addr string) string {
	trimmed := strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndex(trimmed, "@")
	if at <= 0 || at == len(trimmed)-1 {
		return ""
	}
	if strings.ContainsAny(trimmed, " \t\r\n") {
		return ""
	}
	return trimmed
}

// EmailDigest returns the SHA-256 hex digest of the normalized address --
// the row id of its v1:campaigns:suppression entry. Returns "" for an
// address NormalizeEmail rejects, so a caller cannot accidentally
// suppress the digest of the empty string and block nothing while
// appearing to work.
func EmailDigest(addr string) string {
	normalized := NormalizeEmail(addr)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// EmailDomain returns the lowercased domain part of an address, or "" if
// there is not one. This is the only human-readable thing the suppression
// row stores: it makes "which domains are bouncing" answerable to a
// deliverability review without making "who unsubscribed" answerable to
// anyone.
func EmailDomain(addr string) string {
	normalized := NormalizeEmail(addr)
	if normalized == "" {
		return ""
	}
	return normalized[strings.LastIndex(normalized, "@")+1:]
}

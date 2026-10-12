package emailrules

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// digest.go -- how a row-derived address becomes the key its membership in the
// rule's audience is found by, and how a row's own address is compared to it
// (memql#6022, F9).
//
// Every recipient written by addRecipient carries emailDigest, the SHA-256 of its
// address, and a row-address firing finds the rows a mailbox holds by that digest
// (Store.MailboxMembers) instead of listing the audience and comparing every
// address. The digest is a key, not proof -- a raw write can set one that is not
// its row's -- and a row written before the digest existed carries none, so each
// row's own address is compared too (normalizeAddress).
//
// # Why this is a copy, and what holds it to the original
//
// The rule is campaigns.NormalizeEmail and campaigns.EmailDigest. This package
// does not import component/campaigns, so it is written out here, and
// TestNormalizeAddressMatchesCampaigns and TestAddressDigestMatchesCampaigns --
// the one place this package's tests do import it -- fail if the two ever
// differ. They must not: a rule that looks up a digest the campaigns package
// never stamped finds nobody, and enrols a second membership for a person who is
// already in the audience.
//
// # The normalization
//
// The address is trimmed and lower-cased -- case and surrounding whitespace, and
// NOTHING ELSE. Sub-addressing (a+news@x) and dots in the local part are the
// person's own distinctions and stay distinct; an internationalized domain is
// digested as given. An address that cannot be a mailbox -- no "@", an empty
// local part or domain, whitespace inside -- has no normal form, and
// normalizeAddress says so with "".
//
// Four places write this one rule and a test holds each to the others:
// campaigns.NormalizeEmail and EmailDigest (the original, documented at length in
// component/campaigns/digest.go), database.RecipientEmailDigest (the digest
// sweep), this file, and the DSL stamp on addRecipient,
// hash(lower(trim(args.email))).

// digestLessWindow is how many rows of an audience that carry no digest one read
// of them returns. It mirrors the `paginate` on recipientsWithoutDigestInAudience
// and decides when a page is full: a full page says nothing about the rows beyond it.
const digestLessWindow = 50

// digestWindow mirrors recipientInAudienceByDigest. A full page may hide an
// opted-out duplicate, even when every recipient already carries a digest.
const digestWindow = 50

// normalizeAddress returns the form an address is compared and digested under,
// or "" when the text cannot be a mailbox.
func normalizeAddress(addr string) string {
	normalized := strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndex(normalized, "@")
	if at <= 0 || at == len(normalized)-1 || strings.ContainsAny(normalized, " \t\r\n") {
		return ""
	}
	return normalized
}

// addressDigest is the SHA-256, as lower-case hex, of normalizeAddress(addr), or
// "" when the address has no normal form.
func addressDigest(addr string) string {
	normalized := normalizeAddress(addr)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

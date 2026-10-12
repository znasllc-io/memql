package emailrules

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// digest.go -- how a row-derived address becomes the key its membership in the
// rule's audience is found by (memql#6022, F9).
//
// Every recipient row carries emailDigest, the SHA-256 of its address, and a
// row-address firing finds the member for the address on the triggering row with
// one keyed read by that digest (Store.RecipientInAudienceByDigest) instead of
// listing the audience and comparing addresses.
//
// # Why this is a copy, and what holds it to the original
//
// The digest is campaigns.EmailDigest. This package does not import
// component/campaigns, so the rule is written out here, and
// TestAddressDigestMatchesCampaigns -- the one place this package's tests do
// import it -- fails if the two ever differ. They must not: a rule that looks
// up a digest the campaigns package never stamped finds nobody, and enrols a
// second membership for a person who is already in the audience.
//
// # The normalization
//
// The address is trimmed and lower-cased -- case and surrounding whitespace, and
// NOTHING ELSE. Sub-addressing (a+news@x) and dots in the local part are the
// person's own distinctions and stay distinct; an internationalized domain is
// digested as given. An address that cannot be a mailbox -- no "@", an empty
// local part or domain, whitespace inside -- has no digest, and addressDigest
// says so with "".
//
// Four places write this one rule and a test holds each to the others:
// campaigns.EmailDigest (the original, documented at length in
// component/campaigns/digest.go), database.RecipientEmailDigest (the backfill
// migration), this function, and the DSL stamp on addRecipient,
// hash(lower(trim(args.email))).
func addressDigest(addr string) string {
	normalized := strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndex(normalized, "@")
	if at <= 0 || at == len(normalized)-1 || strings.ContainsAny(normalized, " \t\r\n") {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

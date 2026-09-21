package memql

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// site_preview_token.go -- the preview credential (epic memql#5531).
//
// It lives beside site_preview_wire.go rather than in the package that mints
// it, because MINTING and PRESENTING are two ends of one protocol:
// integrations/sitepreview creates the token and component/edge hashes what a
// browser presented and looks the row up by it. Two implementations of one
// digest would be a preview that never resolves, and the symptom -- a link that
// silently serves the public site -- says nothing about the cause.

// A preview URL is a BEARER CREDENTIAL for an unpublished version of somebody's
// storefront. Everything about its shape follows from that one sentence, and
// each choice below has a twin somewhere else in this tree rather than being
// invented here.
//
//   - `mql_prv_` joins mql_wkr_ / mql_enr_ / mql_rec_ / mql_pat_. A prefixed
//     token is greppable in a leak scan and unmistakable in a support
//     transcript, which is the whole reason that convention exists.
//   - 32 bytes from crypto/rand, base64url with no padding: 43 characters, the
//     same 256 bits every other token in the family carries.
//   - ONLY THE DIGEST IS STORED. The plain token goes back to the caller once,
//     in the reply to sitePreviewOpen, and exists thereafter in the operator's
//     URL and cookie and nowhere else -- so a row read, a backup or a log line
//     hands nobody a working preview (component/identity/workertoken sets the
//     precedent and the reasoning).
//
// WHAT IS DELIBERATELY NOT HERE IS A SIGNATURE. A signed token would verify on
// every replica with no read, which is what makes the app-session credential
// DB-free -- and the price there is stated plainly: it is not revocable. Here
// the traffic is one operator's handful of requests, so the read costs nothing,
// and revocation, listing and an id to hang observations off are all worth more
// than the read saves. See v1:platform:sitePreviewGrant's own note.

// PreviewTokenPrefix is the family marker every preview token carries.
const PreviewTokenPrefix = "mql_prv_"

// previewTokenBytes is the entropy behind the 43 encoded characters.
const previewTokenBytes = 32

// MintPreviewToken returns a fresh preview token and its SHA-256 digest.
//
// The two are returned TOGETHER and the caller stores only the second. Splitting
// them into separate functions would let a call site store the plain value by
// writing one line; here there is no shape of the call that does.
func MintPreviewToken() (token string, digest string, err error) {
	buf := make([]byte, previewTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("memql: mint preview token: %w", err)
	}
	token = PreviewTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, PreviewTokenDigest(token), nil
}

// PreviewTokenDigest is the stored form of a token: lowercase hex SHA-256 of the exact
// string presented, prefix included.
//
// THE WHOLE STRING IS HASHED, prefix and all, so a caller cannot present the
// suffix alone and match. Whitespace is trimmed first because a token travels
// through a URL, a cookie and occasionally a copy-paste, and a trailing newline
// must not read as a wrong credential -- which would be a refusal nobody could
// diagnose from either side.
func PreviewTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// WellFormedPreviewToken reports whether a presented string could be a preview token at all.
//
// A CHEAP REJECTION BEFORE A READ, and nothing more: it says nothing about
// whether a grant exists, is live, or is for this site. The edge calls it so a
// cookie somebody pasted in from another site costs a string comparison rather
// than a query -- which matters because the cookie arrives on every request to
// the origin, including every asset.
func WellFormedPreviewToken(token string) bool {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, PreviewTokenPrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, PreviewTokenPrefix))
	return err == nil && len(raw) == previewTokenBytes
}

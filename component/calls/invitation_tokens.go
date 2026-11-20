package calls

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// InvitationLinkToken derives the recoverable guest capability for canonical
// owner/site/room ids. It is a codec, not an authorization decision: the service
// must establish the current host's authority before returning or registering it.
// Length framing prevents ambiguity between scope components. The versioned domain
// separates this MAC from stored token digests, invitation receipt ids and future
// capabilities. Truncation preserves the existing 160-bit token format and strength.
func InvitationLinkToken(pepper []byte, owner, site, room string) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte("memql.calls.guest-invitation-link.v1\x00"))
	for _, scope := range []string{owner, site, room} {
		mac.Write(binary.BigEndian.AppendUint32(nil, uint32(len(scope))))
		mac.Write([]byte(scope))
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(mac.Sum(nil)[:TokenBytes]))
}

// InvitationFingerprint is the existing receipt wire format: SHA-256 of subject,
// plain text and HTML in that order with NUL separators, lowercase hex. Keeping it
// stable lets a retry recognize the same prepared message across engine versions;
// recipient deduplication uses a separate keyed identity and never this checksum.
func InvitationFingerprint(subject, textBody, htmlBody string) string {
	fingerprint := sha256.Sum256([]byte(subject + "\x00" + textBody + "\x00" + htmlBody))
	return hex.EncodeToString(fingerprint[:])
}

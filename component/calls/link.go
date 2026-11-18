package calls

import (
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
)

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// TokenBytes is the randomness in every session token and call link a calls service
// issues: 160 bits, which no guessing at any request rate reaches. It is the capability's
// constant, beside its spelling, so the service that issues a token and a route that
// refuses one before reading anything hold it to the same shape.
const TokenBytes = 20

// tokenSymbols is how many symbols NewToken spells TokenBytes as.
var tokenSymbols = tokenEncoding.EncodedLen(TokenBytes)

// TokenWellFormed reports whether s is exactly what NewToken(r, TokenBytes) makes: that
// many symbols of lowercase RFC 4648 base32, unpadded, and nothing around them. A presented
// token that is not was never issued, which a server can say without reading anything, so a
// guess of the wrong shape costs no read. Callers trim what a person typed first; the
// check itself takes nothing on trust.
func TokenWellFormed(s string) bool {
	if len(s) != tokenSymbols {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// NewToken reads exactly n random bytes from r and returns them as RFC 4648
// base32 in lowercase with no padding. It adds no prefix: a caller that needs
// one (a session code) prepends its own. A short read or a non-positive n is
// an error, never a shorter token.
func NewToken(r io.Reader, n int) (string, error) {
	if n <= 0 {
		return "", errors.New("token length must be positive")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read token randomness: %w", err)
	}
	return strings.ToLower(tokenEncoding.EncodeToString(buf)), nil
}

// GuestLink is a call's guest link on a deployable's public host:
// https://<host>/#j=<guest token>. THE TOKEN RIDES THE FRAGMENT, which a
// browser never sends to a server, so it reaches no server log and no Referer.
func GuestLink(host, guestToken string) string {
	return "https://" + host + "/#j=" + guestToken
}

// HostLink is a call's host link: the guest link with &h=<host token> added,
// in the fragment for GuestLink's reason. It carries both secrets, because a
// host joins the call the guest link names.
func HostLink(host, guestToken, hostToken string) string {
	return GuestLink(host, guestToken) + "&h=" + hostToken
}

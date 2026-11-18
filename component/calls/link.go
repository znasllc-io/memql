package calls

import (
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
)

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

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

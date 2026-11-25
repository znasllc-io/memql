package calls

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The tester password lets the people an operator invites apply simulated
// payment and age results while commerce runs in mock mode. It is a generated
// 100-bit secret, like an API key, never a password a person chose: a salted
// SHA-256 makes a leaked hash useless, and a deliberately slow password hash
// would add nothing at this entropy while giving an attacker a CPU or memory
// cost to trigger.
const (
	// The alphabet leaves out I, O, 0 and 1, which are read for one another when a
	// password is copied by hand (component/accountexport uses the same choice).
	// Its 32 characters divide 256, so a random byte masked to five bits picks each
	// one with the same chance.
	testerPasswordAlphabet  = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	testerPasswordGroups    = 4
	testerPasswordGroupSize = 5
	testerPasswordSaltBytes = 16
	// testerPasswordMaxBytes bounds what a request may carry; a generated password
	// is 23 bytes, so anything longer than a generous paste allowance is refused.
	testerPasswordMaxBytes = 64
)

// testerPasswordLine is the only accepted hash format: version, 16-byte salt and
// SHA-256 digest, both unpadded base64url. The fixed lengths reject whitespace,
// padding and the standard alphabet before decoding.
var testerPasswordLine = regexp.MustCompile(`^v1\$([A-Za-z0-9_-]{22})\$([A-Za-z0-9_-]{43})$`)

type testerPasswordHash struct{ salt, digest []byte }

// parseTesterPasswordHash reads the installation's hash line. Surrounding
// whitespace is trimmed (a Secret made from a file keeps its newline); anything
// else that is not exactly the v1 format means the gate is not configured.
func parseTesterPasswordHash(line string) (*testerPasswordHash, bool) {
	m := testerPasswordLine.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return nil, false
	}
	salt, err := base64.RawURLEncoding.Strict().DecodeString(m[1])
	if err != nil || len(salt) != testerPasswordSaltBytes {
		return nil, false
	}
	digest, err := base64.RawURLEncoding.Strict().DecodeString(m[2])
	if err != nil || len(digest) != sha256.Size {
		return nil, false
	}
	return &testerPasswordHash{salt: salt, digest: digest}, true
}

// canonicalTesterPassword is what a tester means: case, dashes and whitespace
// (including a password manager's trailing newline) are not part of it.
func canonicalTesterPassword(typed string) string {
	var b strings.Builder
	for _, r := range typed {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

func testerPasswordDigest(salt []byte, canonical string) []byte {
	d := sha256.New()
	d.Write(salt)
	d.Write([]byte(canonical))
	return d.Sum(nil)
}

// matches compares in constant time. A nil hash (not configured) and an empty
// password never match.
func (h *testerPasswordHash) matches(typed string) bool {
	if h == nil {
		return false
	}
	canonical := canonicalTesterPassword(typed)
	if canonical == "" {
		return false
	}
	return subtle.ConstantTimeCompare(testerPasswordDigest(h.salt, canonical), h.digest) == 1
}

// GenerateTesterPassword answers a new password (four groups of five, joined by
// "-") and the hash line an operator stores in MEMQL_CALLS_TESTER_PASSWORD_HASH.
// Callers must keep both out of logs and terminals.
func GenerateTesterPassword() (password, hashLine string, err error) {
	raw := make([]byte, testerPasswordGroups*testerPasswordGroupSize)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("calls: generate tester password: %w", err)
	}
	out := make([]byte, 0, len(raw)+testerPasswordGroups-1)
	for i, b := range raw {
		if i > 0 && i%testerPasswordGroupSize == 0 {
			out = append(out, '-')
		}
		out = append(out, testerPasswordAlphabet[b&31])
	}
	salt := make([]byte, testerPasswordSaltBytes)
	if _, err = rand.Read(salt); err != nil {
		return "", "", fmt.Errorf("calls: generate tester password salt: %w", err)
	}
	password = string(out)
	digest := testerPasswordDigest(salt, canonicalTesterPassword(password))
	hashLine = "v1$" + base64.RawURLEncoding.EncodeToString(salt) + "$" + base64.RawURLEncoding.EncodeToString(digest)
	return password, hashLine, nil
}

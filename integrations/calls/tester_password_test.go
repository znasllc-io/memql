package calls

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTesterPasswordHashRoundTrip(t *testing.T) {
	pw, line, err := GenerateTesterPassword()
	require.NoError(t, err)
	require.Regexp(t, `^[A-HJ-NP-Z2-9]{5}(-[A-HJ-NP-Z2-9]{5}){3}$`, pw)
	require.Regexp(t, `^v1\$[A-Za-z0-9_-]{22}\$[A-Za-z0-9_-]{43}$`, line)
	h, ok := parseTesterPasswordHash(line)
	require.True(t, ok)
	require.True(t, h.matches(pw))
	// Pasted from a password manager or typed by hand: lowercase, spaces for
	// dashes, a trailing newline and tab all name the same password.
	require.True(t, h.matches(strings.ToLower(strings.ReplaceAll(pw, "-", " "))+"\n\t"))
	last := byte('X')
	if pw[len(pw)-1] == last {
		last = 'Y'
	}
	require.False(t, h.matches(pw[:len(pw)-1]+string(last)))
	require.False(t, h.matches(""))
	var none *testerPasswordHash
	require.False(t, none.matches(pw))
	other, _, err := GenerateTesterPassword()
	require.NoError(t, err)
	require.NotEqual(t, pw, other)
}

func TestTesterPasswordHashRejectsMalformed(t *testing.T) {
	salt := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	digest := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for name, line := range map[string]string{
		"empty":              "",
		"other version":      "v2$" + salt + "$" + digest,
		"short segments":     "v1$a$b",
		"short salt":         "v1$" + base64.RawURLEncoding.EncodeToString(make([]byte, 15)) + "$" + digest,
		"short digest":       "v1$" + salt + "$" + base64.RawURLEncoding.EncodeToString(make([]byte, 31)),
		"padded base64":      "v1$" + base64.URLEncoding.EncodeToString(make([]byte, 16)) + "$" + digest,
		"standard alphabet":  "v1$" + strings.Repeat("+", 22) + "$" + digest,
		"four segments":      "v1$" + salt + "$" + digest + "$extra",
		"inner whitespace":   "v1$ " + salt + "$" + digest,
		"missing digest":     "v1$" + salt,
		"plain password":     "K7QPM-3XWRT-9HNVA-ZD4ME",
		"uppercase version":  "V1$" + salt + "$" + digest,
		"empty salt segment": "v1$$" + digest,
	} {
		t.Run(name, func(t *testing.T) {
			h, ok := parseTesterPasswordHash(line)
			require.False(t, ok)
			require.Nil(t, h)
		})
	}
}

func TestTesterPasswordHashTrimsSurroundingWhitespace(t *testing.T) {
	pw, line, err := GenerateTesterPassword()
	require.NoError(t, err)
	// A Secret made with --from-file keeps the file's trailing newline.
	h, ok := parseTesterPasswordHash(" " + line + "\n")
	require.True(t, ok)
	require.True(t, h.matches(pw))
}

func TestCanonicalTesterPassword(t *testing.T) {
	require.Equal(t, "K7QPM3XWRT9HNVAZD4ME", canonicalTesterPassword(" k7qpm-3xwrt 9hnva\tzd4me\r\n"))
	require.Equal(t, "", canonicalTesterPassword(" - \n"))
}

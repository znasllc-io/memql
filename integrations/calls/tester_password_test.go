package calls

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/calls/providers"
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

// commerceEnvelopeEnv sets a complete mock envelope, as scripts/k3d/calls-mock.sh does.
func commerceEnvelopeEnv(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"MODE": "mock", "VALID_UNTIL": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"CASH_CENTS": "100000", "PROTECTED_CENTS": "10000", "FIXED_CENTS": "10000",
		"VERIFICATION_UNIT_CENTS": "100", "VERIFICATION_BUDGET_CENTS": "10000", "PROVIDER_CREDIT_CENTS": "10000",
		"CALL_BUDGET_CENTS": "50000", "FULFILLMENT_RESERVE_BPS": "5000", "MAX_ACCOUNTS": "200",
		"MAX_PENDING_VERIFICATIONS": "10", "MAX_VERIFICATION_ATTEMPTS": "3", "MAX_OPEN_CALLS": "5", "MAX_PARTICIPANTS": "40",
	} {
		t.Setenv("MEMQL_CALLS_COMMERCE_"+name, value)
	}
}

func TestCommerceConfigReadsTesterPasswordHash(t *testing.T) {
	pw, line, err := GenerateTesterPassword()
	require.NoError(t, err)
	commerceEnvelopeEnv(t)
	s := &Service{}

	t.Setenv("MEMQL_CALLS_TESTER_PASSWORD_HASH", line+"\n")
	c := s.commerceConfig()
	require.False(t, c.Invalid)
	require.NoError(t, c.admission(time.Now()))
	require.NotNil(t, c.TesterPassword)
	require.True(t, c.TesterPassword.matches(pw))

	// A malformed or missing hash leaves the envelope usable; only simulated
	// results are refused (the gate fails closed on a nil hash).
	t.Setenv("MEMQL_CALLS_TESTER_PASSWORD_HASH", "not-a-hash")
	c = s.commerceConfig()
	require.False(t, c.Invalid)
	require.NoError(t, c.admission(time.Now()))
	require.Nil(t, c.TesterPassword)

	t.Setenv("MEMQL_CALLS_COMMERCE_MODE", "disabled")
	t.Setenv("MEMQL_CALLS_TESTER_PASSWORD_HASH", line)
	c = s.commerceConfig()
	require.Equal(t, providers.Disabled, c.Mode)
	require.Nil(t, c.TesterPassword)
}

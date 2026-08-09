package enrolment

// The enrolment token's whole lifecycle, and the one property everything else
// rests on: the plaintext never leaves this package (memql#3408).
//
// The lifecycle tests drive a recording engine rather than a database. That is
// not a shortcut around the real thing -- what they are pinning is the
// DECISION, not the storage: which state a row resolves to, and therefore
// which of the four human messages a person sees and whether a second attempt
// is admitted. Every one of those is decided here, in Go, from the stamps on
// the row.

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// -----------------------------------------------------------------------
// Test engine
// -----------------------------------------------------------------------

// recordingEngine captures every query string the store sends and replies with
// a canned row for the lookup.
//
// Capturing the RAW query text is the point for the persistence test below:
// the store builds MemQL by string interpolation, so the query it emits is
// exactly, byte for byte, what would reach the database. A token appearing
// anywhere in that text is a token that would be persisted or logged.
type recordingEngine struct {
	queries []string
	row     map[string]string
	err     error
}

func (e *recordingEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	if e.err != nil {
		return nil, e.err
	}
	if !strings.HasPrefix(q, "query enrolmentTokenByHash") || e.row == nil {
		return nil, nil
	}
	fields := map[string]*structpb.Value{}
	for k, v := range e.row {
		fields[k] = structpb.NewStringValue(v)
	}
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{
				Id:      e.row["id"],
				Payload: &structpb.Struct{Fields: fields},
			}},
		},
	}, nil
}

func (e *recordingEngine) allQueries() string { return strings.Join(e.queries, "\n") }

const testUser = "v1:identity:user:target"

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// liveRow is a freshly-issued, unspent, unexpired, unrevoked token row.
func liveRow(hash string, now time.Time) map[string]string {
	return map[string]string{
		"id":        "enr-1",
		"userId":    testUser,
		"tokenHash": hash,
		"issuedBy":  "v1:identity:user:admin",
		"expiresAt": rfc(now.Add(DefaultTTL)),
		"createdAt": rfc(now),
	}
}

// -----------------------------------------------------------------------
// Token format
// -----------------------------------------------------------------------

func TestMintProducesThePinnedWireFormat(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Fatalf("plain = %q, want the %q prefix", plain, TokenPrefix)
	}
	body := strings.TrimPrefix(plain, TokenPrefix)
	if len(body) != TokenBodyChars {
		t.Errorf("body length = %d, want %d (32 CSPRNG bytes as base64url-no-pad)", len(body), TokenBodyChars)
	}
	// Decoded rather than pattern-matched: this is what proves the body is 32
	// BYTES of randomness and not 43 characters of something else.
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("body is not base64url-no-pad: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("decoded entropy = %d bytes, want 32", len(raw))
	}
	if strings.ContainsAny(body, "+/=") {
		t.Errorf("body %q leaked the base64 alphabet; it travels in a URL query string", body)
	}
	if hash != Hash(plain) {
		t.Errorf("Mint's hash disagrees with Hash(plain)")
	}
	if len(hash) != 64 {
		t.Errorf("hash = %d chars, want 64 (SHA-256 hex) -- the shared convention, not a new one", len(hash))
	}
	if !IsEnrolmentToken(plain) || IsEnrolmentToken("mql_pat_"+body) {
		t.Errorf("IsEnrolmentToken does not discriminate on the prefix")
	}
}

func TestEveryMintIsFresh(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		plain, _, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if seen[plain] {
			t.Fatalf("Mint repeated a token after %d draws", i)
		}
		seen[plain] = true
	}
}

func TestClampTTL(t *testing.T) {
	if got := ClampTTL(0); got != DefaultTTL {
		t.Errorf("ClampTTL(0) = %v, want the %v default", got, DefaultTTL)
	}
	if got := ClampTTL(-time.Hour); got != DefaultTTL {
		t.Errorf("ClampTTL(negative) = %v, want the default", got)
	}
	if got := ClampTTL(time.Hour); got != time.Hour {
		t.Errorf("ClampTTL(1h) = %v, want it honoured", got)
	}
	if got := ClampTTL(30 * 24 * time.Hour); got != MaxTTL {
		t.Errorf("ClampTTL(30d) = %v, want it clamped to %v", got, MaxTTL)
	}
}

// -----------------------------------------------------------------------
// THE PLAINTEXT IS NEVER PERSISTED
// -----------------------------------------------------------------------

// TestPlaintextTokenIsNeverPersisted is the acceptance criterion asserted
// rather than inspected.
//
// It drives the FULL write path -- mint, create, look up, consume, revoke --
// and then reads every query string the store actually emitted. Those strings
// are the writes: the store builds MemQL by interpolation, so what is in them
// is what reaches the database, and what is not in them cannot.
//
// Both spellings are checked. The raw token would be the obvious mistake; the
// URL-escaped one is the subtle one, because the link that carries this token
// is percent-encoded and a copy-paste from the link-composition path would
// land escaped and slip a substring search for the raw value.
//
// The hash IS expected to be there. Asserting its presence in the same test is
// what stops the check from passing trivially against a store that persists
// nothing at all.
func TestPlaintextTokenIsNeverPersisted(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	now := time.Now().UTC()
	eng := &recordingEngine{row: liveRow(hash, now)}
	store := &Store{Engine: eng}
	ctx := context.Background()

	if err := store.Create(ctx, "enr-1", testUser, hash, "v1:identity:user:admin", now.Add(DefaultTTL), "203.0.113.9"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := store.Resolve(ctx, plain, now); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := store.Consume(ctx, "enr-1", "203.0.113.9", now); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := store.Revoke(ctx, "enr-1", now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	written := eng.allQueries()
	if written == "" {
		t.Fatal("no queries were captured -- this test would pass vacuously")
	}
	body := strings.TrimPrefix(plain, TokenPrefix)
	for _, forbidden := range []struct {
		what  string
		value string
	}{
		{"the plaintext token", plain},
		{"the token body without its prefix", body},
		{"the URL-escaped token", strings.ReplaceAll(plain, "_", "%5F")},
	} {
		if strings.Contains(written, forbidden.value) {
			t.Errorf("%s reached the engine -- it must never be persisted or logged.\nqueries:\n%s",
				forbidden.what, written)
		}
	}
	if !strings.Contains(written, hash) {
		t.Errorf("the SHA-256 hash never reached the engine, so the check above proves nothing.\nqueries:\n%s", written)
	}
}

// TestRowHasNowhereToPutAPlaintext is the structural half of the same
// property: even a caller determined to persist the token has no field to put
// it in, and Create has no parameter that would accept one.
func TestRowHasNowhereToPutAPlaintext(t *testing.T) {
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	now := time.Now().UTC()
	eng := &recordingEngine{row: liveRow(hash, now)}
	row, err := (&Store{Engine: eng}).LookupByHash(context.Background(), hash)
	if err != nil || row == nil {
		t.Fatalf("LookupByHash: row=%v err=%v", row, err)
	}
	if row.TokenHash != hash {
		t.Errorf("TokenHash = %q, want the digest", row.TokenHash)
	}
	if strings.Contains(row.TokenHash, plain) {
		t.Errorf("the row's only token field carries the plaintext")
	}
}

// -----------------------------------------------------------------------
// Lifecycle: issue -> redeem -> expire -> reuse-rejected -> revoked
// -----------------------------------------------------------------------

func TestLifecycleStates(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	plain, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	cases := []struct {
		name  string
		row   func(map[string]string) map[string]string
		at    time.Time
		want  State
		which string
	}{
		{
			name:  "issued -- a fresh link redeems",
			row:   func(r map[string]string) map[string]string { return r },
			at:    now,
			want:  StateValid,
			which: "a link that has just been issued must work",
		},
		{
			name: "expired -- the clock ran out before anyone used it",
			row: func(r map[string]string) map[string]string {
				r["expiresAt"] = rfc(now.Add(-time.Minute))
				return r
			},
			at:    now,
			want:  StateExpired,
			which: "an aged-out link must say so, not read as invalid",
		},
		{
			name: "already used -- the one registration it authorized happened",
			row: func(r map[string]string) map[string]string {
				r["consumedAt"] = rfc(now.Add(-time.Minute))
				return r
			},
			at:    now,
			want:  StateAlreadyUsed,
			which: "REPLAY: a second presentation of a spent token must be refused",
		},
		{
			name: "revoked -- the issuer killed it first",
			row: func(r map[string]string) map[string]string {
				r["revokedAt"] = rfc(now.Add(-time.Minute))
				return r
			},
			at:    now,
			want:  StateRevoked,
			which: "a revoked link must be distinguishable from a spent one",
		},
		{
			name: "revoked AND expired -- the decision beats the clock",
			row: func(r map[string]string) map[string]string {
				r["revokedAt"] = rfc(now.Add(-2 * time.Minute))
				r["expiresAt"] = rfc(now.Add(-time.Minute))
				return r
			},
			at:    now,
			want:  StateRevoked,
			which: "somebody cancelled this is more useful than the clock ran out",
		},
		{
			name: "used AND expired -- what ended it was the use",
			row: func(r map[string]string) map[string]string {
				r["consumedAt"] = rfc(now.Add(-2 * time.Minute))
				r["expiresAt"] = rfc(now.Add(-time.Minute))
				return r
			},
			at:    now,
			want:  StateAlreadyUsed,
			which: "a spent link that later expired is still best described as spent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &recordingEngine{row: tc.row(liveRow(hash, now))}
			row, state, err := (&Store{Engine: eng}).Resolve(context.Background(), plain, tc.at)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if row == nil {
				t.Fatal("Resolve returned no row for a hash that matches")
			}
			if state != tc.want {
				t.Errorf("state = %q, want %q -- %s", state, tc.want, tc.which)
			}
			if row.UserId != testUser {
				t.Errorf("UserId = %q, want the row's own user (never a caller argument)", row.UserId)
			}
		})
	}
}

func TestAnUnknownTokenIsInvalidAndNotAnError(t *testing.T) {
	plain, _, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// No row: the engine answers the lookup with nothing.
	eng := &recordingEngine{}
	row, state, err := (&Store{Engine: eng}).Resolve(context.Background(), plain, time.Now().UTC())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if row != nil || state != StateInvalid {
		t.Errorf("row=%v state=%q, want (nil, invalid)", row, state)
	}
}

// A malformed presentation is refused WITHOUT a database round trip. Otherwise
// a scanner spraying garbage would be a free query generator.
func TestMalformedTokensNeverReachTheEngine(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-a-token", "mql_pat_" + strings.Repeat("a", 43)} {
		eng := &recordingEngine{}
		_, state, err := (&Store{Engine: eng}).Resolve(context.Background(), bad, time.Now().UTC())
		if err != nil {
			t.Fatalf("Resolve(%q): %v", bad, err)
		}
		if state != StateInvalid {
			t.Errorf("Resolve(%q) state = %q, want invalid", bad, state)
		}
		if len(eng.queries) != 0 {
			t.Errorf("Resolve(%q) hit the engine %d time(s): %v", bad, len(eng.queries), eng.queries)
		}
	}
}

func TestCreateRefusesAnUnboundedToken(t *testing.T) {
	store := &Store{Engine: &recordingEngine{}}
	err := store.Create(context.Background(), "enr-1", testUser, "hash", "issuer", time.Time{}, "")
	if err == nil || !strings.Contains(err.Error(), "expiresAt") {
		t.Fatalf("Create with no expiry: err = %v, want a refusal naming expiresAt", err)
	}
}

func TestLookupSurfacesEngineFailuresRatherThanReportingInvalid(t *testing.T) {
	eng := &recordingEngine{err: errors.New("database down")}
	_, state, err := (&Store{Engine: eng}).Resolve(context.Background(), "mql_enr_"+strings.Repeat("a", 43), time.Now().UTC())
	if err == nil {
		t.Fatal("a database failure must not be reported as an invalid link")
	}
	if state != StateInvalid {
		t.Errorf("state = %q on error, want the conservative invalid", state)
	}
}

func TestCanonicalIdRoundTrips(t *testing.T) {
	if got := CanonicalId("enr-1"); got != canonicalIdPrefix+"enr-1" {
		t.Errorf("CanonicalId = %q", got)
	}
	if got := CanonicalId(canonicalIdPrefix + "enr-1"); got != canonicalIdPrefix+"enr-1" {
		t.Errorf("CanonicalId is not idempotent: %q", got)
	}
	if got := BareSlug(canonicalIdPrefix + "enr-1"); got != "enr-1" {
		t.Errorf("BareSlug = %q", got)
	}
}

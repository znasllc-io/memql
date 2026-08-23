package memql

import (
	"testing"
	"time"
)

// The ownership check on RevokeSessionMsg (memql#4319).
//
// # Why this is worth its own test
//
// `revokeAuthSession` declares (sessionId, revokedReason) and carries no owner
// predicate of its own -- a MemQL mutation cannot hold a `filter`, which is a
// read construct -- and it is not @serverOnly. So the ONLY thing standing
// between a browser and "revoke any session in this cluster by id" is that the
// handler resolves the id against the caller's own set first.
//
// A test that drove the whole handler would be testing a stream. This drives
// the decision, which is the part that must not regress: hand it the caller's
// own sessions and an id, and read what comes back.

func summary(id string, revoked, expires time.Time) *authSessionSummary {
	return &authSessionSummary{ID: id, RevokedAt: revoked, ExpiresAt: expires}
}

func TestPickOwnedLiveSession(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	mine := summary("v1:identity:authSession:mine", time.Time{}, future)
	revoked := summary("v1:identity:authSession:revoked", past, future)
	expired := summary("v1:identity:authSession:expired", time.Time{}, past)
	noExpiry := summary("v1:identity:authSession:forever", time.Time{}, time.Time{})

	own := []*authSessionSummary{mine, revoked, expired, noExpiry, nil}

	cases := []struct {
		name string
		id   string
		want *authSessionSummary
		why  string
	}{
		{
			name: "a live session of the caller's",
			id:   mine.ID,
			want: mine,
			why:  "the ordinary case",
		},
		{
			name: "an id belonging to somebody else",
			id:   "v1:identity:authSession:a-colleagues",
			want: nil,
			why: "THE security case. The list is the caller's own, so an id " +
				"that is not in it is not theirs to end -- and revokeAuthSession " +
				"would happily have written it.",
		},
		{
			name: "an id that names nothing at all",
			id:   "v1:identity:authSession:invented",
			want: nil,
			why: "indistinguishable from the case above, deliberately: two " +
				"answers here would tell an attacker which ids are real",
		},
		{
			name: "a row already revoked",
			id:   revoked.ID,
			want: nil,
			why:  "not a live session; there is nothing to end",
		},
		{
			name: "a row already past its expiry",
			id:   expired.ID,
			want: nil,
			why:  "same -- an expired bearer is not a way in",
		},
		{
			name: "a row with no expiry set",
			id:   noExpiry.ID,
			want: noExpiry,
			why: "a zero expiresAt means 'no expiry recorded', NOT 'expired at " +
				"the epoch' -- reading it as the latter would make every such " +
				"row unrevokable",
		},
		{
			name: "an empty id",
			id:   "",
			want: nil,
			why:  "an empty id must never match a row whose id is also empty",
		},
		{
			name: "an id with surrounding whitespace",
			id:   "  " + mine.ID + "  ",
			want: mine,
			why:  "trimmed at the boundary, so a stray space is not a refusal",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickOwnedLiveSession(own, tc.id, now)
			if got != tc.want {
				t.Fatalf("pickOwnedLiveSession(%q) = %v, want %v\nwhy this matters: %s",
					tc.id, got, tc.want, tc.why)
			}
		})
	}
}

func TestPickOwnedLiveSessionOnAnEmptySet(t *testing.T) {
	// A caller whose session list came back empty owns nothing, so every id is
	// somebody else's. The handler must not fall through to a write.
	if got := pickOwnedLiveSession(nil, "v1:identity:authSession:anything", time.Now()); got != nil {
		t.Fatalf("an empty session set matched an id: %v", got)
	}
}

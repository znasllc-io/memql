package calls

import (
	"strings"
	"testing"
)

func TestHistoryQueryIsAClosedBoundedAllowlist(t *testing.T) {
	req, err := ParseHistoryQuery("archive=1&limit=50&period=90&search=team%20review&role=host&status=cancelled&from=2026-11-01T00%3A00%3A00Z")
	if err != nil || !req.Archive || req.Limit != 50 || req.Period != 90 || req.Search != "team review" || req.Status != "cancelled" {
		t.Fatalf("request = %+v, %v", req, err)
	}
	for _, raw := range []string{"key=secret", "token=secret", "accountId=other", "archive=true", "limit=51", "limit=0", "limit=10&limit=20", "period=365", "role=owner", "status=removed", "cursor=" + strings.Repeat("a", 4097), "search=" + strings.Repeat("a", 257), "search=%zz", "search=one;two"} {
		if _, err := ParseHistoryQuery(raw); err == nil {
			t.Errorf("accepted malformed/unbounded query %q", raw[:min(len(raw), 60)])
		}
	}
}

func TestHistoryTokensSeparatePurposesAndScope(t *testing.T) {
	pepper := []byte("fixture history mac key: generated inputs are never credentials")
	token := SealHistoryToken(pepper, "archive", []byte(`{"key":"one"}`))
	if body, ok := OpenHistoryToken(pepper, "archive", token); !ok || string(body) != `{"key":"one"}` {
		t.Fatal("own token did not verify")
	}
	for _, bad := range []string{token + "a", "changed." + token, strings.Repeat("a", 4097)} {
		if _, ok := OpenHistoryToken(pepper, "archive", bad); ok {
			t.Fatal("tampered token verified")
		}
	}
	if _, ok := OpenHistoryToken(pepper, "cursor", token); ok {
		t.Fatal("archive token verified as a cursor")
	}
	if HistoryDigest(pepper, "entry", "ab", "c") == HistoryDigest(pepper, "entry", "a", "bc") {
		t.Fatal("scope framing collided")
	}
}

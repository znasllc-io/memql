package campaigns

import (
	"strings"
	"testing"
)

// TestRequireUnsubscribeValidatesBaseURL pins memql#3481: the base URL is
// validated, not merely present.
//
// The failure this prevents is a SILENT one. Before the check, any non-empty
// string passed, got interpolated into both the List-Unsubscribe header and
// the body link, and the campaign sent successfully -- with an opt-out the
// mailbox provider declines to honour, because RFC 8058 section 3.1 requires
// an HTTPS URI. Nothing anywhere reported a problem; the first evidence would
// be an unattributable complaint rate.
func TestRequireUnsubscribeValidatesBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		refused bool
		// wantSubstr, when set, must appear in the refusal -- the operator has
		// to be told which variable to fix.
		wantSubstr string
	}{
		{name: "https origin", baseURL: "https://api.example.com"},
		{name: "https origin with trailing slash", baseURL: "https://api.example.com/"},
		{
			// MEMQL_SERVER_PUBLIC_PATH deployments legitimately serve under a path,
			// and unsubscribeURL preserves it.
			name:    "https origin with base path",
			baseURL: "https://api.example.com/app",
		},
		{name: "loopback http by name", baseURL: "http://localhost:8080"},
		{name: "loopback http by v4 literal", baseURL: "http://127.0.0.1:8080"},
		{name: "loopback http by v6 literal", baseURL: "http://[::1]:8080"},
		{
			name:       "plain http on a real host",
			baseURL:    "http://api.example.com",
			refused:    true,
			wantSubstr: "MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL",
		},
		{
			// The trap the issue names: url.Parse accepts this, puts it all in
			// Path, and it would concatenate into a relative URI.
			name:       "no scheme at all",
			baseURL:    "api.example.com",
			refused:    true,
			wantSubstr: "absolute origin",
		},
		{
			name:       "scheme but no host",
			baseURL:    "https://",
			refused:    true,
			wantSubstr: "absolute origin",
		},
		{
			name:       "non-http scheme",
			baseURL:    "ftp://api.example.com",
			refused:    true,
			wantSubstr: "https",
		},
		{
			// unsubscribeURL appends ?token=, so this would produce two.
			name:       "carries a query string",
			baseURL:    "https://api.example.com?utm=1",
			refused:    true,
			wantSubstr: "query string or fragment",
		},
		{
			name:       "carries a fragment",
			baseURL:    "https://api.example.com#x",
			refused:    true,
			wantSubstr: "query string or fragment",
		},
		{
			name:       "unparseable",
			baseURL:    "https://api.example.com/%zz",
			refused:    true,
			wantSubstr: "MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				UnsubscribeSecret:  "a-signing-secret",
				UnsubscribeBaseURL: tc.baseURL,
			}
			reason := cfg.RequireUnsubscribe()

			if tc.refused && reason == "" {
				t.Fatalf("RequireUnsubscribe(%q) allowed the send; want a refusal", tc.baseURL)
			}
			if !tc.refused && reason != "" {
				t.Fatalf("RequireUnsubscribe(%q) refused the send: %s", tc.baseURL, reason)
			}
			if tc.wantSubstr != "" && !strings.Contains(reason, tc.wantSubstr) {
				t.Fatalf("refusal for %q does not mention %q:\n%s", tc.baseURL, tc.wantSubstr, reason)
			}
			// Every refusal must name the variable, or the operator is told
			// something is wrong without being told what to change.
			if reason != "" && !strings.Contains(reason, "MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL") {
				t.Fatalf("refusal for %q does not name the env var:\n%s", tc.baseURL, reason)
			}
		})
	}
}

// TestRequireUnsubscribeAbsentValueMessagesUnchanged guards the two
// preconditions that existed before memql#3481. Adding URL validation must not
// change what an operator sees when a value is simply missing -- those are
// different problems with different fixes, and the secret check has to keep
// running first (a bad URL is no reason to stop reporting an unset key).
func TestRequireUnsubscribeAbsentValueMessagesUnchanged(t *testing.T) {
	t.Run("no secret", func(t *testing.T) {
		cfg := Config{UnsubscribeBaseURL: "https://api.example.com"}
		reason := cfg.RequireUnsubscribe()
		if !strings.Contains(reason, "MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET is not set") {
			t.Fatalf("want the unset-secret message, got: %q", reason)
		}
	})

	t.Run("no base url", func(t *testing.T) {
		cfg := Config{UnsubscribeSecret: "a-signing-secret"}
		reason := cfg.RequireUnsubscribe()
		if !strings.Contains(reason, "MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL is not set") {
			t.Fatalf("want the unset-base-url message, got: %q", reason)
		}
	})

	t.Run("secret is reported before an invalid url", func(t *testing.T) {
		cfg := Config{UnsubscribeBaseURL: "http://api.example.com"}
		reason := cfg.RequireUnsubscribe()
		if !strings.Contains(reason, "MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET is not set") {
			t.Fatalf("want the unset-secret message to win, got: %q", reason)
		}
	})
}

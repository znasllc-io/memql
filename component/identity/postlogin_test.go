package identity

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The post-login destination is a value the browser hands back to a
// page that is about to redirect a JUST-AUTHENTICATED user. An open
// redirect there is worth more to an attacker than almost anywhere
// else in the tree, so the same-origin guard gets its own test rather
// than riding on the caller's.

func TestSafeRelativeRedirectAcceptsFirstPartyPaths(t *testing.T) {
	for _, in := range []string{
		"/device",
		"/device?user_code=ABCD-2345",
		"/me/tokens",
		"/a/b/c?x=1&y=2#frag",
	} {
		if got := SafeRelativeRedirect(in); got != in {
			t.Fatalf("SafeRelativeRedirect(%q) = %q, want it accepted unchanged", in, got)
		}
	}
}

func TestSafeRelativeRedirectRejectsEverythingElse(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"absolute https":       "https://evil.test/steal",
		"absolute http":        "http://evil.test/steal",
		"protocol relative":    "//evil.test/steal",
		"backslash host":       "/\\evil.test",
		"scheme only":          "javascript:alert(1)",
		"data url":             "data:text/html,<script>",
		"bare relative":        "device",
		"crlf header split":    "/device\r\nLocation: https://evil.test",
		"newline":              "/device\nSet-Cookie: x=y",
		"null byte":            "/device\x00",
		"whitespace then host": "  https://evil.test",
	}
	for name, in := range cases {
		if got := SafeRelativeRedirect(in); got != "" {
			t.Fatalf("%s: SafeRelativeRedirect(%q) = %q, want a rejection", name, in, got)
		}
	}
}

func TestPostLoginRedirectRoundTrip(t *testing.T) {
	const dest = "/device?user_code=ABCD-2345"

	set := httptest.NewRecorder()
	SetPostLoginRedirect(set, dest, true)
	cookies := set.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly one cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != PostLoginCookieName || c.Value != dest {
		t.Fatalf("cookie = %s=%q, want %s=%q", c.Name, c.Value, PostLoginCookieName, dest)
	}
	if !c.HttpOnly {
		t.Error("the post-login cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("secure=true must set the Secure flag")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax -- the magic-link click is a cross-site top-level GET", c.SameSite)
	}

	// Read it back, and confirm the read CLEARS it: a destination that
	// survived one sign-in would fire again on the next unrelated one.
	req := httptest.NewRequest(http.MethodGet, "/auth/complete", nil)
	req.AddCookie(c)
	take := httptest.NewRecorder()
	if got := TakePostLoginRedirect(take, req); got != dest {
		t.Fatalf("TakePostLoginRedirect = %q, want %q", got, dest)
	}
	cleared := take.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("the read did not clear the cookie: %+v", cleared)
	}
}

func TestSetPostLoginRedirectRefusesAnUnsafeDestination(t *testing.T) {
	rec := httptest.NewRecorder()
	SetPostLoginRedirect(rec, "https://evil.test/steal", true)
	if got := rec.Result().Cookies(); len(got) != 0 {
		t.Fatalf("an off-origin destination was stored: %+v", got)
	}
}

// The cookie is client-held, so the value is re-validated on READ and
// not merely on write. A tampered cookie yields nothing.
func TestTakePostLoginRedirectRevalidatesOnRead(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/auth/complete", nil)
	req.AddCookie(&http.Cookie{Name: PostLoginCookieName, Value: "https://evil.test/steal"})
	rec := httptest.NewRecorder()
	if got := TakePostLoginRedirect(rec, req); got != "" {
		t.Fatalf("a tampered cookie produced the destination %q", got)
	}
}

func TestTakePostLoginRedirectWithNoCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/complete", nil)
	if got := TakePostLoginRedirect(rec, req); got != "" {
		t.Fatalf("got %q with no cookie present", got)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a clearing cookie was emitted when there was nothing to clear")
	}
}

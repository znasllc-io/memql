package campaigns

import (
	"net/url"
	"strings"
	"testing"
)

// links_test.go -- the URL builders (memql#6033).
//
// The properties that matter are that every link is built from the origin the
// unsubscribe link has always used, that the token in a URL is the one for THAT
// page (a link for one page does not open another), and that the unsubscribe
// URL did not change shape when it moved behind this seam.

func linkTestConfig(base string) Config {
	return Config{UnsubscribeSecret: linkSecretA, UnsubscribeBaseURL: base}
}

// tokenFromLinkURL checks the URL is exactly <prefix>?token=<escaped token> and
// returns the token a mail client would hand back to the server: the decoded
// query value.
func tokenFromLinkURL(t *testing.T, raw, prefix string) string {
	t.Helper()
	if !strings.HasPrefix(raw, prefix+"?token=") {
		t.Fatalf("link %q does not start %q", raw, prefix+"?token=")
	}
	if strings.Count(raw, "?") != 1 || strings.Contains(raw, "#") {
		t.Fatalf("link %q has more than one query or a fragment", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("link %q does not parse: %v", raw, err)
	}
	if len(parsed.Query()) != 1 {
		t.Fatalf("link %q carries more than the token: %v", raw, parsed.Query())
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("link %q has an empty token", raw)
	}
	return token
}

// TestLinkURLsUseTheUnsubscribeOrigin -- one origin for every link in a
// message, and no doubled slash where a trailing one was configured.
func TestLinkURLsUseTheUnsubscribeOrigin(t *testing.T) {
	cfg := linkTestConfig("https://api.example.test/")
	p := fullLinkPayload()
	keys := cfg.UnsubscribeKeys()

	cases := []struct {
		name  string
		build func(Config, LinkPayload) (string, error)
		path  string
		kind  LinkKind
	}{
		{"confirm", ConfirmURL, "/email/confirm", LinkConfirm},
		{"preferences", PreferencesURL, "/email/preferences", LinkPreferences},
		{"view", ViewURL, "/email/view", LinkView},
	}
	for _, tc := range cases {
		got, err := tc.build(cfg, p)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		token := tokenFromLinkURL(t, got, "https://api.example.test"+tc.path)
		if strings.Contains(strings.TrimPrefix(got, "https://"), "//") {
			t.Errorf("%s: %q has a doubled slash", tc.name, got)
		}
		// What the server will do with the link: read the token back with the
		// ring and the page's own kind.
		back, err := ParseLinkToken(keys, tc.kind, token, linkTestNow)
		if err != nil {
			t.Errorf("%s: the token in %q does not verify for its own page: %v", tc.name, got, err)
		}
		if back != p {
			t.Errorf("%s: the link carries %+v, want %+v", tc.name, back, p)
		}
	}

	// The path constants are the routes the pages are served from.
	if ConfirmPath != "/email/confirm" || PreferencesPath != "/email/preferences" || ViewPath != "/email/view" {
		t.Errorf("paths are %q %q %q", ConfirmPath, PreferencesPath, ViewPath)
	}
}

// TestLinkURLsKeepTheConfiguredBase -- UnsubscribeBaseURL may carry a path (a
// deployment served under a prefix), and surrounding space or any number of
// trailing slashes are the operator's typing, not part of the origin.
func TestLinkURLsKeepTheConfiguredBase(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.example.test":      "https://api.example.test",
		"https://api.example.test/":     "https://api.example.test",
		"https://api.example.test///":   "https://api.example.test",
		"  https://api.example.test/  ": "https://api.example.test",
		"https://api.example.test/app":  "https://api.example.test/app",
		"https://api.example.test/app/": "https://api.example.test/app",
		"http://127.0.0.1:8080/":        "http://127.0.0.1:8080",
	} {
		cfg := linkTestConfig(base)
		for path, build := range map[string]func(Config, LinkPayload) (string, error){
			ConfirmPath: ConfirmURL, PreferencesPath: PreferencesURL, ViewPath: ViewURL,
		} {
			got, err := build(cfg, fullLinkPayload())
			if err != nil {
				t.Fatalf("base %q, %s: %v", base, path, err)
			}
			tokenFromLinkURL(t, got, want+path)
		}
	}
}

// TestLinkURLEscapesWhateverTokenItIsGiven -- real tokens are URL-safe by
// construction, so escaping is a no-op for them and no end-to-end test can see
// it. The helper takes any token (an inert placeholder, for one), and what it
// is given must never be able to add a parameter or end the query.
func TestLinkURLEscapesWhateverTokenItIsGiven(t *testing.T) {
	got := linkURL("https://api.example.test/", "/email/view", "a b&c=d/e?f#g")
	const want = "https://api.example.test/email/view?token=a+b%26c%3Dd%2Fe%3Ff%23g"
	if got != want {
		t.Errorf("linkURL:\n got %s\nwant %s", got, want)
	}
}

// TestEachLinkURLCarriesATokenForItsOwnPage -- the token in a confirm URL opens
// the confirm page and no other, and likewise for the others.
func TestEachLinkURLCarriesATokenForItsOwnPage(t *testing.T) {
	cfg := linkTestConfig("https://api.example.test")
	keys := cfg.UnsubscribeKeys()
	built := map[LinkKind]string{}
	for kind, build := range map[LinkKind]func(Config, LinkPayload) (string, error){
		LinkConfirm: ConfirmURL, LinkPreferences: PreferencesURL, LinkView: ViewURL,
	} {
		raw, err := build(cfg, fullLinkPayload())
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		built[kind] = raw
	}
	for minted, raw := range built {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", minted, err)
		}
		for _, presented := range linkKinds {
			_, err := ParseLinkToken(keys, presented, parsed.Query().Get("token"), linkTestNow)
			if (err == nil) != (presented == minted) {
				t.Errorf("the %s URL's token at the %s page: err = %v", minted, presented, err)
			}
		}
	}
}

// TestLinkURLsAreSignedByTheCurrentSecretOnly -- during a rotation the new
// secret signs; the previous one only verifies.
func TestLinkURLsAreSignedByTheCurrentSecretOnly(t *testing.T) {
	rotated := Config{UnsubscribeSecret: linkSecretB, UnsubscribeSecretPrevious: linkSecretA, UnsubscribeBaseURL: "https://api.example.test"}
	raw, err := PreferencesURL(rotated, fullLinkPayload())
	if err != nil {
		t.Fatal(err)
	}
	token := tokenFromLinkURL(t, raw, "https://api.example.test"+PreferencesPath)
	if id := splitLinkToken(t, token)[1]; id != LinkKeyID(linkSecretB) {
		t.Errorf("the link names key %q, want the current secret's %q", id, LinkKeyID(linkSecretB))
	}
	if _, err := ParseLinkToken([]string{linkSecretA}, LinkPreferences, token, linkTestNow); err == nil {
		t.Error("the PREVIOUS secret alone verified a link signed with the current one")
	}
}

// TestUnsubscribeURLKeepsItsFormat -- the one-click link behind the
// List-Unsubscribe header is the same string it was before it moved behind this
// seam: origin, /unsubscribe, ?token=, and the u3 token escaped for a query.
func TestUnsubscribeURLKeepsItsFormat(t *testing.T) {
	payload := UnsubscribePayload{
		OwnerUserID: "owner-1", RecipientID: "recipient-1", CampaignID: "campaign-1",
		AccountID: "account-1", EmailDigest: linkTestDigest,
	}
	for _, base := range []string{"https://api.example.test", "https://api.example.test/", "  https://api.example.test/  "} {
		cfg := linkTestConfig(base)
		token, err := MintUnsubscribeToken(linkSecretA, payload)
		if err != nil {
			t.Fatalf("MintUnsubscribeToken: %v", err)
		}
		want := "https://api.example.test" + "/unsubscribe?token=" + url.QueryEscape(token)

		got, err := UnsubscribeURL(cfg, payload)
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		if got != want {
			t.Errorf("base %q:\n got %s\nwant %s", base, got, want)
		}
	}

	// The token in it is an unsubscribe token the existing endpoint accepts.
	got, err := UnsubscribeURL(linkTestConfig("https://api.example.test"), payload)
	if err != nil {
		t.Fatal(err)
	}
	token := tokenFromLinkURL(t, got, "https://api.example.test"+UnsubscribePath)
	back, err := ParseUnsubscribeToken([]string{linkSecretA}, token)
	if err != nil {
		t.Fatalf("the unsubscribe endpoint would refuse the link: %v", err)
	}
	if back.OwnerUserID != payload.OwnerUserID || back.RecipientID != payload.RecipientID || back.EmailDigest != payload.EmailDigest {
		t.Errorf("the link carries %+v, want %+v", back, payload)
	}

	// And a link token is not an unsubscribe token: the two cannot stand in for each other.
	viewLink, err := ViewURL(linkTestConfig("https://api.example.test"), fullLinkPayload())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseUnsubscribeToken([]string{linkSecretA}, tokenFromLinkURL(t, viewLink, "https://api.example.test"+ViewPath)); err == nil {
		t.Error("a view link's token verified at the unsubscribe endpoint")
	}
}

// TestLinkURLBuildersRefuseWhatMintingRefuses -- an incomplete payload or a
// missing secret is an error and an empty URL, never a link to nowhere.
func TestLinkURLBuildersRefuseWhatMintingRefuses(t *testing.T) {
	cfg := linkTestConfig("https://api.example.test")
	noSecret := Config{UnsubscribeBaseURL: "https://api.example.test"}

	incomplete := func(edit func(*LinkPayload)) LinkPayload {
		p := fullLinkPayload()
		edit(&p)
		return p
	}
	cases := map[string]func() (string, error){
		"confirm with no expiry": func() (string, error) { return ConfirmURL(cfg, incomplete(func(p *LinkPayload) { p.ExpiresAt = 0 })) },
		"confirm with no recipient": func() (string, error) {
			return ConfirmURL(cfg, incomplete(func(p *LinkPayload) { p.RecipientID = "" }))
		},
		"preferences with no owner": func() (string, error) {
			return PreferencesURL(cfg, incomplete(func(p *LinkPayload) { p.OwnerUserID = "" }))
		},
		"preferences with no digest": func() (string, error) {
			return PreferencesURL(cfg, incomplete(func(p *LinkPayload) { p.EmailDigest = "" }))
		},
		"view with no template": func() (string, error) { return ViewURL(cfg, incomplete(func(p *LinkPayload) { p.TemplateID = "" })) },
		"view with no revision": func() (string, error) {
			return ViewURL(cfg, incomplete(func(p *LinkPayload) { p.TemplateRevision = "" }))
		},
		"confirm with no secret":     func() (string, error) { return ConfirmURL(noSecret, fullLinkPayload()) },
		"preferences with no secret": func() (string, error) { return PreferencesURL(noSecret, fullLinkPayload()) },
		"view with no secret":        func() (string, error) { return ViewURL(noSecret, fullLinkPayload()) },
		"unsubscribe with no secret": func() (string, error) {
			return UnsubscribeURL(noSecret, UnsubscribePayload{OwnerUserID: "o", RecipientID: "r"})
		},
		"unsubscribe with no owner":     func() (string, error) { return UnsubscribeURL(cfg, UnsubscribePayload{RecipientID: "r"}) },
		"unsubscribe with no recipient": func() (string, error) { return UnsubscribeURL(cfg, UnsubscribePayload{OwnerUserID: "o"}) },
	}
	for name, build := range cases {
		got, err := build()
		if err == nil {
			t.Errorf("%s: built %q anyway", name, got)
		}
		if got != "" {
			t.Errorf("%s: returned a URL alongside the error: %q", name, got)
		}
	}
}

// TestLinkURLBuildersRefuseABlankOrigin -- with no origin a link would be a
// relative path like /email/confirm?token=..., which no mail client can open. It
// is an error, not a link. An origin that is only slashes is no origin either.
func TestLinkURLBuildersRefuseABlankOrigin(t *testing.T) {
	unsubscribe := UnsubscribePayload{OwnerUserID: "owner-1", RecipientID: "recipient-1", EmailDigest: linkTestDigest}
	builders := map[string]func(Config) (string, error){
		"confirm":     func(cfg Config) (string, error) { return ConfirmURL(cfg, fullLinkPayload()) },
		"preferences": func(cfg Config) (string, error) { return PreferencesURL(cfg, fullLinkPayload()) },
		"view":        func(cfg Config) (string, error) { return ViewURL(cfg, fullLinkPayload()) },
		"unsubscribe": func(cfg Config) (string, error) { return UnsubscribeURL(cfg, unsubscribe) },
	}

	// Control: with an origin each builder builds, so the refusals below are
	// about the origin and nothing else in the fixtures.
	for name, build := range builders {
		if got, err := build(linkTestConfig("https://api.example.test")); err != nil || got == "" {
			t.Fatalf("control: %s with an origin: %q, %v", name, got, err)
		}
	}

	for _, base := range []string{"", "   ", "\t\n", "/", "///", "  /  "} {
		for name, build := range builders {
			got, err := build(linkTestConfig(base))
			if err == nil {
				t.Errorf("%s with the origin %q: built %q", name, base, got)
				continue
			}
			if got != "" {
				t.Errorf("%s with the origin %q: returned a URL alongside the error: %q", name, base, got)
			}
			if !strings.Contains(err.Error(), "base URL") {
				t.Errorf("%s with the origin %q: the error does not name the base URL: %v", name, base, err)
			}
		}
	}
}

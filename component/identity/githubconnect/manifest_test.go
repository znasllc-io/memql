package githubconnect

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// manifest_test.go -- what this cluster asks GitHub to create (design record
// 2026-09-20-github-app-setup, D3 and D5). The manifest is the whole of what an
// owner is asked to trust on GitHub's confirmation page, so every field is
// pinned, and so is the ABSENCE of the ones that would widen it.

func TestTheManifestIsTheRegistrationTheOperatorGuideDescribes(t *testing.T) {
	m, err := BuildManifest("lab.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.URL != "https://os.lab.example.com" {
		t.Errorf("homepage = %q", m.URL)
	}
	if got := strings.Join(m.CallbackURLs, ","); got != "https://identity.lab.example.com/auth/github/callback" {
		t.Errorf("callback URLs = %q; ONE callback, the route the Connect design approved", got)
	}
	if m.RedirectURL != "https://identity.lab.example.com/auth/github/app/callback" {
		t.Errorf("redirect URL = %q", m.RedirectURL)
	}
	if !m.RequestOAuthOnInstall {
		t.Error("authorization is not requested during installation, so an install would land nobody's grant")
	}
	if !m.Public {
		t.Error("the app is private: it could be installed only on the account that owns it, and 'install on another organization' would work for nobody")
	}
	if !m.HookAttributes.Active || m.HookAttributes.URL != "https://api.lab.example.com/inbound/github" {
		t.Errorf("webhook = %+v, want active at the inbound seam", m.HookAttributes)
	}
	if strings.Join(m.DefaultEvents, ",") != "push" {
		t.Errorf("events = %v, want pushes and nothing else", m.DefaultEvents)
	}
}

// TestTheAskIsContentsAndMetadataReadAndNothingElse is decision C8 of the
// Connect design, restated where a regression would land: on the manifest.
func TestTheAskIsContentsAndMetadataReadAndNothingElse(t *testing.T) {
	m, err := BuildManifest("lab.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.DefaultPermissions) != 2 || m.DefaultPermissions["contents"] != "read" || m.DefaultPermissions["metadata"] != "read" {
		t.Fatalf("permissions = %v", m.DefaultPermissions)
	}
	for name, level := range m.DefaultPermissions {
		if level != "read" {
			t.Errorf("%s is asked at %q", name, level)
		}
	}
}

// TestTheManifestCarriesExactlyTheseKeys reads the JSON GitHub reads. A key
// added here is a change to what an owner is asked to approve, and it should
// not arrive as a side effect of a struct edit. `setup_url` is named because
// GitHub does not accept one alongside request_oauth_on_install.
func TestTheManifestCarriesExactlyTheseKeys(t *testing.T) {
	for domain, want := range map[string][]string{
		"lab.example.com": {"callback_urls", "default_events", "default_permissions", "description", "hook_attributes", "name", "public", "redirect_url", "request_oauth_on_install", "url"},
		"memql.localhost": {"callback_urls", "default_permissions", "description", "hook_attributes", "name", "public", "redirect_url", "request_oauth_on_install", "url"},
	} {
		m, err := BuildManifest(domain, "")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := m.JSON()
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatal(err)
		}
		var got []string
		for k := range decoded {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: keys = %v, want %v", domain, got, want)
		}
		if _, has := decoded["setup_url"]; has {
			t.Errorf("%s: the manifest carries a setup_url, which GitHub refuses alongside request_oauth_on_install", domain)
		}
	}
}

// TestALocalClusterRegistersItsWebhookOff: GitHub cannot reach a laptop. The
// app still registers -- with nothing to deliver and nowhere it could go.
func TestALocalClusterRegistersItsWebhookOff(t *testing.T) {
	m, err := BuildManifest("memql.localhost", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.HookAttributes.Active {
		t.Error("a webhook GitHub cannot deliver was registered active")
	}
	if strings.Contains(m.HookAttributes.URL, "localhost") {
		t.Errorf("webhook URL = %q; the manifest must not depend on GitHub accepting a name that never resolves publicly", m.HookAttributes.URL)
	}
	if len(m.DefaultEvents) != 0 {
		t.Errorf("events = %v on an app whose webhook is off", m.DefaultEvents)
	}
	// Everything a BROWSER follows is still this cluster's own: those redirects
	// happen in the person's browser, which can reach a laptop.
	if m.CallbackURLs[0] != "https://identity.memql.localhost/auth/github/callback" || m.URL != "https://os.memql.localhost" {
		t.Errorf("callback = %v homepage = %q", m.CallbackURLs, m.URL)
	}
}

// TestTheCallbackIsTheOneConnectAsksFor. GitHub matches an authorization's
// redirect_uri against the callback registered here. Both come from
// RedirectURI over the identity service's own base URL, so they cannot differ
// -- including on a cluster whose identity host is not `identity.<domain>`.
func TestTheCallbackIsTheOneConnectAsksFor(t *testing.T) {
	const base = "https://login.corp.example.com/"
	m, err := BuildManifest("lab.example.com", base)
	if err != nil {
		t.Fatal(err)
	}
	if m.CallbackURLs[0] != RedirectURI(base) {
		t.Errorf("callback = %q, and Connect will send redirect_uri = %q", m.CallbackURLs[0], RedirectURI(base))
	}
	if m.RedirectURL != "https://login.corp.example.com/auth/github/app/callback" {
		t.Errorf("redirect URL = %q", m.RedirectURL)
	}
	// What is NOT the identity service's still comes from the domain.
	if m.URL != "https://os.lab.example.com" || m.HookAttributes.URL != "https://api.lab.example.com/inbound/github" {
		t.Errorf("homepage = %q webhook = %q", m.URL, m.HookAttributes.URL)
	}
}

func TestTheAppNameFitsGitHubsLimit(t *testing.T) {
	for _, domain := range []string{"memql.localhost", "a-very-long-subdomain.of.an.equally-long-company.example.com"} {
		m, err := BuildManifest(domain, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Name) > maxAppNameLen || !strings.HasPrefix(m.Name, "MemQL on ") {
			t.Errorf("name = %q (%d characters)", m.Name, len(m.Name))
		}
		if strings.HasSuffix(m.Name, ".") || strings.HasSuffix(m.Name, "-") {
			t.Errorf("name = %q was cut mid-separator", m.Name)
		}
	}
}

func TestADomainThatIsNotAHostnameIsRefused(t *testing.T) {
	for _, domain := range []string{"", "   ", "https://example.com", "example.com/path", "user@example.com", "example.com:8443"} {
		if _, err := BuildManifest(domain, ""); err == nil {
			t.Errorf("BuildManifest(%q) composed a manifest", domain)
		}
	}
}

func TestOnlyANameThatCanResolvePubliclyGetsAnActiveWebhook(t *testing.T) {
	for domain, want := range map[string]bool{
		// `example.com` is a real, resolvable name; it is the `.example` TLD
		// further down that RFC 2606 takes out of the public tree.
		"lab.example.com":  true,
		"memql.io":         true,
		"cluster.acme.dev": true,
		"memql.localhost":  false,
		"localhost":        false,
		"cluster":          false,
		"studio.local":     false,
		"ci.test":          false,
		"lab.example":      false,
		"nothing.invalid":  false,
		"memql.internal":   false,
		"box.lan":          false,
		"nas.home.arpa":    false,
		"10.0.0.4":         false,
		"  Memql.IO.  ":    true,
	} {
		if got := DomainIsPubliclyReachable(domain); got != want {
			t.Errorf("DomainIsPubliclyReachable(%q) = %v, want %v", domain, got, want)
		}
	}
}

func TestAnOrganizationIsAGitHubLoginOrNothing(t *testing.T) {
	for login, want := range map[string]bool{
		"":                      true, // the person's own account
		"znasllc-io":            true,
		"acme":                  true,
		"A1":                    true,
		"-acme":                 false,
		"acme-":                 false,
		"ac--me":                false,
		"acme/evil":             false,
		"acme?x=1":              false,
		"../settings":           false,
		"acme corp":             false,
		strings.Repeat("a", 39): true,
		strings.Repeat("a", 40): false,
	} {
		if got := ValidOrganization(login); got != want {
			t.Errorf("ValidOrganization(%q) = %v, want %v", login, got, want)
		}
	}
}

func TestWhereTheManifestIsPosted(t *testing.T) {
	if got := ManifestPostURL("", "", "abc"); got != "https://github.com/settings/apps/new?state=abc" {
		t.Errorf("own account: %q", got)
	}
	if got := ManifestPostURL("https://github.com/", "znasllc-io", "abc"); got != "https://github.com/organizations/znasllc-io/settings/apps/new?state=abc" {
		t.Errorf("organization: %q", got)
	}
	// A state is random bytes, base64url today -- and escaped all the same.
	if got := ManifestPostURL("", "", "a b&c=d"); !strings.HasSuffix(got, "?state=a+b%26c%3Dd") {
		t.Errorf("state was not escaped: %q", got)
	}
	// Defence in depth: ValidOrganization would refuse this, and the URL does
	// not rely on that having happened.
	if got := ManifestPostURL("", "a/../b", "s"); strings.Contains(got, "/../") {
		t.Errorf("an organization segment escaped its path: %q", got)
	}
}

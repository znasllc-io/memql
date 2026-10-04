package githubconnect

import (
	"encoding/json"
	"reflect"
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
	if got := strings.Join(m.DefaultEvents, ","); got != "check_run,merge_group,pull_request,push,release" {
		t.Errorf("events = %v, want the deliveries the update cue and pipelines read, and nothing else", m.DefaultEvents)
	}
	// The sentence GitHub shows for the app, so it says everything the app
	// does: it reads, and it reports checks.
	if m.Description != "Lets the MemQL cluster at lab.example.com read the repositories you choose and report checks on them." {
		t.Errorf("description = %q", m.Description)
	}
}

// TestTheAskIsReadsAndCheckWritesAndNothingElse is decision C8 of the Connect
// design as pipelines widened it (pipelines record, D4), restated where a
// regression would land: on the manifest. Every permission is read except
// checks, the one write, which is how a run reports back.
func TestTheAskIsReadsAndCheckWritesAndNothingElse(t *testing.T) {
	m, err := BuildManifest("lab.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"checks":        "write",
		"contents":      "read",
		"merge_queues":  "read",
		"metadata":      "read",
		"pull_requests": "read",
	}
	if !reflect.DeepEqual(m.DefaultPermissions, want) {
		t.Fatalf("permissions = %v, want %v", m.DefaultPermissions, want)
	}
	for name, level := range m.DefaultPermissions {
		if level != "read" && name != "checks" {
			t.Errorf("%s is asked at %q", name, level)
		}
	}
}

// TestEveryEventIsOneTheAskCanSubscribeTo. GitHub lets an app subscribe to an
// event only while it holds that event's permission (merge_group needs merge
// queues read, which is why the ask carries it). The table is GitHub's, from
// its webhook reference; an event added to the manifest without its
// permission fails here rather than on GitHub's page, where the owner can fix
// nothing.
func TestEveryEventIsOneTheAskCanSubscribeTo(t *testing.T) {
	needs := map[string]string{
		"check_run":    "checks",
		"merge_group":  "merge_queues",
		"pull_request": "pull_requests",
		"push":         "contents",
		"release":      "contents",
	}
	m, err := BuildManifest("lab.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.DefaultEvents) == 0 {
		t.Fatal("no events on a public cluster, so the loop below would pass on nothing")
	}
	for _, event := range m.DefaultEvents {
		permission, known := needs[event]
		if !known {
			t.Errorf("the manifest subscribes to %q, and this table does not say which permission GitHub requires for it", event)
			continue
		}
		if level := m.DefaultPermissions[permission]; level != "read" && level != "write" {
			t.Errorf("%q needs %s, which the manifest asks at %q", event, permission, level)
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
	// THE ASK IS THE SAME. A local cluster polls instead of receiving, and still
	// reports a check run on what it polled.
	if !reflect.DeepEqual(m.DefaultPermissions, RequestedPermissions()) {
		t.Errorf("permissions = %v on a local cluster, want the whole ask %v", m.DefaultPermissions, RequestedPermissions())
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

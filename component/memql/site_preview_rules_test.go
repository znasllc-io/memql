package memql

import (
	"strings"
	"testing"
)

// site_preview_rules_test.go -- the four pure rules of storefront preview
// (epic memql#5531), on a table of values.
//
// THE RULES ARE FUNCTIONS OVER VALUES, so every case here is a property of a
// function rather than a count against a mock. The guard
// (platform_site_preview_guard.go) and the readiness capability both call
// these, which is why they are tested once, here, and not twice.

// readable is a store somebody could read: the answer to "is it a development
// store" is the boolean. It carries a Storefront token, because a store without
// one is not connected and refuses go-live on that ground alone -- see
// connectedButTokenless for that case.
func readableStore(id, domain string, development bool) PreviewBoundStore {
	return PreviewBoundStore{ID: id, Readable: true, IsDevelopment: development, Domain: domain, HasStorefrontToken: true}
}

// tokenlessStore is a store somebody could read, that is not a development
// store, and that has no Storefront token: a store row with nothing to serve a
// catalog with (Connect Shopify, D5).
func tokenlessStore(id, domain string) PreviewBoundStore {
	return PreviewBoundStore{ID: id, Readable: true, Domain: domain}
}

// unreadableStore is the third state: the store row did not come back. Its
// IsDevelopment field is deliberately false, because a rule that read it would
// be reading the zero value of a question nobody answered.
func unreadableStore(id string) PreviewBoundStore {
	return PreviewBoundStore{ID: id, Readable: false}
}

func TestStorefrontDestinationBindingRules(t *testing.T) {
	for _, store := range []PreviewBoundStore{
		{}, {ID: "   "}, readableStore("sandbox", "sandbox.myshopify.com", true),
		readableStore("live", "live.myshopify.com", false), tokenlessStore("setup", "setup.myshopify.com"),
	} {
		for _, rule := range []func(bool, PreviewBoundStore) PreviewRefusal{SiteGoLiveRefusal, SitePreviewBindingRefusal} {
			if refusal := rule(true, store); !refusal.Empty() {
				t.Fatalf("destination rejected a readable store or design preview: %+v", refusal)
			}
		}
	}
	for _, rule := range []func(bool, PreviewBoundStore) PreviewRefusal{SiteGoLiveRefusal, SitePreviewBindingRefusal} {
		store := unreadableStore("private")
		if refusal := rule(true, store); refusal.Code != PreviewRefusalStoreUnreadable || !strings.Contains(refusal.Message, "private") {
			t.Fatalf("unreadable binding not refused: %+v", refusal)
		}
		if refusal := rule(false, store); !refusal.Empty() {
			t.Fatal("store rule applied to non-storefront")
		}
	}
}

func TestSiteCandidateRefusal(t *testing.T) {
	for _, tc := range []struct {
		name               string
		serving, candidate string
		wantCode           string
		why                string
	}{
		{
			name:    "a candidate equal to the serving version is refused",
			serving: "blob://sites/s/v2/", candidate: "blob://sites/s/v2/",
			wantCode: PreviewRefusalCandidateIsServing,
			why: "The preview would show exactly what the public already sees, and the " +
				"promotion would be a write that changes nothing while reading like a release.",
		},
		{
			name:    "a different candidate passes",
			serving: "blob://sites/s/v2/", candidate: "blob://sites/s/v3/",
			wantCode: "",
			why:      "The ordinary candidate.",
		},
		{
			name:    "an empty candidate is the cleared state and passes",
			serving: "blob://sites/s/v2/", candidate: "",
			wantCode: "",
			why:      "Withdrawing a candidate must stay expressible; clearing is not setting.",
		},
		{
			name:    "an empty serving version refuses nothing",
			serving: "", candidate: "blob://sites/s/v1/",
			wantCode: "",
			why: "A deployable that has never published serves nothing, so its first " +
				"candidate cannot be the version already serving.",
		},
		{
			name:    "both empty refuses nothing",
			serving: "", candidate: "",
			wantCode: "",
			why:      "There is no comparison to make.",
		},
		{
			name:    "whitespace does not make two identical versions different",
			serving: "blob://sites/s/v2/", candidate: "  blob://sites/s/v2/  ",
			wantCode: PreviewRefusalCandidateIsServing,
			why: "The values arrive off a row and out of a request; a padded copy is the " +
				"same version, and reading it as a different one is the refusal silently lost.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SiteCandidateRefusal(tc.serving, tc.candidate)
			if got.Code != tc.wantCode {
				t.Fatalf("SiteCandidateRefusal(%q, %q) = %q, want %q\n%s",
					tc.serving, tc.candidate, got.Code, tc.wantCode, tc.why)
			}
		})
	}
}

func TestSitePromotionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stored, named string
		wantCode      string
		why           string
	}{
		{
			name:   "no stored candidate is nothing to promote",
			stored: "", named: "blob://sites/s/v3/",
			wantCode: PreviewRefusalNoCandidate,
			why:      "A promotion of nothing would move the serving version to a value nobody set.",
		},
		{
			name:   "a named candidate differing from the stored one is refused",
			stored: "blob://sites/s/v4/", named: "blob://sites/s/v3/",
			wantCode: PreviewRefusalCandidateMoved,
			why: "The candidate was republished between the reading and the click, so " +
				"promoting would put a version nobody looked at in front of shoppers -- " +
				"the artifact-hash property the work spine's approvals carry.",
		},
		{
			name:   "a named candidate equal to the stored one passes",
			stored: "blob://sites/s/v4/", named: "blob://sites/s/v4/",
			wantCode: "",
			why:      "The ordinary promotion.",
		},
		{
			name:   "an empty name against a stored candidate passes",
			stored: "blob://sites/s/v4/", named: "",
			wantCode: "",
			why: "THE READINESS CALL. It asks 'could anything be promoted' without naming " +
				"one, so an empty name must not read as a mismatch -- that would report " +
				"every ready deployable as moved and hide the control that works.",
		},
		{
			name:   "both empty is still no candidate",
			stored: "", named: "",
			wantCode: PreviewRefusalNoCandidate,
			why:      "The readiness call on a deployable with nothing staged.",
		},
		{
			name:   "whitespace does not make the named candidate a different one",
			stored: "blob://sites/s/v4/", named: " blob://sites/s/v4/ ",
			wantCode: "",
			why:      "A padded copy is the same version; refusing it would refuse the promotion it names.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SitePromotionRefusal(tc.stored, tc.named)
			if got.Code != tc.wantCode {
				t.Fatalf("SitePromotionRefusal(%q, %q) = %q, want %q\n%s",
					tc.stored, tc.named, got.Code, tc.wantCode, tc.why)
			}
		})
	}
}

// EVERY REFUSAL CARRIES A REMEDY, asserted as a PROPERTY over all of them
// rather than case by case -- a per-case assertion is one somebody forgets on
// the next code, and the whole reason Remedy is a field is that a refusal
// naming only what is wrong leaves an operator on a screen with a missing
// button and nothing to do (issue memql#5546).
func TestEveryPreviewRefusalCarriesAMessageAndARemedy(t *testing.T) {
	refusals := []PreviewRefusal{
		SiteGoLiveRefusal(true, unreadableStore("v1:shopify:store:gone")),
		SitePreviewBindingRefusal(true, unreadableStore("v1:shopify:store:gone")),
		SiteCandidateRefusal("blob://sites/s/v2/", "blob://sites/s/v2/"),
		SitePromotionRefusal("", "blob://sites/s/v3/"),
		SitePromotionRefusal("blob://sites/s/v4/", "blob://sites/s/v3/"),
	}

	seen := map[string]bool{}
	for _, r := range refusals {
		if r.Empty() {
			t.Fatalf("the corpus carries a non-refusal (%+v), so the property below is asserted "+
				"over fewer refusals than it claims", r)
		}
		seen[r.Code] = true
		if strings.TrimSpace(r.Message) == "" {
			t.Errorf("%s carries no message", r.Code)
		}
		if strings.TrimSpace(r.Remedy) == "" {
			t.Errorf("%s carries no remedy -- the act that clears a refusal is part of the answer", r.Code)
		}
	}

	// THE CORPUS IS THE COVERAGE CLAIM, so it says what it covers. Every code
	// these four functions can produce appears above; PreviewRefusalSystemOwned
	// is deliberately absent, because no rule in this file produces it -- the
	// guard refuses a systemOwned site in its own words.
	for _, code := range []string{
		PreviewRefusalStoreUnreadable,
		PreviewRefusalCandidateIsServing,
		PreviewRefusalNoCandidate,
		PreviewRefusalCandidateMoved,
	} {
		if !seen[code] {
			t.Errorf("no case in this file produces %s, so nothing asserts it carries a remedy", code)
		}
	}
}

// A REFUSAL NAMES THE STORE BY DOMAIN WHEN THE READER COULD SEE IT, and by id
// when they could not. An operator acts on the name they recognise; the id is
// the honest fallback -- "this one, which you cannot see".
func TestARefusalNamesTheStoreTheWayTheReaderCouldSeeIt(t *testing.T) {
	byDomain := SiteGoLiveRefusal(true, PreviewBoundStore{ID: "abc", Domain: "acme-dev.myshopify.com"})
	if !strings.Contains(byDomain.Message, "acme-dev.myshopify.com") {
		t.Errorf("a readable store is not named by its domain: %q", byDomain.Message)
	}

	byID := SitePreviewBindingRefusal(true, unreadableStore("v1:shopify:store:abc"))
	if !strings.Contains(byID.Message, "v1:shopify:store:abc") {
		t.Errorf("an unreadable store is not named by its id: %q", byID.Message)
	}
}

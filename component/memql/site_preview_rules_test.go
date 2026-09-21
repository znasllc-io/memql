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
// store" is the boolean.
func readableStore(id, domain string, development bool) PreviewBoundStore {
	return PreviewBoundStore{ID: id, Readable: true, IsDevelopment: development, Domain: domain}
}

// unreadableStore is the third state: the store row did not come back. Its
// IsDevelopment field is deliberately false, because a rule that read it would
// be reading the zero value of a question nobody answered.
func unreadableStore(id string) PreviewBoundStore {
	return PreviewBoundStore{ID: id, Readable: false}
}

func TestSiteGoLiveRefusal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		storefront bool
		serving    PreviewBoundStore
		wantCode   string
		// why names the failure the case prevents.
		why string
	}{
		{
			name:       "an unreadable serving store refuses go-live",
			storefront: true,
			serving:    unreadableStore("v1:shopify:store:gone"),
			wantCode:   PreviewRefusalStoreUnreadable,
			why: "THE MOST IMPORTANT CASE IN THIS FILE. An unreadable store must " +
				"not read as 'not a development store': that reading lets a go-live " +
				"through precisely when the cluster has lost track of what the site " +
				"is bound to, which is the one moment it must not.",
		},
		{
			name:       "a development store on the serving binding refuses go-live",
			storefront: true,
			serving:    readableStore("dev", "acme-dev.myshopify.com", true),
			wantCode:   PreviewRefusalServingBindingIsDevelopment,
			why: "Serving a development store to shoppers shows a catalog nobody can " +
				"buy from and takes orders into a store that is not the merchant's -- " +
				"while looking like a successful launch.",
		},
		{
			name:       "a live store on the serving binding does not refuse go-live",
			storefront: true,
			serving:    readableStore("live", "acme.myshopify.com", false),
			wantCode:   "",
			why:        "The ordinary launch. A rule that refused this would refuse every go-live.",
		},
		{
			name:       "an unbound serving binding does not refuse go-live",
			storefront: true,
			serving:    PreviewBoundStore{},
			wantCode:   "",
			why: "A storefront nobody wired up reaches no store at all, so publishing " +
				"it is safe. Refusing here would be refusing to publish a page that " +
				"is not yet connected to anything.",
		},
		{
			name:       "a blank store id is the unbound state too",
			storefront: true,
			serving:    PreviewBoundStore{ID: "   ", Readable: false},
			wantCode:   "",
			why: "The id arrives off a row, so whitespace is a realistic value; reading " +
				"it as a bound-but-unreadable store would refuse a storefront nobody bound.",
		},
		{
			name:       "a non-storefront deployable is never refused",
			storefront: false,
			serving:    unreadableStore("dev"),
			wantCode:   "",
			why: "An spa or a static site has no binding and no store, so there is no " +
				"question to ask -- not even when a store id is somehow on the row.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SiteGoLiveRefusal(tc.storefront, tc.serving)
			if got.Code != tc.wantCode {
				t.Fatalf("SiteGoLiveRefusal = %q, want %q\n%s", got.Code, tc.wantCode, tc.why)
			}
		})
	}
}

// THE UNREADABLE STORE, SAID TWICE. The case above pins the code; this pins
// the property behind it -- that "unreadable" and "readable and not a
// development store" are DIFFERENT ANSWERS. They are one line apart in the
// rule and one typo apart from being the same.
func TestAnUnreadableServingStoreIsNotTheSameAnswerAsALiveOne(t *testing.T) {
	unreadable := SiteGoLiveRefusal(true, unreadableStore("v1:shopify:store:gone"))
	live := SiteGoLiveRefusal(true, readableStore("live", "acme.myshopify.com", false))

	if unreadable.Empty() {
		t.Fatal("an unreadable serving store was read as a live one -- fail-toward-refusal is the " +
			"whole point of PreviewBoundStore having three states rather than two")
	}
	if !live.Empty() {
		t.Fatal("a readable non-development store was refused, so the comparison above proves nothing")
	}
	// The refusal names the store, by id, since the domain could not be read.
	if !strings.Contains(unreadable.Message, "v1:shopify:store:gone") {
		t.Errorf("the refusal does not name the store it could not read: %q", unreadable.Message)
	}
}

func TestSitePreviewBindingRefusal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		storefront bool
		preview    PreviewBoundStore
		wantCode   string
		why        string
	}{
		{
			name:       "an unbound preview binding refuses a preview",
			storefront: true,
			preview:    PreviewBoundStore{},
			wantCode:   PreviewRefusalNoPreviewBinding,
			why: "Exercising a candidate with no development store attached would fall " +
				"back to no store at all and report observations of nothing.",
		},
		{
			name:       "an unreadable preview store refuses a preview",
			storefront: true,
			preview:    unreadableStore("v1:shopify:store:gone"),
			wantCode:   PreviewRefusalStoreUnreadable,
			why:        "Same fail-toward-refusal as the serving side: nobody could answer the question.",
		},
		{
			name:       "the store shoppers reach on the preview binding refuses a preview",
			storefront: true,
			preview:    readableStore("live", "acme.myshopify.com", false),
			wantCode:   PreviewRefusalBindingIsNotDevelopment,
			why: "A preview pointed at the live store looks exactly like a preview until " +
				"a test payment lands in the merchant's real orders -- and by then it has happened.",
		},
		{
			name:       "a development store on the preview binding is what a preview is for",
			storefront: true,
			preview:    readableStore("dev", "acme-dev.myshopify.com", true),
			wantCode:   "",
			why:        "The ordinary preview. A rule that refused this would refuse the feature.",
		},
		{
			name:       "a non-storefront deployable is never refused",
			storefront: false,
			preview:    PreviewBoundStore{},
			wantCode:   "",
			why:        "There is no store axis on an spa or a static site, so there is nothing to refuse.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SitePreviewBindingRefusal(tc.storefront, tc.preview)
			if got.Code != tc.wantCode {
				t.Fatalf("SitePreviewBindingRefusal = %q, want %q\n%s", got.Code, tc.wantCode, tc.why)
			}
		})
	}
}

// THE ASYMMETRY IS A DECISION, NOT AN ACCIDENT, so it is asserted as one
// statement rather than inferred from two tables.
//
// An UNBOUND storefront may go live -- it reaches no store, which is the state
// every storefront is in before anybody attaches one -- and may NOT be
// previewed, because a preview asks "is there a development store to exercise
// against" and the answer is no. Collapsing the two would either refuse every
// unwired publish or let a preview run against nothing and report four
// observations of a store that is not there.
func TestAnUnboundStorefrontMayGoLiveAndMayNotBePreviewed(t *testing.T) {
	unbound := PreviewBoundStore{}

	if refusal := SiteGoLiveRefusal(true, unbound); !refusal.Empty() {
		t.Errorf("an unbound storefront was refused go-live (%s) -- publishing a page nobody "+
			"has wired up is not this guard's judgement to make", refusal.Code)
	}
	refusal := SitePreviewBindingRefusal(true, unbound)
	if refusal.Empty() {
		t.Fatal("an unbound storefront was allowed a preview -- there is no development store " +
			"to exercise the candidate against, so the walk would observe nothing")
	}
	if refusal.Code != PreviewRefusalNoPreviewBinding {
		t.Errorf("the unbound preview refusal is %q, want %q -- the code is what the OS keys its "+
			"copy on, and 'no preview binding' is the one with an act behind it",
			refusal.Code, PreviewRefusalNoPreviewBinding)
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
		SiteGoLiveRefusal(true, readableStore("dev", "acme-dev.myshopify.com", true)),
		SitePreviewBindingRefusal(true, PreviewBoundStore{}),
		SitePreviewBindingRefusal(true, unreadableStore("v1:shopify:store:gone")),
		SitePreviewBindingRefusal(true, readableStore("live", "acme.myshopify.com", false)),
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
		PreviewRefusalServingBindingIsDevelopment,
		PreviewRefusalNoPreviewBinding,
		PreviewRefusalBindingIsNotDevelopment,
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
	byDomain := SiteGoLiveRefusal(true, readableStore("v1:shopify:store:abc", "acme-dev.myshopify.com", true))
	if !strings.Contains(byDomain.Message, "acme-dev.myshopify.com") {
		t.Errorf("a readable store is not named by its domain: %q", byDomain.Message)
	}

	byID := SitePreviewBindingRefusal(true, unreadableStore("v1:shopify:store:abc"))
	if !strings.Contains(byID.Message, "v1:shopify:store:abc") {
		t.Errorf("an unreadable store is not named by its id: %q", byID.Message)
	}
}

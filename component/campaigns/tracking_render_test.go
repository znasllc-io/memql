package campaigns

import (
	"strings"
	"testing"
)

// tracking_render_test.go -- what the renderer does to a body when a campaign
// asks for tracking (memql#4823).
//
// The load-bearing assertions here are the NEGATIVE ones: the text part is
// untouched, the unsubscribe link is never rewritten, and a non-http scheme
// is left exactly as authored. Each is a case where doing the obvious thing
// -- rewrite every link everywhere -- produces a message that is worse for
// the recipient than one with no tracking in it at all.

func trackingCampaign(opens, clicks bool) Campaign {
	return Campaign{
		ID: "camp-1", OwnerUserID: "u-1", Name: "Spring",
		TrackOpens: opens, TrackClicks: clicks,
	}
}

func trackingConfig() Config {
	return Config{
		UnsubscribeSecret:  trackSecretA,
		UnsubscribeBaseURL: "https://api.example.test",
	}
}

func renderTracked(t *testing.T, c Campaign, tmpl Template) (text string, html string) {
	t.Helper()
	r := Recipient{ID: "r-1", Email: "someone@example.test", DisplayName: "Ada"}
	opts := RenderOptions{Tracking: trackingConfig().trackingRenderFor(c, deliveryRowID(c.ID, r.ID))}
	msg, err := renderMessage(c, tmpl, r, "https://api.example.test/unsubscribe?token=u2.abc", opts)
	if err != nil {
		t.Fatalf("renderMessage: %v", err)
	}
	return msg.TextBody, msg.HTMLBody
}

func TestClickTrackingRewritesHTTPLinks(t *testing.T) {
	tmpl := Template{
		ID: "t-1", Subject: "s",
		TextBody: "Visit https://acme.test/spring",
		HTMLBody: `<p><a href="https://acme.test/spring?a=1&amp;b=2">Shop</a></p>`,
	}
	text, html := renderTracked(t, trackingCampaign(false, true), tmpl)

	if !strings.Contains(html, "https://api.example.test"+TrackingClickPath) {
		t.Fatalf("the href was not rewritten to the click endpoint.\ngot: %s", html)
	}
	if strings.Contains(html, `href="https://acme.test/spring`) {
		t.Errorf("the original href survived alongside the tracked one.\ngot: %s", html)
	}
	// THE TEXT PART IS UNTOUCHED. Rewriting a URL a reader can SEE is visible
	// mangling of the message for a number nobody asked for.
	if !strings.Contains(text, "https://acme.test/spring") {
		t.Errorf("the text part's link was rewritten. A recipient reading the plain-text alternative "+
			"must get the real link.\ngot: %s", text)
	}
}

// TestTheSignedTargetIsTheUNESCAPEDURL is the subtle half of the rewrite. An
// authored href carries `&amp;` for an ampersand; the redirect has to send
// the recipient to the URL with a real `&`, or every tracked link with two
// query parameters lands somewhere different from the untracked one.
func TestTheSignedTargetIsTheUnescapedURL(t *testing.T) {
	tmpl := Template{
		ID: "t-1", Subject: "s", TextBody: "t",
		HTMLBody: `<a href="https://acme.test/spring?a=1&amp;b=2">Shop</a>`,
	}
	_, html := renderTracked(t, trackingCampaign(false, true), tmpl)

	token := tokenFromTrackedHref(t, html, TrackingClickPath)
	payload, err := ParseTrackingToken([]string{trackSecretA}, token)
	if err != nil {
		t.Fatalf("the rewritten link carries a token this node cannot verify: %v", err)
	}
	if payload.URL != "https://acme.test/spring?a=1&b=2" {
		t.Errorf("signed target = %q, want the UNESCAPED url. Signing the escaped form sends the "+
			"recipient to a different address than the authored link named", payload.URL)
	}
	if payload.Kind != EngagementClick {
		t.Errorf("kind = %q, want click", payload.Kind)
	}
	if payload.CampaignID != "camp-1" {
		t.Errorf("campaignId = %q, want the campaign's own -- the handler resolves the owner from it", payload.CampaignID)
	}
}

// TestTheUnsubscribeLinkIsNeverTracked: a click on "unsubscribe" is not
// engagement, and routing an opt-out through a redirect adds a hop to the one
// link in the message that must work.
func TestTheUnsubscribeLinkIsNeverTracked(t *testing.T) {
	tmpl := Template{ID: "t-1", Subject: "s", TextBody: "t", HTMLBody: `<p>hello</p>`}
	_, html := renderTracked(t, trackingCampaign(false, true), tmpl)

	if !strings.Contains(html, `href="https://api.example.test/unsubscribe?token=u2.abc"`) {
		t.Errorf("the unsubscribe footer's own href was rewritten or lost.\ngot: %s", html)
	}
}

func TestNonHTTPSchemesAreLeftAlone(t *testing.T) {
	tmpl := Template{
		ID: "t-1", Subject: "s", TextBody: "t",
		HTMLBody: `<a href="mailto:hi@acme.test">mail</a><a href="#top">top</a><a href="/relative">rel</a>`,
	}
	_, html := renderTracked(t, trackingCampaign(false, true), tmpl)

	for _, want := range []string{`href="mailto:hi@acme.test"`, `href="#top"`, `href="/relative"`} {
		if !strings.Contains(html, want) {
			t.Errorf("%s was rewritten. A target that cannot be signed is a target that is not "+
				"rewritten, never one that redirects unverified.\ngot: %s", want, html)
		}
	}
}

func TestOpenPixelIsAppendedOnlyWhenAsked(t *testing.T) {
	tmpl := Template{ID: "t-1", Subject: "s", TextBody: "t", HTMLBody: `<p>hello</p>`}

	_, withPixel := renderTracked(t, trackingCampaign(true, false), tmpl)
	if !strings.Contains(withPixel, `width="1" height="1" alt=""`) {
		t.Errorf("no open pixel was appended.\ngot: %s", withPixel)
	}
	token := tokenFromTrackedHref(t, withPixel, TrackingOpenPath)
	payload, err := ParseTrackingToken([]string{trackSecretA}, token)
	if err != nil {
		t.Fatalf("the pixel carries a token this node cannot verify: %v", err)
	}
	if payload.Kind != EngagementOpen || payload.URL != "" {
		t.Errorf("pixel payload = %+v, want kind=open and no url", payload)
	}

	_, without := renderTracked(t, trackingCampaign(false, false), tmpl)
	if strings.Contains(without, TrackingOpenPath) {
		t.Errorf("a pixel was appended to a campaign with trackOpens=false.\ngot: %s", without)
	}
}

// TestTheTextPartNeverCarriesTracking states the format decision once: there
// is no honest way to count an open in a plain-text part, so a text-part
// beacon would be a claim the format cannot support.
func TestTheTextPartNeverCarriesTracking(t *testing.T) {
	tmpl := Template{
		ID: "t-1", Subject: "s",
		TextBody: "Visit https://acme.test/spring",
		HTMLBody: `<a href="https://acme.test/spring">Shop</a>`,
	}
	text, _ := renderTracked(t, trackingCampaign(true, true), tmpl)
	if strings.Contains(text, TrackingOpenPath) || strings.Contains(text, TrackingClickPath) {
		t.Errorf("the text part carries a tracking URL.\ngot: %s", text)
	}
}

// TestAZeroTrackingRenderTracksNothing is the default that makes every
// caller without a delivery row in hand correct by construction.
func TestAZeroTrackingRenderTracksNothing(t *testing.T) {
	tmpl := Template{
		ID: "t-1", Subject: "s", TextBody: "t",
		HTMLBody: `<a href="https://acme.test/spring">Shop</a>`,
	}
	r := Recipient{ID: "r-1", Email: "a@example.test", DisplayName: "Ada"}
	msg, err := renderMessage(trackingCampaign(true, true), tmpl, r, "https://api.example.test/unsubscribe?token=x", RenderOptions{})
	if err != nil {
		t.Fatalf("renderMessage: %v", err)
	}
	if strings.Contains(msg.HTMLBody, "/t/") {
		t.Errorf("a zero-valued TrackingRender still tracked. A caller with no delivery row has nothing "+
			"to attribute a hit to, so the honest default is an untracked body.\ngot: %s", msg.HTMLBody)
	}
	if !strings.Contains(msg.HTMLBody, `href="https://acme.test/spring"`) {
		t.Errorf("the authored link did not survive.\ngot: %s", msg.HTMLBody)
	}
}

// tokenFromTrackedHref pulls the token segment out of the first tracked URL
// on a path prefix.
func tokenFromTrackedHref(t *testing.T, body, prefix string) string {
	t.Helper()
	marker := "https://api.example.test" + prefix
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no %s URL in the body:\n%s", prefix, body)
	}
	rest := body[i+len(marker):]
	if j := strings.IndexAny(rest, `"'<> `); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatalf("a %s URL carried no token:\n%s", prefix, body)
	}
	return rest
}

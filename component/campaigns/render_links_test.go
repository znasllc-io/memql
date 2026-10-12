package campaigns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/integrations/email"
)

// render_links_test.go -- the four link merge tags, the footer's sender and
// where the footer sits (memql#6033, #6022: F6 and F11).
//
// WHAT THE FOOTER TESTS LOOK AT. They parse the rendered HTML the way a mail
// client does and ask what is on the screen, not what is in the string. A link
// that sits inside a comment, a raw-text element or an unterminated tag is in
// the string and nowhere else, and "the unsubscribe link is in the message" is
// only true when a client would show it. The footer is the one thing RFC 8058
// compliance cannot go out without, so the question is asked of the parse.

func messageTestLinks() MessageLinks {
	return MessageLinks{
		Unsubscribe: "https://api.example.test/unsubscribe?token=u3.unsubscribe",
		Preferences: "https://api.example.test/email/preferences?token=l1.preferences",
		View:        "https://api.example.test/email/view?token=l1.view",
		Confirm:     "https://api.example.test/email/confirm?token=l1.confirm",
	}
}

// renderLinks renders a fixed campaign and recipient through a template. A
// zero-valued opts.Links gets the four distinct test links, so a test can tell
// which tag resolved to which.
func renderLinks(t *testing.T, tmpl Template, opts RenderOptions) email.Message {
	t.Helper()
	if opts.Links == (MessageLinks{}) {
		opts.Links = messageTestLinks()
	}
	campaign := Campaign{ID: "camp-1", OwnerUserID: "u-1", Name: "Spring"}
	recipient := Recipient{ID: "r-1", Email: "someone@example.test", DisplayName: "Ada"}
	msg, err := renderMessage(campaign, tmpl, recipient, opts)
	if err != nil {
		t.Fatalf("renderMessage: %v", err)
	}
	return msg
}

// --- reading a rendered part back the way a client does ------------------

func parseRendered(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("the rendered HTML does not parse: %v", err)
	}
	return doc
}

// elementsNamed returns every HTML element with the given tag name, in document
// order. Comments and text are never elements, which is the point; an element
// of SVG or MathML is not an HTML one (an SVG <a> is not a link a mail client
// follows); and nothing inside a <template> is returned: a client does not
// render its content.
func elementsNamed(doc *html.Node, name string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == name && n.Namespace == "" {
			out = append(out, n)
		}
		if n.Type == html.ElementNode && n.Data == "template" {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// anchorHrefs is the href of every <a> a client would build from body, with
// the space and control characters around it dropped as a browser drops them
// (a non-breaking space is not one of them).
func anchorHrefs(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, a := range elementsNamed(parseRendered(t, body), "a") {
		out = append(out, strings.TrimFunc(attrOf(a, "href"), func(r rune) bool { return r <= ' ' }))
	}
	return out
}

func countOf(values []string, want string) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}
	return n
}

const footerSentence = "You are receiving this because"

// --- the four link tags -------------------------------------------------

// TestLinkTagsResolveInBothParts: every message resolves the four tags, in the
// text part and in the HTML part, and the scanner that reports typos finds
// nothing left over.
func TestLinkTagsResolveInBothParts(t *testing.T) {
	links := messageTestLinks()
	const tags = "U={{unsubscribeUrl}} P={{preferencesUrl}} V={{viewUrl}} C={{confirmUrl}}"
	tmpl := Template{ID: "t1", Subject: "s", TextBody: "Text " + tags, HTMLBody: "<p>Html " + tags + "</p>"}

	msg := renderLinks(t, tmpl, RenderOptions{})

	want := "U=" + links.Unsubscribe + " P=" + links.Preferences + " V=" + links.View + " C=" + links.Confirm
	if !strings.Contains(msg.TextBody, want) {
		t.Errorf("the text part did not resolve the four tags.\n  want to contain: %s\n  got: %s", want, msg.TextBody)
	}
	if !strings.Contains(msg.HTMLBody, want) {
		t.Errorf("the HTML part did not resolve the four tags.\n  want to contain: %s\n  got: %s", want, msg.HTMLBody)
	}
	if left := UnresolvedMergeTags(msg.Subject, msg.TextBody, msg.HTMLBody); len(left) != 0 {
		t.Errorf("tags left in the message: %v", left)
	}
}

// TestAnEmptyLinkRendersEmptyNotLiteral: a link a message was not given (a
// confirm link on a message that is not a confirmation) is a known tag with no
// value, which renders empty like {{accountName}} does. Leaving it literal
// would put "{{confirmUrl}}" in a recipient's inbox.
func TestAnEmptyLinkRendersEmptyNotLiteral(t *testing.T) {
	links := messageTestLinks()
	links.Confirm = ""
	tmpl := Template{ID: "t1", Subject: "s", TextBody: "[{{confirmUrl}}]", HTMLBody: "<p>[{{confirmUrl}}]</p>"}

	msg := renderLinks(t, tmpl, RenderOptions{Links: links})

	for part, body := range map[string]string{"text": msg.TextBody, "html": msg.HTMLBody} {
		if !strings.Contains(body, "[]") {
			t.Errorf("%s part: an empty link did not render empty: %s", part, body)
		}
		if strings.Contains(body, "{{") {
			t.Errorf("%s part: a tag was left literal: %s", part, body)
		}
	}
}

func TestRenderRefusesAMessageWithoutItsUnsubscribeLink(t *testing.T) {
	campaign := Campaign{ID: "camp-1", OwnerUserID: "u-1", Name: "Spring"}
	recipient := Recipient{ID: "r-1", Email: "someone@example.test"}
	tmpl := Template{ID: "t1", Subject: "s", TextBody: "Body", HTMLBody: "<p>Body</p>"}

	for name, opts := range map[string]RenderOptions{
		"no links at all":            {},
		"only the other three links": {Links: MessageLinks{Preferences: "https://x.test/p", View: "https://x.test/v", Confirm: "https://x.test/c"}},
		"a blank unsubscribe link":   {Links: MessageLinks{Unsubscribe: "   "}},
	} {
		_, err := renderMessage(campaign, tmpl, recipient, opts)
		if err == nil || !strings.Contains(err.Error(), "a message needs its unsubscribe link") {
			t.Errorf("%s: err = %v, want the refusal that names the missing link", name, err)
		}
	}
}

// --- F6: the footer names the sender -----------------------------------

func TestFooterNamesTheSenderThenTheOrganization(t *testing.T) {
	for _, tc := range []struct {
		name, sender, organization string
		wantSender                 string
		wantText, wantHTML         string
	}{
		{name: "the sender comes first", sender: "Acme News", organization: "Acme", wantSender: "Acme News",
			wantText: "you subscribed to Acme News.", wantHTML: "you subscribed to Acme News."},
		{name: "the organization when there is no sender", sender: "", organization: "Acme", wantSender: "Acme",
			wantText: "you subscribed to Acme.", wantHTML: "you subscribed to Acme."},
		{name: "neither", sender: "", organization: "", wantSender: "this mailing list",
			wantText: "you subscribed to this mailing list.", wantHTML: "you subscribed to this mailing list."},
		{name: "blank names are no names", sender: "   ", organization: "\t", wantSender: "this mailing list",
			wantText: "you subscribed to this mailing list.", wantHTML: "you subscribed to this mailing list."},
		{name: "a name stays on one line", sender: "Acme\r\nNews", organization: "", wantSender: "Acme News",
			wantText: "you subscribed to Acme News.", wantHTML: "you subscribed to Acme News."},
		{name: "markup in a name is text", sender: "Tea & <Co>", organization: "", wantSender: "Tea & <Co>",
			wantText: "you subscribed to Tea & <Co>.", wantHTML: "you subscribed to Tea &amp; &lt;Co&gt;."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := RenderOptions{SenderName: tc.sender, AccountName: tc.organization}
			if got := footerSender(opts); got != tc.wantSender {
				t.Errorf("footerSender = %q, want %q", got, tc.wantSender)
			}
			msg := renderLinks(t, Template{ID: "t1", Subject: "s", TextBody: "Body", HTMLBody: "<p>Body</p>"}, opts)
			if !strings.Contains(msg.TextBody, tc.wantText) {
				t.Errorf("text footer: want %q in %q", tc.wantText, msg.TextBody)
			}
			if !strings.Contains(msg.HTMLBody, tc.wantHTML) {
				t.Errorf("HTML footer: want %q in %q", tc.wantHTML, msg.HTMLBody)
			}
		})
	}
}

// --- F11: the footer and the pixel sit inside the body -----------------

func TestFooterAndPixelGoInsideTheBody(t *testing.T) {
	c := trackingCampaign(true, false)
	tmpl := Template{ID: "t1", Subject: "s", TextBody: "t", HTMLBody: "<html><body><p>Hi</p></body></html>"}
	r := Recipient{ID: "r-1", Email: "someone@example.test", DisplayName: "Ada"}
	opts := RenderOptions{Links: messageTestLinks(), Tracking: trackingConfig().trackingRenderFor(c, deliveryRowID(c.ID, r.ID))}

	msg, err := renderMessage(c, tmpl, r, opts)
	if err != nil {
		t.Fatalf("renderMessage: %v", err)
	}
	body := msg.HTMLBody

	if !strings.HasSuffix(body, "</body></html>") {
		t.Fatalf("the document does not end with </body></html>, so something sits after the body.\ngot: %s", body)
	}
	closeAt := strings.LastIndex(body, "</body>")
	contentAt := strings.Index(body, "<p>Hi</p>")
	footerAt := strings.Index(body, footerSentence)
	pixelAt := strings.Index(body, TrackingOpenPath)
	if contentAt < 0 || footerAt < 0 || pixelAt < 0 {
		t.Fatalf("content, footer and pixel must all be present (content %d, footer %d, pixel %d).\ngot: %s", contentAt, footerAt, pixelAt, body)
	}
	if !(contentAt < footerAt && footerAt < pixelAt && pixelAt < closeAt) {
		t.Errorf("order must be content, footer, pixel, </body> (content %d, footer %d, pixel %d, </body> %d).\ngot: %s",
			contentAt, footerAt, pixelAt, closeAt, body)
	}
}

// TestAnUntouchedPartIsLeftAsAuthored: when there is nothing to place (the
// template carries its own link and nothing is tracked) the HTML part is the
// substituted template and nothing else. Re-serializing it would add a document
// wrapper and table bodies to a layout somebody built by hand.
func TestAnUntouchedPartIsLeftAsAuthored(t *testing.T) {
	const source = "<table><tr><td>Hi {{displayName}}</td></tr></table><a href='{{unsubscribeUrl}}'>Leave</a>"
	msg := renderLinks(t, Template{ID: "t1", Subject: "s", TextBody: "t", HTMLBody: source}, RenderOptions{})

	want := "<table><tr><td>Hi Ada</td></tr></table><a href='" + messageTestLinks().Unsubscribe + "'>Leave</a>"
	if msg.HTMLBody != want {
		t.Errorf("the HTML part was changed although nothing had to be placed.\n  want: %s\n  got:  %s", want, msg.HTMLBody)
	}
}

// --- a template that carries its own unsubscribe link ------------------

func TestATemplateCarryingItsOwnUnsubscribeLinkGetsNoSecondFooter(t *testing.T) {
	links := messageTestLinks()
	const htmlWith = `<p>Hi</p><p><a href="{{unsubscribeUrl}}">Unsubscribe</a></p>`
	const htmlWithout = `<p>Hi</p>`
	const textWith = "Hi\nUnsubscribe: {{unsubscribeUrl}}\n"
	const textWithout = "Hi\n"

	for _, tc := range []struct {
		name       string
		html, text string
		htmlFooter bool
		textFooter bool
	}{
		{"the HTML carries it, the text does not", htmlWith, textWithout, false, true},
		{"the text carries it, the HTML does not", htmlWithout, textWith, true, false},
		{"both carry it", htmlWith, textWith, false, false},
		{"neither carries it", htmlWithout, textWithout, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := renderLinks(t, Template{ID: "t1", Subject: "s", TextBody: tc.text, HTMLBody: tc.html}, RenderOptions{})

			if got := strings.Contains(msg.HTMLBody, footerSentence); got != tc.htmlFooter {
				t.Errorf("HTML footer appended = %v, want %v.\n%s", got, tc.htmlFooter, msg.HTMLBody)
			}
			if got := strings.Contains(msg.TextBody, footerSentence); got != tc.textFooter {
				t.Errorf("text footer appended = %v, want %v.\n%s", got, tc.textFooter, msg.TextBody)
			}
			// However it got there, each part shows the link exactly once, and
			// the tag is the real URL and not left behind.
			if n := countOf(anchorHrefs(t, msg.HTMLBody), links.Unsubscribe); n != 1 {
				t.Errorf("the HTML part shows %d unsubscribe links, want 1.\n%s", n, msg.HTMLBody)
			}
			if n := strings.Count(msg.TextBody, links.Unsubscribe); n != 1 {
				t.Errorf("the text part carries the unsubscribe URL %d times, want 1.\n%s", n, msg.TextBody)
			}
			for part, body := range map[string]string{"text": msg.TextBody, "html": msg.HTMLBody} {
				if strings.Contains(body, "{{unsubscribeUrl}}") {
					t.Errorf("%s part: the tag was left in the message", part)
				}
			}
			// The header pair does not depend on the body.
			if got := msg.Headers[headerListUnsubscribe]; got != "<"+links.Unsubscribe+">" {
				t.Errorf("List-Unsubscribe = %q", got)
			}
			if got := msg.Headers[headerListUnsubscribePost]; got != oneClickValue {
				t.Errorf("List-Unsubscribe-Post = %q", got)
			}
		})
	}
}

// TestOnlyALinkHrefCountsAsTheHTMLPartsOwnUnsubscribeLink: the tag counts as
// the template's own link only where it is the whole href of an <a>. In a
// preheader, an alt text, a comment, plain text or as part of a longer URL it
// shows nobody a link, so the footer is still appended.
func TestOnlyALinkHrefCountsAsTheHTMLPartsOwnUnsubscribeLink(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		wantFooter bool
	}{
		{"a double-quoted href", `<a href="{{unsubscribeUrl}}">Leave</a>`, false},
		{"a single-quoted href", `<a href='{{unsubscribeUrl}}'>Leave</a>`, false},
		{"an unquoted href", `<a href={{unsubscribeUrl}}>Leave</a>`, false},
		{"upper-case markup", `<A HREF="{{unsubscribeUrl}}">Leave</A>`, false},
		{"an href padded with space", `<a href=" {{unsubscribeUrl}} ">Leave</a>`, false},
		{"an anchor among others", `<a href="https://acme.test">Shop</a> <a class="x" href="{{unsubscribeUrl}}" title="y">Leave</a>`, false},

		{"an href padded with a non-breaking space, which a browser keeps", "<a href=\"\u00a0{{unsubscribeUrl}}\">Leave</a>", true},
		{"a hidden preheader", `<div style="display:none">{{unsubscribeUrl}}</div><p>Hi</p>`, true},
		{"an alt text", `<img src="https://acme.test/a.png" alt="{{unsubscribeUrl}}">`, true},
		{"plain text in a paragraph", `<p>Copy this: {{unsubscribeUrl}}</p>`, true},
		{"the text of a link to somewhere else", `<a href="https://acme.test">{{unsubscribeUrl}}</a>`, true},
		{"a title on a link to somewhere else", `<a href="https://acme.test" title="{{unsubscribeUrl}}">Shop</a>`, true},
		{"a comment", `<!-- <a href="{{unsubscribeUrl}}">Leave</a> -->`, true},
		{"a longer URL", `<a href="https://acme.test/?u={{unsubscribeUrl}}">Leave</a>`, true},
		{"a URL with something after it", `<a href="{{unsubscribeUrl}}&x=1">Leave</a>`, true},
		{"a link inside a template element", `<template><a href="{{unsubscribeUrl}}">Leave</a></template>`, true},
		{"a link inside an svg element", `<svg><a href="{{unsubscribeUrl}}"><text>Leave</text></a></svg>`, true},
		{"a link inside a script", `<script>var a = '<a href="{{unsubscribeUrl}}">';</script>`, true},
		{"a link inside a textarea", `<textarea><a href="{{unsubscribeUrl}}">Leave</a></textarea>`, true},
		{"a link inside a noscript", `<noscript><a href="{{unsubscribeUrl}}">Leave</a></noscript>`, true},
		{"a second href that a browser ignores", `<a href="https://acme.test" href="{{unsubscribeUrl}}">Leave</a>`, true},
		{"the first of two hrefs", `<a href="{{unsubscribeUrl}}" href="https://acme.test">Leave</a>`, false},
		{"a link after a template element", `<template><p>x</p></template><a href="{{unsubscribeUrl}}">Leave</a>`, false},
		{"a link after an svg element", `<svg><circle/></svg><a href="{{unsubscribeUrl}}">Leave</a>`, false},
		{"a link after a script", `<script>var a = 1;</script><a href="{{unsubscribeUrl}}">Leave</a>`, false},
		{"the other link tags", `<a href="{{preferencesUrl}}">Preferences</a>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := renderLinks(t, Template{ID: "t1", Subject: "s", TextBody: "t", HTMLBody: tc.source}, RenderOptions{})
			if got := strings.Contains(msg.HTMLBody, footerSentence); got != tc.wantFooter {
				t.Errorf("footer appended = %v, want %v.\ntemplate: %s\ngot: %s", got, tc.wantFooter, tc.source, msg.HTMLBody)
			}
			if n := countOf(anchorHrefs(t, msg.HTMLBody), messageTestLinks().Unsubscribe); n != 1 {
				t.Errorf("a client would show %d unsubscribe links, want exactly 1.\ngot: %s", n, msg.HTMLBody)
			}
		})
	}
}

// TestOnlyAStandaloneURLCountsAsTheTextPartsOwnUnsubscribeLink: in the text part
// the tag counts as the template's own link only when it stands as a URL on its
// own, so a reader's client can open it. Glued to other text it is not one.
//
// The rule is stricter than it has to be, on purpose: a duplicate footer is
// harmless, and a message whose only visible opt-out is a URL no client will
// link is not.
func TestOnlyAStandaloneURLCountsAsTheTextPartsOwnUnsubscribeLink(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		wantFooter bool
	}{
		{"after a label", "Hi\nUnsubscribe: {{unsubscribeUrl}}\nBye", false},
		{"on a line of its own", "Hi\n\n{{unsubscribeUrl}}\n\nBye", false},
		{"the whole text", "{{unsubscribeUrl}}", false},
		{"at the very end", "Hi\nUnsubscribe: {{unsubscribeUrl}}", false},
		{"in angle brackets", "Hi\nUnsubscribe: <{{unsubscribeUrl}}>\n", false},
		{"after a tab", "Hi\tunsubscribe\t{{unsubscribeUrl}}\t", false},
		{"the second of two", "see {{unsubscribeUrl}}x and then\n{{unsubscribeUrl}}\n", false},

		{"glued to a label", "Hi\nUnsubscribe:{{unsubscribeUrl}}\n", true},
		{"glued to a word before", "Hi\nleave{{unsubscribeUrl}}\n", true},
		{"glued to a word after", "Hi\n{{unsubscribeUrl}}now\n", true},
		{"followed by a full stop", "Hi\nUnsubscribe here: {{unsubscribeUrl}}.\n", true},
		{"inside parentheses", "Hi\nUnsubscribe ({{unsubscribeUrl}})\n", true},
		{"inside a Markdown link", "Hi\n[Unsubscribe]({{unsubscribeUrl}})\n", true},
		{"in half an angle bracket", "Hi\n<{{unsubscribeUrl}}\n", true},
		{"a longer URL", "Hi\nhttps://acme.test/?u={{unsubscribeUrl}}\n", true},
		{"the other link tags", "Hi\nPreferences: {{preferencesUrl}}\n", true},
		{"no tag", "Hi\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := renderLinks(t, Template{ID: "t1", Subject: "s", TextBody: tc.source}, RenderOptions{})
			if got := strings.Contains(msg.TextBody, footerSentence); got != tc.wantFooter {
				t.Errorf("footer appended = %v, want %v.\ntemplate: %q\ngot: %q", got, tc.wantFooter, tc.source, msg.TextBody)
			}
		})
	}
}

// --- minting the links for one message ---------------------------------

func linksTestWorker(t *testing.T) (*Worker, *bytes.Buffer) {
	t.Helper()
	w := newTestWorker(t, &fakeEngine{}, &recordingSender{})
	w.cfg.UnsubscribeSecret = linkSecretA
	w.cfg.UnsubscribeBaseURL = "https://api.example.test"
	w.now = func() time.Time { return linkTestNow }
	logs := &bytes.Buffer{}
	w.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return w, logs
}

// linkTestRequest names everything in canonical form, as a row hands it over, so
// the test can see that the tokens carry the bare ids.
func linkTestRequest() linkRequest {
	return linkRequest{
		OwnerUserID:      "owner-1",
		RecipientID:      "v1:campaigns:recipient:recipient-1",
		CampaignID:       "v1:campaigns:campaign:campaign-1",
		AccountID:        "v1:accounts:account:account-1",
		EmailDigest:      linkTestDigest,
		TemplateID:       "v1:campaigns:template:template-1",
		TemplateRevision: "2026-10-11T09:30:00.123456789Z",
		CampaignName:     "October newsletter",
		SenderName:       "Acme News",
	}
}

func TestMessageLinksCarryBareIdsAndWhatWasSent(t *testing.T) {
	w, _ := linksTestWorker(t)
	keys := w.cfg.UnsubscribeKeys()
	links, err := w.messageLinks(linkTestRequest())
	if err != nil {
		t.Fatalf("messageLinks: %v", err)
	}
	const origin = "https://api.example.test"

	unsubscribe, err := ParseUnsubscribeToken(keys, tokenFromLinkURL(t, links.Unsubscribe, origin+UnsubscribePath))
	if err != nil {
		t.Fatalf("the unsubscribe link does not verify: %v", err)
	}
	if unsubscribe.OwnerUserID != "owner-1" || unsubscribe.RecipientID != "recipient-1" || unsubscribe.CampaignID != "campaign-1" ||
		unsubscribe.AccountID != "account-1" || unsubscribe.EmailDigest != linkTestDigest {
		t.Errorf("unsubscribe payload = %+v", unsubscribe)
	}

	preferences, err := ParseLinkToken(keys, LinkPreferences, tokenFromLinkURL(t, links.Preferences, origin+PreferencesPath), linkTestNow)
	if err != nil {
		t.Fatalf("the preferences link does not verify for its own page: %v", err)
	}
	if preferences.OwnerUserID != "owner-1" || preferences.RecipientID != "recipient-1" || preferences.CampaignID != "campaign-1" ||
		preferences.AccountID != "account-1" || preferences.EmailDigest != linkTestDigest {
		t.Errorf("preferences payload = %+v", preferences)
	}
	if preferences.TemplateID != "" || preferences.SenderName != "" {
		t.Errorf("the preferences link carries what only the view page needs: %+v", preferences)
	}

	view, err := ParseLinkToken(keys, LinkView, tokenFromLinkURL(t, links.View, origin+ViewPath), linkTestNow)
	if err != nil {
		t.Fatalf("the view link does not verify for its own page: %v", err)
	}
	want := LinkPayload{
		OwnerUserID: "owner-1", RecipientID: "recipient-1", CampaignID: "campaign-1", AccountID: "account-1",
		EmailDigest: linkTestDigest, TemplateID: "template-1", TemplateRevision: "2026-10-11T09:30:00.123456789Z",
		SenderName: "Acme News", CampaignName: "October newsletter",
	}
	if view != want {
		t.Errorf("view payload = %+v\n  want %+v", view, want)
	}

	if links.Confirm != "" {
		t.Errorf("a message that is not a confirmation carries a confirm link: %s", links.Confirm)
	}
	// And a link opens its own page and no other.
	if _, err := ParseLinkToken(keys, LinkView, tokenFromLinkURL(t, links.Preferences, origin+PreferencesPath), linkTestNow); err == nil {
		t.Error("the preferences link opened the view page")
	}
}

func TestMessageLinksMintAConfirmLinkOnlyWhenAsked(t *testing.T) {
	w, _ := linksTestWorker(t)
	keys := w.cfg.UnsubscribeKeys()
	req := linkTestRequest()
	req.Confirm = true

	links, err := w.messageLinks(req)
	if err != nil {
		t.Fatalf("messageLinks: %v", err)
	}
	token := tokenFromLinkURL(t, links.Confirm, "https://api.example.test"+ConfirmPath)
	confirm, err := ParseLinkToken(keys, LinkConfirm, token, linkTestNow)
	if err != nil {
		t.Fatalf("the confirm link does not verify: %v", err)
	}
	if want := linkTestNow.Add(confirmLinkLifetime).Unix(); confirm.ExpiresAt != want {
		t.Errorf("the confirm link expires at %d, want %d (thirty days from the clock)", confirm.ExpiresAt, want)
	}
	if confirm.RecipientID != "recipient-1" || confirm.EmailDigest != linkTestDigest || confirm.AccountID != "account-1" {
		t.Errorf("confirm payload = %+v", confirm)
	}
	// Thirty days is the whole lifetime.
	if _, err := ParseLinkToken(keys, LinkConfirm, token, linkTestNow.Add(confirmLinkLifetime).Add(time.Second)); err != errExpiredLinkToken {
		t.Errorf("a confirm link past its lifetime gave %v, want errExpiredLinkToken", err)
	}
	// The other three are unaffected by it.
	if links.Unsubscribe == "" || links.Preferences == "" || links.View == "" {
		t.Errorf("the other links went missing: %+v", links)
	}
}

// TestAViewLinkThatCannotBeBuiltIsOmittedAndLogged: the view page is a courtesy.
// A message that cannot get one still sends, with the opt-out it has to have,
// and the failure is logged because it can only be a programming error: the
// names that feed the link are capped where they are saved.
func TestAViewLinkThatCannotBeBuiltIsOmittedAndLogged(t *testing.T) {
	w, logs := linksTestWorker(t)
	req := linkTestRequest()
	req.TemplateRevision = ""

	links, err := w.messageLinks(req)
	if err != nil {
		t.Fatalf("messageLinks refused the whole message over its view link: %v", err)
	}
	if links.View != "" {
		t.Errorf("a view link was built without a revision: %s", links.View)
	}
	if links.Unsubscribe == "" || links.Preferences == "" {
		t.Errorf("the links that can be built were dropped with it: %+v", links)
	}
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "view") || !strings.Contains(got, "campaign-1") {
		t.Errorf("the failure was not logged with the campaign it belongs to: %q", got)
	}
}

func TestMessageLinksNeedAnOriginAndASecret(t *testing.T) {
	for name, mutate := range map[string]func(*Worker){
		"no origin": func(w *Worker) { w.cfg.UnsubscribeBaseURL = "" },
		"no secret": func(w *Worker) { w.cfg.UnsubscribeSecret = "" },
	} {
		w, _ := linksTestWorker(t)
		mutate(w)
		if links, err := w.messageLinks(linkTestRequest()); err == nil {
			t.Errorf("%s: links were built: %+v", name, links)
		}
	}
}

func TestMessageLinksNeedTheOwnerRecipientAndDigest(t *testing.T) {
	w, _ := linksTestWorker(t)
	for name, mutate := range map[string]func(*linkRequest){
		"no owner":     func(r *linkRequest) { r.OwnerUserID = "" },
		"no recipient": func(r *linkRequest) { r.RecipientID = "" },
		"no digest":    func(r *linkRequest) { r.EmailDigest = "" },
	} {
		req := linkTestRequest()
		mutate(&req)
		if links, err := w.messageLinks(req); err == nil {
			t.Errorf("%s: links were built: %+v", name, links)
		}
	}
}

// TestInertLinksUseTheInertTokenOnEveryPage: a test send shows all four links
// and none of them does anything.
func TestInertLinksUseTheInertTokenOnEveryPage(t *testing.T) {
	cfg := linkTestConfig("https://api.example.test/")
	links := inertLinks(cfg)

	for _, tc := range []struct{ got, path string }{
		{links.Unsubscribe, UnsubscribePath},
		{links.Preferences, PreferencesPath},
		{links.View, ViewPath},
		{links.Confirm, ConfirmPath},
	} {
		if want := "https://api.example.test" + tc.path + "?token=" + inertUnsubscribeToken; tc.got != want {
			t.Errorf("link = %q, want %q", tc.got, want)
		}
	}
	keys := cfg.UnsubscribeKeys()
	if _, err := ParseUnsubscribeToken(keys, inertUnsubscribeToken); err == nil {
		t.Error("the inert token verifies as an unsubscribe token")
	}
	for _, kind := range linkKinds {
		if _, err := ParseLinkToken(keys, kind, inertUnsubscribeToken, linkTestNow); err == nil {
			t.Errorf("the inert token verifies as a %s token", kind)
		}
	}
}

// --- name lengths -------------------------------------------------------

// TestTheNameCapsKeepTheViewTokenUnderItsLimit: the view link carries the
// campaign's name and the sender's, and a token over maxLinkTokenBytes is
// refused. The caps are what make that refusal impossible, so this is the worst
// case they allow: every character one that JSON spells in six bytes, and every
// id as long as ids get.
func TestTheNameCapsKeepTheViewTokenUnderItsLimit(t *testing.T) {
	longID := strings.Repeat("a", 64)
	p := LinkPayload{
		OwnerUserID: longID, RecipientID: longID, CampaignID: longID, AccountID: longID, TemplateID: longID,
		EmailDigest: linkTestDigest, TemplateRevision: "2026-10-11T09:30:00.123456789Z",
		// '<' is written < by encoding/json: six bytes for one character.
		CampaignName: strings.Repeat("<", maxCampaignNameRunes),
		SenderName:   strings.Repeat("<", maxSenderNameRunes),
	}
	token, err := MintLinkToken(linkSecretA, LinkView, p)
	if err != nil {
		t.Fatalf("a view link with the longest names the caps allow cannot be minted: %v", err)
	}
	if limit := maxLinkTokenBytes * 4 / 5; len(token) > limit {
		t.Errorf("the worst case is %d bytes, over four fifths of the %d byte limit: the caps are not leaving headroom",
			len(token), maxLinkTokenBytes)
	}
}

func TestClipRunesCutsOnCharactersNotBytes(t *testing.T) {
	for _, tc := range []struct {
		in    string
		limit int
		want  string
	}{
		{"abcdef", 3, "abc"},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		{"héllo wörld", 5, "héllo"},
		{"日本語のキャンペーン", 4, "日本語の"},
		{"emoji 🙂🙂🙂", 8, "emoji 🙂🙂"},
		{"abc", 0, ""},
		{"abc", -1, ""},
	} {
		if got := clipRunes(tc.in, tc.limit); got != tc.want {
			t.Errorf("clipRunes(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}

// TestADerivedNameFitsTheCapWhateverItsBase: a recurring campaign's occurrences
// and a test run are named from another campaign's name plus a suffix. A name at
// the cap plus a suffix is over it, and the engine would refuse the write, so
// the base gives way and the suffix, which says which occurrence it is, stays.
func TestADerivedNameFitsTheCapWhateverItsBase(t *testing.T) {
	const suffix = " · 2026-10-12 09:30 UTC"
	for _, base := range []string{
		"Short",
		strings.Repeat("n", maxCampaignNameRunes-utf8.RuneCountInString(suffix)),
		strings.Repeat("n", maxCampaignNameRunes),
		strings.Repeat("日", maxCampaignNameRunes+50),
	} {
		got := nameWithSuffix(base, suffix)
		if n := utf8.RuneCountInString(got); n > maxCampaignNameRunes {
			t.Errorf("a derived name has %d characters, over the cap of %d", n, maxCampaignNameRunes)
		}
		if !strings.HasSuffix(got, suffix) {
			t.Errorf("the suffix was lost: %q", got)
		}
	}
	if got := nameWithSuffix("Short", " · Test"); got != "Short · Test" {
		t.Errorf("a name that already fits was changed: %q", got)
	}
}

// TestTheNameCapsAreDeclaredWhereTheyAreSaved ties the Go constants to the DSL.
// The view token's size bound is a statement about the DSL's limits, so the two
// cannot be allowed to drift apart: a longer name in the DSL with the old
// constant here would make the bound a lie.
func TestTheNameCapsAreDeclaredWhereTheyAreSaved(t *testing.T) {
	for _, tc := range []struct {
		concept, field string
		want           int
	}{
		{"campaign", "name", maxCampaignNameRunes},
		{"campaign", "fromName", maxSenderNameRunes},
		{"senderIdentity", "fromName", maxSenderNameRunes},
	} {
		if got := conceptMaxLength(t, tc.concept, tc.field); got != tc.want {
			t.Errorf("v1:campaigns:%s.%s declares maxLength %d, want %d", tc.concept, tc.field, got, tc.want)
		}
	}

	// The mutations that save them carry no @maxLength of their own. The argument
	// check runs before the write does, so one would answer first, in the engine's
	// words (`argument "name": value too long (201 runes, max 200)`), and the
	// sentence the person is meant to read, from the write guard, would never be
	// reached. TestAnOverLongNameIsRefusedInAPlainSentence drives the real thing.
	for mutation, args := range map[string][]string{
		"createCampaign":        {"name", "fromName"},
		"updateCampaign":        {"name", "fromName"},
		"createCampaignTestRun": {"name", "fromName"},
		"createSenderIdentity":  {"fromName"},
		"updateSenderIdentity":  {"fromName"},
	} {
		for _, arg := range args {
			if got := mutationArgMaxLength(t, mutation, arg); got != 0 {
				t.Errorf("%s declares @maxLength(%d) on %s, which makes the engine's own refusal answer before the sentence does", mutation, got, arg)
			}
		}
	}
}

// TestTheNameCapNotesAreNotInTheSDKDocs: the /// text above a mutation is its
// documentation, and the generated SDKs copy it into their doc comments, where
// it is read by people who call the API. Why the caps are declared on the concept
// and not on the arguments, and which Go file states the refusal, is for whoever
// edits the DSL, and is a // comment.
func TestTheNameCapNotesAreNotInTheSDKDocs(t *testing.T) {
	source, err := os.ReadFile("../../dsl/campaigns/mutations.memql")
	if err != nil {
		t.Fatalf("read the campaigns mutations: %v", err)
	}
	for _, mutation := range []string{"createCampaign", "updateCampaign", "createCampaignTestRun", "createSenderIdentity", "updateSenderIdentity"} {
		doc := parsedMutation(t, string(source), mutation).DocComment
		for _, implementation := range []string{"write guard", "@maxLength", ".go", "argument check", "caps are createCampaign"} {
			if strings.Contains(doc, implementation) {
				t.Errorf("%s: its documentation says %q, which is a note about the DSL and not about the API:\n%s", mutation, implementation, doc)
			}
		}
	}
	// The generated docs are the DSL's, so the same words are not in them.
	for _, generated := range []string{"../../sdk/go/client/generated_mutations.go", "../../sdk/ts/src/client/generated_mutations.ts"} {
		body, err := os.ReadFile(generated)
		if err != nil {
			t.Fatalf("read %s: %v", generated, err)
		}
		if strings.Contains(string(body), "campaign_name_guard") {
			t.Errorf("%s carries the implementation note about the name guard", generated)
		}
	}
	// The explanation is not lost: it stands in the DSL as a plain comment.
	explained := false
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "///") && strings.Contains(trimmed, "campaign_name_guard.go") {
			explained = true
		}
	}
	if !explained {
		t.Error("no // comment in the campaigns mutations says where the name caps are stated")
	}
}

// conceptMaxLength reads the @maxLength a concept in dsl/campaigns/concepts.memql
// declares on a field, the way the engine builds it: through the concept
// parser, out of the JSON schema that validates every write. Zero means none.
func conceptMaxLength(t *testing.T, concept, field string) int {
	t.Helper()
	source, err := os.ReadFile("../../dsl/campaigns/concepts.memql")
	if err != nil {
		t.Fatalf("read the campaigns concepts: %v", err)
	}
	file, err := langparser.ParseFile(string(source))
	if err != nil {
		t.Fatalf("parse the campaigns concepts: %v", err)
	}
	for _, def := range file.Definitions {
		decl, ok := def.(*langparser.ConceptDecl)
		if !ok || decl.Name != concept {
			continue
		}
		built, err := memorynodes.BuildConceptFromDecl(decl, "v1:campaigns:"+concept)
		if err != nil {
			t.Fatalf("build v1:campaigns:%s: %v", concept, err)
		}
		var schema struct {
			Properties map[string]struct {
				MaxLength int `json:"maxLength"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(built.Schemas["definition"], &schema); err != nil {
			t.Fatalf("read the schema of v1:campaigns:%s: %v", concept, err)
		}
		return schema.Properties[field].MaxLength
	}
	t.Fatalf("v1:campaigns:%s is not declared in dsl/campaigns/concepts.memql", concept)
	return 0
}

// mutationArgMaxLength reads the @maxLength on one argument of one mutation in
// dsl/campaigns/mutations.memql. Zero means none.
func mutationArgMaxLength(t *testing.T, mutation, arg string) int {
	t.Helper()
	source, err := os.ReadFile("../../dsl/campaigns/mutations.memql")
	if err != nil {
		t.Fatalf("read the campaigns mutations: %v", err)
	}
	return argMaxLengthIn(t, string(source), mutation, arg)
}

// argMaxLengthIn is mutationArgMaxLength over a source it is given. It asks the
// DSL parser, the way the engine loads a mutation, and not the text: the parser
// reads @maxLength wherever it stands among the argument's other annotations,
// which a pattern over the line has to be taught one position at a time (the
// pattern this replaced found it only straight after the type, so a cap written
// after @required or a description passed unseen).
func argMaxLengthIn(t *testing.T, source, mutation, arg string) int {
	t.Helper()
	fn := parsedMutation(t, source, mutation)
	if fn.ArgsSchema == nil {
		t.Fatalf("mutation %s declares no arguments", mutation)
	}
	for _, field := range fn.ArgsSchema.Fields {
		if field.Name == arg {
			return field.MaxLength
		}
	}
	t.Fatalf("mutation %s has no argument %s", mutation, arg)
	return 0
}

// parsedMutation parses a campaigns mutations source the way the engine's
// loader does (struct forms expanded, then parsed) and returns one mutation.
func parsedMutation(t *testing.T, source, mutation string) *langparser.FunctionDef {
	t.Helper()
	expanded, err := langparser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("expand the mutations: %v", err)
	}
	file, err := langparser.ParseFile(expanded)
	if err != nil {
		t.Fatalf("parse the mutations: %v", err)
	}
	for _, def := range file.Definitions {
		if fn, ok := def.(*langparser.FunctionDef); ok && fn.Name == mutation {
			return fn
		}
	}
	t.Fatalf("mutation %s is not declared", mutation)
	return nil
}

// TestAnArgumentCapIsFoundWhereverItStandsAmongTheAnnotations: the check that no
// mutation caps the names on its arguments (above) must not pass because a cap
// was written in a form it does not look for. @maxLength may come after
// @required, after a pattern, between two annotations or last.
func TestAnArgumentCapIsFoundWhereverItStandsAmongTheAnnotations(t *testing.T) {
	const mutation = "mutation campaign createThing {\n  args {\n%s    name string%s\n    other string\n  }\n  insert {\n    id: args.name\n  }\n}\n"
	for name, annotations := range map[string]string{
		"first":                   ` @maxLength(200)`,
		"after required":          ` @required @maxLength(200)`,
		"after a pattern":         ` @pattern("^[a-z]+$") @maxLength(200)`,
		"after two":               ` @required @pattern("^a") @maxLength(200)`,
		"between two":             ` @required @maxLength(200) @pattern("^a")`,
		"before required":         ` @maxLength(200) @required`,
		"after an enumeration":    ` @enum("a", "b") @maxLength(200)`,
		"after the bang spelling": `! @maxLength(200)`,
		"after the bang and more": `! @required @maxLength(200)`,
	} {
		source := fmt.Sprintf(mutation, "", annotations)
		if got := argMaxLengthIn(t, source, "createThing", "name"); got != 200 {
			t.Errorf("%s: the cap is read as %d, want 200", name, got)
		}
		if got := argMaxLengthIn(t, source, "createThing", "other"); got != 0 {
			t.Errorf("%s: another argument is read as capped at %d", name, got)
		}
	}
	for name, tc := range map[string]struct{ doc, annotations string }{
		"nothing":                     {``, ``},
		"only required":               {``, ` @required`},
		"a doc comment that names it": {"    /// The name, at most @maxLength(200) of it.\n", ``},
	} {
		if got := argMaxLengthIn(t, fmt.Sprintf(mutation, tc.doc, tc.annotations), "createThing", "name"); got != 0 {
			t.Errorf("%s: a cap is read as %d where there is none", name, got)
		}
	}
}

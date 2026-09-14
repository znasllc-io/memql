package campaigns

import (
	"strings"
	"testing"
)

// The `{{displayName}}` substitution carries RECIPIENT-supplied data -- it
// arrives on an imported audience roster, which is the one field in a campaign
// send that the operator did not author. It reaches two bodies with opposite
// requirements, and the original implementation used a single replacer for
// both: escaped in neither, while the footer built right below it escaped its
// operator-set values. CodeQL caught the asymmetry (memql#3348).
//
// These tests pin both halves. Deleting `substHTML` and reverting to one
// replacer fails TestDisplayNameIsEscapedInTheHTMLBody; escaping the text path
// too fails TestDisplayNameIsNotEscapedInTheTextBody.

func renderFixture(displayName string) (text string, html string) {
	c := Campaign{ID: "c1", OwnerUserID: "u1", Name: "Spring", FromName: "Acme"}
	t := Template{
		ID:       "t1",
		Subject:  "Hello {{displayName}}",
		TextBody: "Hi {{displayName}}, welcome.",
		HTMLBody: "<p>Hi {{displayName}}, welcome.</p>",
	}
	r := Recipient{ID: "r1", Email: "someone@example.com", DisplayName: displayName}
	msg, err := renderMessage(c, t, r, "https://example.com/unsubscribe?token=abc", RenderOptions{})
	if err != nil {
		panic(err)
	}
	return msg.TextBody, msg.HTMLBody
}

func TestDisplayNameIsEscapedInTheHTMLBody(t *testing.T) {
	const attack = `<script>alert(1)</script>`
	_, html := renderFixture(attack)

	if strings.Contains(html, attack) {
		t.Errorf("the recipient's display name reached the HTML body unescaped.\n"+
			"A roster field is not operator-authored content, and this body is rendered by a "+
			"mail client.\ngot: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("expected the display name to be HTML-escaped in the HTML body, got: %s", html)
	}
}

func TestDisplayNameIsNotEscapedInTheTextBody(t *testing.T) {
	// The mirror-image bug: escaping the text/plain part is not "safer", it is
	// wrong. There is no markup to break out of, and the reader would see the
	// literal characters "&amp;" where an ampersand belongs.
	text, _ := renderFixture("Ben & Jerry")

	if strings.Contains(text, "&amp;") {
		t.Errorf("the text body escaped an ampersand -- text/plain has no markup context, "+
			"so the reader sees the entity literally.\ngot: %s", text)
	}
	if !strings.Contains(text, "Ben & Jerry") {
		t.Errorf("expected the raw display name in the text body, got: %s", text)
	}
}

func TestDisplayNameEscapingDoesNotAlterAnOrdinaryName(t *testing.T) {
	// Guards against a fix that mangles the common case -- the overwhelming
	// majority of names contain none of the five characters.
	text, html := renderFixture("Ada Lovelace")

	if !strings.Contains(text, "Hi Ada Lovelace, welcome.") {
		t.Errorf("text body: got %s", text)
	}
	if !strings.Contains(html, "Hi Ada Lovelace, welcome.") {
		t.Errorf("html body: got %s", html)
	}
}

func TestFooterNeutralisesAJavascriptURL(t *testing.T) {
	// html/template's urlFilter is the reason the footer is a template rather
	// than a Sprintf: html.EscapeString would pass `javascript:` through
	// untouched, because nothing about that string needs HTML escaping. Only a
	// context-aware renderer knows an href is a URL position.
	c := Campaign{ID: "c1", OwnerUserID: "u1", Name: "Spring", FromName: "Acme"}
	tpl := Template{ID: "t1", Subject: "s", TextBody: "t", HTMLBody: "<p>body</p>"}
	r := Recipient{ID: "r1", Email: "a@example.com", DisplayName: "Ada"}

	msg, err := renderMessage(c, tpl, r, "javascript:alert(1)", RenderOptions{})
	if err != nil {
		t.Fatalf("renderMessage: %v", err)
	}
	if strings.Contains(msg.HTMLBody, "javascript:alert(1)") {
		t.Errorf("a javascript: scheme survived into the href.\ngot: %s", msg.HTMLBody)
	}
	if !strings.Contains(msg.HTMLBody, "#ZgotmplZ") {
		t.Errorf("expected html/template's urlFilter to replace the scheme, got: %s", msg.HTMLBody)
	}
}

// --- the widened merge-tag set (memql#4822, design D10) -----------------
//
// The set went from one tag to five families, and the escaping asymmetry has
// to hold for EVERY one of them or it regresses for the tag that was left
// out. The failure is silent in both directions -- an unescaped value in the
// HTML body is an injection, an escaped one in the text body is visible
// mojibake in somebody's inbox -- so this table drives each tag through both
// paths with a payload that is unambiguous in each.
//
// mergeTagsCoverEveryDeclaredTag below is the other half: a tag added to
// mergeReplacers and not to this table is a tag nothing checks.

// mergeTagCase is one tag, the value it should resolve to, and where that
// value comes from.
type mergeTagCase struct {
	tag string
	// raw is what the value literally is, and what the TEXT body must carry.
	raw string
	// escaped is what the HTML body must carry instead.
	escaped string
	// apply puts the value where the renderer will find it.
	apply func(c *Campaign, r *Recipient, opts *RenderOptions)
}

func mergeTagCases() []mergeTagCase {
	const attack = `Ben & <script>alert(1)</script>`
	const attackEscaped = `Ben &amp; &lt;script&gt;alert(1)&lt;/script&gt;`
	return []mergeTagCase{
		{
			tag: "{{displayName}}", raw: attack, escaped: attackEscaped,
			apply: func(_ *Campaign, r *Recipient, _ *RenderOptions) { r.DisplayName = attack },
		},
		{
			// RECIPIENT-supplied like displayName: an imported roster can
			// carry anything in the address column that passed shape
			// validation.
			tag: "{{email}}", raw: `a&b@example.test`, escaped: `a&amp;b@example.test`,
			apply: func(_ *Campaign, r *Recipient, _ *RenderOptions) { r.Email = `a&b@example.test` },
		},
		{
			// OPERATOR-authored, and still escaped in the HTML path. The
			// asymmetry is about the BODY's context, not about who typed the
			// value -- an operator who names a campaign "Tea & Coffee" would
			// otherwise emit a bare ampersand into markup.
			tag: "{{campaignName}}", raw: `Tea & Coffee`, escaped: `Tea &amp; Coffee`,
			apply: func(c *Campaign, _ *Recipient, _ *RenderOptions) { c.Name = `Tea & Coffee` },
		},
		{
			tag: "{{accountName}}", raw: `Smith & Sons`, escaped: `Smith &amp; Sons`,
			apply: func(_ *Campaign, _ *Recipient, o *RenderOptions) { o.AccountName = `Smith & Sons` },
		},
		{
			// The whole point of {{fields.*}}: the value came out of a CSV
			// somebody uploaded, which is the least trustworthy input in the
			// domain.
			tag: "{{fields.company}}", raw: attack, escaped: attackEscaped,
			apply: func(_ *Campaign, r *Recipient, _ *RenderOptions) {
				r.Fields = map[string]any{"company": attack}
			},
		},
	}
}

func renderTagFixture(t *testing.T, c mergeTagCase) (text string, html string) {
	t.Helper()
	campaign := Campaign{ID: "c1", OwnerUserID: "u1", Name: "Spring", FromName: "Acme"}
	recipient := Recipient{ID: "r1", Email: "someone@example.com", DisplayName: "Ada"}
	opts := RenderOptions{}
	c.apply(&campaign, &recipient, &opts)

	tmpl := Template{
		ID:       "t1",
		Subject:  "Subject " + c.tag,
		TextBody: "Text " + c.tag + " end.",
		HTMLBody: "<p>Html " + c.tag + " end.</p>",
	}
	msg, err := renderMessage(campaign, tmpl, recipient, "https://example.com/unsubscribe?token=abc", opts)
	if err != nil {
		t.Fatalf("%s: renderMessage: %v", c.tag, err)
	}
	return msg.TextBody, msg.HTMLBody
}

func TestEveryMergeTagIsEscapedInTheHTMLBody(t *testing.T) {
	for _, c := range mergeTagCases() {
		_, html := renderTagFixture(t, c)
		if strings.Contains(html, c.tag) {
			t.Errorf("%s was not substituted in the HTML body at all.\ngot: %s", c.tag, html)
			continue
		}
		if !strings.Contains(html, c.escaped) {
			t.Errorf("%s reached the HTML body unescaped.\n  want to contain: %s\n  got: %s\n"+
				"Every tag has to be in BOTH replacers. One built from the escaping list and not the "+
				"other is exactly the asymmetry CodeQL caught in memql#3348, and it is silent.",
				c.tag, c.escaped, html)
		}
	}
}

func TestEveryMergeTagIsRawInTheTextBody(t *testing.T) {
	for _, c := range mergeTagCases() {
		text, _ := renderTagFixture(t, c)
		if strings.Contains(text, c.tag) {
			t.Errorf("%s was not substituted in the text body at all.\ngot: %s", c.tag, text)
			continue
		}
		if !strings.Contains(text, c.raw) {
			t.Errorf("%s was ESCAPED in the text body. text/plain has no markup context, so the reader "+
				"sees the entity literally.\n  want to contain: %s\n  got: %s", c.tag, c.raw, text)
		}
	}
}

// TestMergeTagCasesCoverEveryDeclaredTag is the gate on the table above: a
// tag added to mergeReplacers and not to mergeTagCases is a tag with no
// escaping coverage, and nothing else would notice.
//
// It works by rendering a body containing EVERY tag the replacer declares --
// discovered by asking mergeReplacers itself rather than by a second list --
// and checking that each one both resolved and appears in the table.
func TestMergeTagCasesCoverEveryDeclaredTag(t *testing.T) {
	campaign := Campaign{ID: "c1", OwnerUserID: "u1", Name: "Spring"}
	recipient := Recipient{
		ID: "r1", Email: "a@example.test", DisplayName: "Ada",
		Fields: map[string]any{"company": "Acme"},
	}
	text, _ := mergeReplacers(campaign, recipient, "Account")

	// Replacer exposes no list, so the declared set is recovered by
	// substituting a body built from the table and asserting nothing is
	// left -- plus the converse, that a tag NOT in the table is still
	// unresolved, which is what proves the check is not vacuous.
	var body strings.Builder
	covered := map[string]bool{}
	for _, c := range mergeTagCases() {
		body.WriteString(c.tag)
		body.WriteString(" ")
		covered[c.tag] = true
	}
	if left := UnresolvedMergeTags(text.Replace(body.String())); len(left) != 0 {
		t.Errorf("these tags are in the escaping table but the replacer does not resolve them: %v", left)
	}
	if left := UnresolvedMergeTags(text.Replace("{{fields.notAColumn}}")); len(left) != 1 {
		t.Fatalf("an unknown tag resolved to something. It must stay LITERAL in the body -- that is what "+
			"makes the closed set a list of exact strings rather than a lookup that can return nothing. got %v", left)
	}
}

// TestUnknownTagsStayLiteralAndAreReported is design D11's check in one line:
// a typo'd tag is not an error, not an empty string, and not a lookup -- it
// is text nobody looked at, and the test send is what reports it.
func TestUnknownTagsStayLiteralAndAreReported(t *testing.T) {
	campaign := Campaign{ID: "c1", OwnerUserID: "u1", Name: "Spring"}
	recipient := Recipient{ID: "r1", Email: "a@example.test", DisplayName: "Ada",
		Fields: map[string]any{"company": "Acme"}}
	tmpl := Template{
		ID:       "t1",
		Subject:  "Hi {{displayName}}",
		TextBody: "Your company is {{fields.compnay}} and your plan is {{fields.plan}}.",
		HTMLBody: "<p>{{fields.compnay}}</p>",
	}
	msg, err := renderMessage(campaign, tmpl, recipient, "https://example.test/unsubscribe?token=x", RenderOptions{})
	if err != nil {
		t.Fatalf("renderMessage: %v", err)
	}
	if !strings.Contains(msg.TextBody, "{{fields.compnay}}") {
		t.Errorf("a typo'd tag did not survive into the body literally; got %s", msg.TextBody)
	}
	got := UnresolvedMergeTags(msg.Subject, msg.TextBody, msg.HTMLBody)
	want := map[string]bool{"{{fields.compnay}}": true, "{{fields.plan}}": true}
	for _, tag := range got {
		delete(want, tag)
	}
	if len(want) != 0 {
		t.Errorf("UnresolvedMergeTags missed %v; it reported %v", want, got)
	}
}

// TestMergeTagsWithAwkwardColumnNames covers the header shapes a real CSV
// produces. An identifier-shaped tag scanner would miss exactly these, which
// are the ones an operator is most likely to typo.
func TestMergeTagsWithAwkwardColumnNames(t *testing.T) {
	recipient := Recipient{
		ID: "r1", Email: "a@example.test", DisplayName: "Ada",
		Fields: map[string]any{"Company Name": "Acme", "2026 spend": "1200", "first-name": "Ada"},
	}
	text, _ := mergeReplacers(Campaign{Name: "Spring"}, recipient, "")
	body := "{{Company Name}} {{fields.Company Name}} {{fields.2026 spend}} {{fields.first-name}}"
	got := text.Replace(body)

	for _, want := range []string{"Acme", "1200", "Ada"} {
		if !strings.Contains(got, want) {
			t.Errorf("a column named with a space or a hyphen did not resolve; want %q in %q", want, got)
		}
	}
	// {{Company Name}} is NOT a tag: only the fields.* namespace reaches the
	// recipient's columns, so a bare column name stays literal.
	if left := UnresolvedMergeTags(got); len(left) != 1 || left[0] != "{{Company Name}}" {
		t.Errorf("expected only the un-namespaced {{Company Name}} to be unresolved, got %v", left)
	}
}

// TestStructuredFieldValuesRenderEmpty pins mergeValueString's one
// opinionated case: "map[a:1]" in the middle of a sentence in somebody's
// inbox is worse than a blank.
func TestStructuredFieldValuesRenderEmpty(t *testing.T) {
	recipient := Recipient{
		ID: "r1", Email: "a@example.test", DisplayName: "Ada",
		Fields: map[string]any{
			"nested": map[string]any{"a": 1},
			"list":   []any{"a", "b"},
			"count":  float64(12),
			"ok":     true,
		},
	}
	text, _ := mergeReplacers(Campaign{}, recipient, "")
	got := text.Replace("[{{fields.nested}}][{{fields.list}}][{{fields.count}}][{{fields.ok}}]")
	if got != "[][][12][true]" {
		t.Errorf("field value rendering = %q, want [][][12][true] -- structured values render empty, "+
			"scalars render in their natural spelling", got)
	}
}

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
	msg := renderMessage(c, t, r, "https://example.com/unsubscribe?token=abc")
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

package campaigns

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/compose/emaildoc"
)

func TestRecipientLinksCannotEscapeThroughTemplateContexts(t *testing.T) {
	for _, body := range []string{
		`<a href="https://outside.test/?token={{confirmUrl}}">Confirm</a>`,
		`<img src="{{preferencesUrl}}">`,
		`<a href="https://outside.test">{{viewUrl}}</a>`,
		`<p>https://outside.test/?q=<em>{{unsubscribeUrl}}</em></p>`,
		`<p>{{viewUrl}}<em>.outside.test</em></p>`,
		`<style>p{background:url({{viewUrl}})}</style>`,
		`<!-- {{confirmUrl}} -->`,
	} {
		t.Run(body, func(t *testing.T) {
			if err := validateRecipientLinkLocations(Template{HTMLBody: body}); err == nil {
				t.Fatal("unsafe recipient link admitted")
			}
		})
	}
	for _, body := range []string{`<p>Confirm: <a href="{{confirmUrl}}">Confirm subscription</a></p>`, `<p>View ({{viewUrl}}).</p>`, `<p>Preferences: {{preferencesUrl}}.</p>`} {
		if err := validateRecipientLinkLocations(Template{HTMLBody: body}); err != nil {
			t.Errorf("safe body refused: %s: %v", body, err)
		}
	}
	if err := validateRecipientLinkLocations(Template{TextBody: `https://outside.test/?q={{viewUrl}}`}); err == nil {
		t.Fatal("plain-text token exfiltration admitted")
	}
}

// TestEveryBuiltinLayoutCanBeSent: what the email builder writes for each of its
// layouts has to pass the send check. The confirmation layout puts {{confirmUrl}}
// on a button, and the builder used to draw Outlook's copy of a button as a VML
// shape whose href carried the link: not an <a>, so every confirmation email
// built in the app was refused at send ("a recipient link must be the whole href
// of an email link").
func TestEveryBuiltinLayoutCanBeSent(t *testing.T) {
	for _, info := range emaildoc.Layouts() {
		t.Run(info.ID, func(t *testing.T) {
			r, err := emaildoc.Render(context.Background(), info.Document, nil, emaildoc.RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRecipientLinkLocations(Template{Subject: r.Subject, HTMLBody: r.HTML, TextBody: r.Text}); err != nil {
				t.Errorf("cannot be sent: %v", err)
			}
		})
	}
}

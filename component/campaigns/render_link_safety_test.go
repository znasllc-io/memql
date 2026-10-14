package campaigns

import "testing"

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

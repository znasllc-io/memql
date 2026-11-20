package calls

import (
	"strings"
	"testing"
)

func TestInvitationTemplateEscapesBrandAndContainsOnlyGuestCapability(t *testing.T) {
	token := strings.Repeat("a", 32)
	message, err := renderInvitation("Call <script>alert(1)</script>\x00\r\nBcc: stranger@example.test", "calls.example.test", "person@example.test", token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(message.HTMLBody, "<script>") || strings.ContainsAny(message.Subject, "\x00\r\n") {
		t.Fatal("brand escaped its text or subject boundary")
	}
	for _, body := range []string{message.HTMLBody, message.TextBody} {
		if !strings.Contains(body, "https://calls.example.test/#j="+token) || strings.Contains(body, "#h=") {
			t.Fatal("invitation does not contain exclusively its guest link")
		}
	}
	if !strings.Contains(message.HTMLBody, "https://calls.example.test/favicon.png") {
		t.Fatal("missing same-origin logo")
	}
	if err := message.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationTemplateRefusesInjectedOriginsAndCapabilities(t *testing.T) {
	for _, host := range []string{"", "example.test/other", "example.test?redirect=evil", "user@example.test", "example.test#fragment", "example.test\r\nBcc:x", "https://example.test"} {
		if _, err := renderInvitation("Call", host, "person@example.test", strings.Repeat("a", 32)); err == nil {
			t.Errorf("accepted host %q", host)
		}
	}
	if _, err := renderInvitation("Call", "example.test", "person@example.test", "invalid"); err == nil {
		t.Fatal("accepted malformed capability")
	}
}

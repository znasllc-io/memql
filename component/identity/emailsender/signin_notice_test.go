package emailsender

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
)

// signin_notice_test.go -- design section 9 item 10's third clause: "the
// email body contains no URL" (memql#4305).
//
// # Why that is a test and not a code comment
//
// "Wasn't you? Click here to sign out everywhere" is the line every reviewer
// will want to add, and it is exactly wrong here. An unauthenticated revoke
// link mailed to a SHARED MAILBOX is a denial-of-service handle: anybody who
// can read the mailbox can sign everybody out, repeatedly, using a link we
// delivered to them. The design rejects it explicitly (section 7.1, and again
// in "Out of scope"), and the only way that decision survives contact with a
// helpful future edit is a test that fails when a link appears.

// containsURL reports whether a body carries anything a mail client would
// linkify. Deliberately crude and deliberately broad -- the assertion is
// "no URL of any kind", not "no URL of a particular shape".
func containsURL(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{"http://", "https://", "href=", "<a ", "mailto:"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func TestNewSignInNoticeCarriesNoLink(t *testing.T) {
	notice := identity.SignInNotice{
		Email:       "team@acme.test",
		Source:      "oidc_cookie",
		ClientLabel: "Mozilla/5.0",
		SourceIP:    "203.0.113.9",
		At:          time.Date(2026, 8, 22, 14, 30, 0, 0, time.UTC),
		BrandName:   "MemQL",
	}
	text := buildNewSignInText(notice.BrandName, notice)
	html := buildNewSignInHTML(notice.BrandName, notice)

	for name, body := range map[string]string{"text": text, "html": html} {
		if containsURL(body) {
			t.Errorf("the %s body of the new-sign-in notice carries a URL:\n%s\n\n"+
				"A revoke link mailed to a shared mailbox is a denial-of-service handle -- anybody "+
				"who can read the mailbox can sign everybody out, repeatedly, with a link we sent "+
				"them. The copy tells the reader to sign in and revoke from their profile page "+
				"instead. Design section 7.1.", name, body)
		}
	}

	// The three facts a reader judges by must all be present. A notice that
	// says only "somebody signed in" gives nobody anything to act on.
	for _, want := range []string{"203.0.113.9", "Mozilla/5.0", "2026"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text body omits %q; a reader deciding whether to worry needs when, "+
				"from where, and with what", want)
		}
	}
	// And it names the KIND of sign-in in words rather than as the enum.
	if !strings.Contains(text, "a web browser") {
		t.Errorf("the body does not describe the sign-in source in readable words:\n%s", text)
	}
}

// TestMissingFactsSayUnknown pins that a gap prints as "unknown" rather than
// as an empty line. "IP address: " reads like a rendering bug, and this is a
// message somebody reads while deciding whether to be alarmed.
func TestMissingFactsSayUnknown(t *testing.T) {
	body := buildNewSignInText("MemQL", identity.SignInNotice{Email: "a@b.test"})
	if strings.Contains(body, "IP address: \n") || strings.Contains(body, "Client:     \n") {
		t.Errorf("a missing fact rendered as an empty value:\n%s", body)
	}
	if !strings.Contains(body, "unknown") {
		t.Errorf("a missing fact did not render as 'unknown':\n%s", body)
	}
}

// TestSignInDisabledNoticeCarriesNoLink is the same property for the
// passkey-only message (memql#4304).
//
// It matters more here, if anything: this message is what a passkey-only
// account gets INSTEAD of a credential, and it lands in front of everyone who
// can read the address. A link in it would hand back exactly what turning
// sign-in links off was meant to take away.
func TestSignInDisabledNoticeCarriesNoLink(t *testing.T) {
	text := buildSignInDisabledText("MemQL")
	html := buildSignInDisabledHTML("MemQL")

	for name, body := range map[string]string{"text": text, "html": html} {
		if containsURL(body) {
			t.Errorf("the %s body of the sign-in-disabled notice carries a URL:\n%s\n\n"+
				"This message replaces a credential for an account that has deliberately turned "+
				"sign-in links off. Anything clickable in it gives back what the policy removed.",
				name, body)
		}
	}
	if !strings.Contains(strings.ToLower(text), "passkey") {
		t.Error("the notice does not tell the reader what to use instead")
	}
	if !strings.Contains(strings.ToLower(text), "nothing has happened") {
		t.Error("the notice does not reassure a reader who did not request it; without that line " +
			"it reads as an alarm with no resolution")
	}
}

// TestSenderSatisfiesBothInterfaces pins the wiring: EngineEmailSender is the
// magic-link Sender AND the sign-in notifier, so one object covers both, and
// a signature change on either shows up here rather than at boot.
func TestSenderSatisfiesBothInterfaces(t *testing.T) {
	var _ magiclink.Sender = (*EngineEmailSender)(nil)
	var _ identity.SignInNotifier = (*EngineEmailSender)(nil)
}

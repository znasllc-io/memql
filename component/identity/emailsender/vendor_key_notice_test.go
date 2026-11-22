package emailsender

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/integrations/email"
)

// vendor_key_notice_test.go -- the one email a key's owner gets when it stops
// working (design record 2026-11-21-vendor-accounts-and-api-keys, K13).

func refusedKeyNotice() VendorKeyNotice {
	return VendorKeyNotice{
		Email:     "dana@acme.test",
		OwnerName: "Dana",
		KeyLabel:  "Personal",
		Vendor:    "openai",
		State:     "refused",
		// What OpenAI answers a key it does not accept, after the store has
		// removed the part of the key the vendor echoes.
		Detail:    "Incorrect API key provided: sk-[removed]. You can find your API key at https://platform.openai.com/account/api-keys.",
		At:        time.Date(2026, 11, 22, 14, 30, 0, 0, time.UTC),
		BrandName: "MemQL",
	}
}

// The notice tells its reader what to do in words. It carries nothing a mail
// client would turn into a link: not one of ours, and not the one a vendor
// puts in its own answer. A message about a credential that asks its reader to
// click is what a forged one looks like, and the reader of this one should
// never have learned to.
func TestTheNoticeCarriesNoActionLinkAndNoKeyMaterial(t *testing.T) {
	notice := refusedKeyNotice()
	// Whatever reached this package, nothing shaped like a key leaves it.
	const keyShaped = "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGh"
	notice.Detail += " The key " + keyShaped + " was sent."

	bodies := map[string]string{
		"text": buildVendorKeyNoticeText(notice.BrandName, notice),
		"html": buildVendorKeyNoticeHTML(notice.BrandName, notice),
	}
	for name, body := range bodies {
		if strings.TrimSpace(body) == "" {
			t.Fatalf("the %s body of the notice is empty", name)
		}
		if containsURL(body) {
			t.Errorf("the %s body of the API key notice carries a link:\n%s", name, body)
		}
		if strings.Contains(body, "platform.openai.com") {
			t.Errorf("the %s body repeats the address in the vendor's answer, which a mail client turns into a link:\n%s", name, body)
		}
		if strings.Contains(body, keyShaped) || strings.Contains(body, "AbCdEfGhIjKl") {
			t.Errorf("the %s body carries something shaped like a key:\n%s", name, body)
		}
		// What a reader needs: which key, at which vendor, what the vendor
		// said, that it is not paying, and where to go.
		for _, want := range []string{"Personal", "OpenAI", "Incorrect API key provided", "not using this key", "Settings", "API keys"} {
			if !strings.Contains(body, want) {
				t.Errorf("the %s body does not say %q:\n%s", name, want, body)
			}
		}
	}
	if !strings.Contains(bodies["text"], "2026") {
		t.Errorf("the notice does not say when:\n%s", bodies["text"])
	}
}

// The two ways a key stops working need different things of its owner, and
// the notice says which this is.
func TestTheNoticeSaysWhichWayTheKeyStoppedWorking(t *testing.T) {
	refused := buildVendorKeyNoticeText("MemQL", refusedKeyNotice())
	if !strings.Contains(refused, "OpenAI refused the key") {
		t.Errorf("a refused key's notice does not say the vendor refused it:\n%s", refused)
	}
	if !strings.Contains(refused, "Check now") {
		t.Errorf("a refused key's notice does not say how to have it checked again:\n%s", refused)
	}

	credit := refusedKeyNotice()
	credit.Vendor, credit.State = "anthropic", "credit"
	credit.Detail = "Your credit balance is too low to access the Anthropic API."
	held := buildVendorKeyNoticeText("MemQL", credit)
	for _, want := range []string{"Anthropic", "out of credit", "about an hour"} {
		if !strings.Contains(held, want) {
			t.Errorf("an out-of-credit key's notice does not say %q:\n%s", want, held)
		}
	}
	if strings.Contains(held, "refused the key") {
		t.Errorf("an out-of-credit key's notice says the key was refused:\n%s", held)
	}
}

// The key's name is its owner's own text and the vendor's answer is the
// vendor's. Neither is markup.
func TestTheNoticeEscapesTheNameAndTheVendorsWords(t *testing.T) {
	notice := refusedKeyNotice()
	notice.KeyLabel = `<b>Personal</b>`
	notice.Detail = `<script>alert(1)</script> invalid key`
	html := buildVendorKeyNoticeHTML("MemQL", notice)
	if strings.Contains(html, "<script>") || strings.Contains(html, "<b>Personal</b>") {
		t.Errorf("the HTML body carries its inputs as markup:\n%s", html)
	}
	if !strings.Contains(html, "&lt;b&gt;Personal&lt;/b&gt;") {
		t.Errorf("the HTML body does not show the key's name as written:\n%s", html)
	}
}

// A notice with nothing the vendor said still reads as a sentence.
func TestTheNoticeWithoutTheVendorsWords(t *testing.T) {
	notice := refusedKeyNotice()
	notice.Detail = ""
	text := buildVendorKeyNoticeText("MemQL", notice)
	if strings.Contains(text, `""`) || strings.Contains(text, "said: \n") {
		t.Errorf("a missing answer rendered as an empty quotation:\n%s", text)
	}
}

func TestSendVendorKeyNoticeNeedsARecipient(t *testing.T) {
	localInstall(t)
	s := New(nil, nil, identity.Config{})
	notice := refusedKeyNotice()
	notice.Email = " "
	if err := s.SendVendorKeyNotice(context.Background(), notice); err == nil {
		t.Fatal("a notice with no recipient was reported as sent")
	}
}

// On an install that must deliver mail, a node with no sender says so, and
// the pass that asked keeps the notice owed and tries again.
func TestSendVendorKeyNoticeRefusesWithNoSenderOnACloudInstall(t *testing.T) {
	cloudInstall(t)
	s := New(nil, nil, identity.Config{})
	if err := s.SendVendorKeyNotice(context.Background(), refusedKeyNotice()); !errors.Is(err, email.ErrLogOnlyRefused) {
		t.Fatalf("SendVendorKeyNotice = %v, want ErrLogOnlyRefused", err)
	}
}

func TestSendVendorKeyNoticeLogsWithNoSenderLocally(t *testing.T) {
	localInstall(t)
	s := New(nil, nil, identity.Config{})
	if err := s.SendVendorKeyNotice(context.Background(), refusedKeyNotice()); err != nil {
		t.Fatalf("SendVendorKeyNotice = %v, want nil on a local install with no sender", err)
	}
}

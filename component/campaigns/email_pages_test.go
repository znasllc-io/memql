package campaigns

import (
	"context"
	"errors"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/integrations/email"
)

// email_pages_test.go -- the pages behind email links: the confirm button of a
// browser copy, the window to confirm, the organization's brand, and the next
// step every page offers.

var pageHrefPattern = regexp.MustCompile(`href="([^"]+)"`)

func firstHref(t *testing.T, page string) *url.URL {
	t.Helper()
	m := pageHrefPattern.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no link in %s", page)
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func hasForm(page string) bool { return strings.Contains(page, "<form") }

func TestTheViewPageOfAPendingConfirmationHasAWorkingConfirmLink(t *testing.T) {
	keys := Config{UnsubscribeSecret: testSecret}.UnsubscribeKeys()
	view := func(t *testing.T, status string, joined time.Time, suppressed bool) (*emailPageEngine, string) {
		e, h := newEmailPageEngine(status)
		e.roster[0]["joinedAt"] = joined.UTC().Format(time.RFC3339Nano)
		if suppressed {
			e.suppression = map[string]map[string]any{emailPagePayload().EmailDigest: {"reason": "hard_bounce"}}
		}
		e.historical = map[string]any{"id": testTemplate, "accountId": "org-1", "createdAt": emailPagePayload().TemplateRevision, "subject": "Confirm", "htmlBody": `<p><a href="{{confirmUrl}}">Confirm</a></p>`}
		rr := pageRequest(h, "GET", ViewPath, emailPageToken(t, LinkView, emailPagePayload()), "")
		if rr.Code != 200 {
			t.Fatal(rr.Code, rr.Body.String())
		}
		for _, c := range e.calls {
			if strings.HasPrefix(c.query, "mutation ") {
				t.Fatal("a view wrote", c)
			}
		}
		return e, rr.Body.String()
	}

	t.Run("a pending member inside the window gets a confirm link that dies with the window", func(t *testing.T) {
		joined := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
		_, page := view(t, "pending", joined, false)
		link := firstHref(t, page)
		if link.Path != ConfirmPath {
			t.Fatalf("link %s", link)
		}
		p, err := ParseLinkToken(keys, LinkConfirm, link.Query().Get("token"), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if want := joined.Add(confirmLinkLifetime).Unix(); p.ExpiresAt != want {
			t.Fatalf("expires %d, want the end of the member's window %d (viewing must not extend it)", p.ExpiresAt, want)
		}
		if p.RecipientID != "r-1" || p.EmailDigest != emailPagePayload().EmailDigest {
			t.Fatalf("payload %+v", p)
		}
	})
	for name, c := range map[string]struct {
		status     string
		joined     time.Time
		suppressed bool
	}{
		"a pending member past the window":  {"pending", time.Now().Add(-31 * 24 * time.Hour), false},
		"a member who is already confirmed": {"subscribed", time.Now().Add(-time.Hour), false},
		"a suppressed pending member":       {"pending", time.Now().Add(-time.Hour), true},
	} {
		t.Run(name+" gets their preferences page", func(t *testing.T) {
			_, page := view(t, c.status, c.joined, c.suppressed)
			if link := firstHref(t, page); link.Path != PreferencesPath {
				t.Fatalf("link %s", link)
			}
		})
	}
}

func TestThePreferencesPageDoesNotConfirmAnExpiredSignUp(t *testing.T) {
	token := emailPageToken(t, LinkPreferences, emailPagePayload())
	t.Run("inside the window it offers to confirm", func(t *testing.T) {
		_, h := newEmailPageEngine("pending")
		rr := pageRequest(h, "GET", PreferencesPath, token, "")
		if !strings.Contains(rr.Body.String(), "Confirm subscription") {
			t.Fatal(rr.Body.String())
		}
	})
	t.Run("past it the sign-up has lapsed and nothing confirms", func(t *testing.T) {
		e, h := newEmailPageEngine("pending")
		e.roster[0]["joinedAt"] = time.Now().Add(-confirmLinkLifetime - time.Hour).UTC().Format(time.RFC3339Nano)
		for _, method := range []string{"GET", "POST"} {
			rr := pageRequest(h, method, PreferencesPath, token, "subscribe")
			body := rr.Body.String()
			if rr.Code != 200 || offersConfirm(body) || !strings.Contains(body, "<h1>This sign-up has expired.</h1>") || !strings.Contains(body, "didn't confirm") {
				t.Fatal(method, rr.Code, body)
			}
		}
		for _, c := range e.calls {
			if strings.HasPrefix(c.query, "mutation ") {
				t.Fatal("an expired sign-up was changed", c)
			}
		}
	})
}

// offersConfirm says whether a page carries a button that confirms.
func offersConfirm(page string) bool { return strings.Contains(page, `name="action" value="confirm"`) }

// offersRenewal says whether a page offers to send a new confirmation email.
func offersRenewal(page string) bool {
	return strings.Contains(page, `name="action" value="renew"`) && strings.Contains(page, "Send me a new confirmation email")
}

// TestAnExpiredSignUpCanAskForANewConfirmationEmail: a pending member past the
// window to confirm is offered "Send me a new confirmation email". Pressing it
// starts a fresh pending membership, as "Subscribe again" does in an audience that
// asks people to confirm: the audience's joined automation mails the new
// confirmation, and the page says to check the inbox. Pressing it again, or
// anywhere it is not offered, writes nothing.
func TestAnExpiredSignUpCanAskForANewConfirmationEmail(t *testing.T) {
	token := emailPageToken(t, LinkPreferences, emailPagePayload())
	lapsed := time.Now().Add(-confirmLinkLifetime - time.Hour).UTC()
	expired := func(t *testing.T) (*emailPageEngine, *EmailLinkHandler) {
		e, h := newEmailPageEngine("pending")
		e.roster[0]["joinedAt"] = lapsed.Format(time.RFC3339Nano)
		return e, h
	}
	t.Run("the expired sign-up offers it and confirms nothing", func(t *testing.T) {
		_, h := expired(t)
		body := pageRequest(h, "GET", PreferencesPath, token, "").Body.String()
		if !offersRenewal(body) || offersConfirm(body) || !strings.Contains(body, "<h1>This sign-up has expired.</h1>") {
			t.Fatal(body)
		}
	})
	t.Run("pressing it starts a new membership once", func(t *testing.T) {
		e, h := expired(t)
		for press := 1; press <= 2; press++ {
			rr := pageRequest(h, "POST", PreferencesPath, token, "renew")
			if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Check your inbox to confirm.") {
				t.Fatal(press, rr.Code, rr.Body.String())
			}
		}
		restarts := e.mutations("restartRecipientMembership")
		if len(restarts) != 1 || argOf(restarts[0].query, "subscriptionStatus") != "pending" || !parseTime(argOf(restarts[0].query, "joinedAt")).After(lapsed) {
			t.Fatalf("pressing twice restarted the membership %v, want once, pending, after it lapsed", restarts)
		}
		if str(e.roster[0], "subscriptionStatus") != "pending" || len(e.mutations("recordConsentGrant")) != 0 || len(e.mutations("confirmRecipientSubscription")) != 0 {
			t.Fatalf("a new confirmation email confirmed somebody: %v", e.calls)
		}
	})
	for name, arrange := range map[string]func(e *emailPageEngine){
		"inside the window": func(e *emailPageEngine) {
			e.roster[0]["joinedAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		},
		"a suppressed address": func(e *emailPageEngine) {
			e.suppression = map[string]map[string]any{emailPagePayload().EmailDigest: {"reason": "hard_bounce"}}
		},
		"someone subscribed":   func(e *emailPageEngine) { e.roster[0]["subscriptionStatus"] = "subscribed" },
		"someone who left":     func(e *emailPageEngine) { e.roster[0]["subscriptionStatus"] = "unsubscribed" },
		"a provider's verdict": func(e *emailPageEngine) { e.roster[0]["subscriptionStatus"] = "bounced" },
	} {
		t.Run("not for "+name, func(t *testing.T) {
			e, h := expired(t)
			arrange(e)
			if body := pageRequest(h, "GET", PreferencesPath, token, "").Body.String(); offersRenewal(body) {
				t.Fatal(body)
			}
			pageRequest(h, "POST", PreferencesPath, token, "renew")
			for _, c := range e.calls {
				if strings.HasPrefix(c.query, "mutation ") {
					t.Fatal("a renewal nobody was offered wrote", c.query)
				}
			}
		})
	}
}

// savedLogo is the logo a kit saved the normal way holds: a data: picture.
const savedLogo = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func TestEmailPagesCarryTheOrganizationsNameAndLogo(t *testing.T) {
	kit := func(primary, logo string) map[string]any {
		return map[string]any{"kit": map[string]any{"theme": map[string]any{"primary": primary}, "logo": map[string]any{"src": logo}}}
	}
	unsubscribeToken := func(t *testing.T) string {
		t.Helper()
		token, err := MintUnsubscribeToken(testSecret, UnsubscribePayload{OwnerUserID: testOwner, RecipientID: "r-1", CampaignID: testCampaign, AccountID: "org-1", EmailDigest: emailPagePayload().EmailDigest})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	type page struct {
		name, title string
		get         func(e *emailPageEngine, h *EmailLinkHandler) (int, string, string)
	}
	pages := []page{
		{"confirm", "<title>Confirm your subscription to Example &amp; Co</title>", func(e *emailPageEngine, h *EmailLinkHandler) (int, string, string) {
			rr := pageRequest(h, "GET", ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
			return rr.Code, rr.Body.String(), rr.Header().Get("Content-Security-Policy")
		}},
		{"preferences", "<title>Example &amp; Co email preferences</title>", func(e *emailPageEngine, h *EmailLinkHandler) (int, string, string) {
			rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
			return rr.Code, rr.Body.String(), rr.Header().Get("Content-Security-Policy")
		}},
		{"unsubscribe", "<title>Unsubscribe from Example &amp; Co</title>", func(e *emailPageEngine, _ *EmailLinkHandler) (int, string, string) {
			u := handlerFor(e)
			rr := pageRequest(u, "GET", UnsubscribePath, unsubscribeToken(t), "")
			return rr.Code, rr.Body.String(), rr.Header().Get("Content-Security-Policy")
		}},
	}
	for _, logo := range []string{"https://cdn.example.test/logo.png", savedLogo} {
		for _, pg := range pages {
			t.Run(pg.name+" is branded with "+logo[:10], func(t *testing.T) {
				e, h := newEmailPageEngine("subscribed")
				if pg.name == "confirm" {
					e.roster[0]["subscriptionStatus"] = "pending"
				}
				e.brandKit = kit("#0a7", logo)
				code, body, csp := pg.get(e, h)
				if code != 200 || !strings.Contains(body, pg.title) {
					t.Fatal(code, body)
				}
				if !strings.Contains(body, `<img class="logo" src="`+logo+`" alt="Example &amp; Co">`) || !strings.Contains(body, "background:#0a7") {
					t.Fatal(body)
				}
				if !strings.Contains(body, "<main>") || !strings.Contains(body, "<h1>") {
					t.Fatal(body)
				}
				if !strings.Contains(csp, "img-src data: https:") || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") || strings.Contains(csp, "font-src") || strings.Contains(csp, "style-src http") {
					t.Fatal(csp)
				}
			})
		}
	}
	t.Run("brand values that are not what they claim are left out", func(t *testing.T) {
		for _, c := range []struct{ primary, logo string }{
			{"red", "http://cdn.example.test/logo.png"},
			{"#12", "data:image/svg+xml;base64,PHN2Zy8+"},
			{"#12", "data:text/html;base64,PHNjcmlwdD4="},
			{"#12", "data:image/png;base64,AAAA\"onerror=\"alert(1)"},
			{"#0a7;}</style><script>alert(1)</script>", "https://x.test/a\"onerror=\"alert(1)"},
			{"url(https://x.test)", "javascript:alert(1)"},
			{"#abcd", "//cdn.example.test/logo.png"},
		} {
			e, h := newEmailPageEngine("subscribed")
			e.brandKit = kit(c.primary, c.logo)
			rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
			body := rr.Body.String()
			if rr.Code != 200 || strings.Contains(body, "<img") || strings.Contains(body, "background:") || strings.Contains(body, "<script") || strings.Contains(rr.Header().Get("Content-Security-Policy"), "img-src") {
				t.Fatalf("%+v: %d %s %s", c, rr.Code, body, rr.Header())
			}
		}
	})
	t.Run("a kit that cannot be read leaves the page plain, never broken", func(t *testing.T) {
		e, h := newEmailPageEngine("subscribed")
		e.fail = "query brandKitForAccount"
		rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Unsubscribe from all Example &amp; Co emails") || strings.Contains(rr.Body.String(), "<img") {
			t.Fatal(rr.Code, rr.Body.String())
		}
		e.fail = ""
		e.brandKit = map[string]any{"kit": "not an object"}
		rr = pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
		if rr.Code != 200 || strings.Contains(rr.Body.String(), "<img") {
			t.Fatal(rr.Code, rr.Body.String())
		}
	})
	t.Run("with no organization the titles stay generic", func(t *testing.T) {
		e, h := newEmailPageEngine("subscribed")
		e.accounts = nil
		rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
		if !strings.Contains(rr.Body.String(), "<title>Email preferences</title>") {
			t.Fatal(rr.Body.String())
		}
	})
}

func TestTheUnsubscribedPageOffersAWayBack(t *testing.T) {
	wayBack := func(t *testing.T, body string) {
		t.Helper()
		if !strings.Contains(body, "Changed your mind? <a ") || !strings.Contains(body, "Manage your email preferences.</a>") {
			t.Fatal(body)
		}
		link := firstHref(t, body)
		p, err := ParseLinkToken(Config{UnsubscribeSecret: testSecret}.UnsubscribeKeys(), LinkPreferences, link.Query().Get("token"), time.Now())
		if link.Path != PreferencesPath || err != nil || p.RecipientID != "r-1" || p.AccountID != "org-1" || p.EmailDigest != emailPagePayload().EmailDigest {
			t.Fatalf("link %s %+v %v", link, p, err)
		}
	}
	t.Run("after the unsubscribe page", func(t *testing.T) {
		token, err := MintUnsubscribeToken(testSecret, UnsubscribePayload{OwnerUserID: testOwner, RecipientID: "r-1", CampaignID: testCampaign, AccountID: "org-1", EmailDigest: emailPagePayload().EmailDigest})
		if err != nil {
			t.Fatal(err)
		}
		rr := pageRequest(handlerFor(emailPageFixtureEngine()), "POST", UnsubscribePath, token, "")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "<h1>Unsubscribed</h1>") {
			t.Fatal(rr.Code, rr.Body.String())
		}
		wayBack(t, rr.Body.String())
	})
	t.Run("a link from before tokens named an organization has no way back and no brand", func(t *testing.T) {
		e := emailPageFixtureEngine()
		e.brandKit = map[string]any{"kit": map[string]any{"logo": map[string]any{"src": "https://cdn.example.test/logo.png"}}}
		rr := pageRequest(handlerFor(e), "POST", UnsubscribePath, validToken(t), "")
		body := rr.Body.String()
		if rr.Code != 200 || strings.Contains(body, "Changed your mind") || strings.Contains(body, "<img") || !strings.Contains(body, "<title>Unsubscribe</title>") {
			t.Fatal(rr.Code, body)
		}
	})
	t.Run("after the preferences page", func(t *testing.T) {
		_, h := newEmailPageEngine("subscribed")
		rr := pageRequest(h, "POST", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "unsubscribe")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "You're unsubscribed.") {
			t.Fatal(rr.Code, rr.Body.String())
		}
		wayBack(t, rr.Body.String())
	})
	t.Run("confirming after unsubscribing says so and offers the same way back", func(t *testing.T) {
		for _, method := range []string{"GET", "POST"} {
			e, h := newEmailPageEngine("unsubscribed")
			rr := pageRequest(h, method, ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
			body := rr.Body.String()
			if rr.Code != 200 || !strings.Contains(body, "<h1>You've unsubscribed.</h1>") || !strings.Contains(body, "doesn't subscribe you again") || hasForm(body) {
				t.Fatal(method, rr.Code, body)
			}
			wayBack(t, body)
			for _, c := range e.calls {
				if strings.HasPrefix(c.query, "mutation ") {
					t.Fatal("confirming after unsubscribing wrote", c)
				}
			}
		}
	})
	t.Run("every state that cannot send has a heading", func(t *testing.T) {
		for _, status := range []string{"bounced", "complained"} {
			_, h := newEmailPageEngine(status)
			for _, path := range []string{ConfirmPath, PreferencesPath} {
				kind := LinkConfirm
				if path == PreferencesPath {
					kind = LinkPreferences
				}
				body := pageRequest(h, "GET", path, emailPageToken(t, kind, emailPagePayload()), "").Body.String()
				if !strings.Contains(body, "<h1>We can't email this address.</h1>") {
					t.Fatal(status, path, body)
				}
			}
		}
	})
}

// emailPageFixtureEngine is the page fixture as an emailPageEngine, for the
// unsubscribe handler.
func emailPageFixtureEngine() *emailPageEngine {
	return &emailPageEngine{fakeEngine: emailPageFixture()}
}

// connectionContacts is an email transport whose organizations' own email
// connections each name a contact (email.Contacts), as Azure's does from its
// reply address or default sender. err, when set, is a connection that cannot be
// read.
type connectionContacts struct {
	contacts map[string]string
	err      error
	asked    []string
}

func (c *connectionContacts) Send(context.Context, email.Message, email.SendAs) error { return nil }
func (c *connectionContacts) Contact(_ context.Context, accountID string) (string, error) {
	c.asked = append(c.asked, accountID)
	return c.contacts[accountID], c.err
}

func TestExpiredAndInvalidLinkPagesNameAContact(t *testing.T) {
	const reply = "the email this link came from"
	expired := func() string {
		p := emailPagePayload()
		p.ExpiresAt = time.Now().Add(-time.Hour).Unix()
		return emailPageToken(t, LinkConfirm, p)
	}
	page := func(contacts *connectionContacts) (*emailPageEngine, *EmailLinkHandler) {
		e, h := newEmailPageEngine("pending")
		h.WithSender(func() email.Sender { return contacts })
		return e, h
	}
	t.Run("the organization's own email connection names the contact", func(t *testing.T) {
		contacts := &connectionContacts{contacts: map[string]string{"org-1": "help@example.test", "org-2": "other@example.test"}}
		_, h := page(contacts)
		rr := pageRequest(h, "POST", ConfirmPath, expired(), "")
		body := rr.Body.String()
		if rr.Code != 410 || !strings.Contains(body, "help@example.test") || !strings.Contains(body, "Example &amp; Co") || strings.Contains(body, "other@") {
			t.Fatal(rr.Code, body)
		}
		if len(contacts.asked) != 1 || contacts.asked[0] != "org-1" {
			t.Fatalf("the transport was asked for %v, want the link's organization", contacts.asked)
		}
	})
	t.Run("a sender record is never the contact", func(t *testing.T) {
		e, h := page(&connectionContacts{})
		e.senders = []map[string]any{{"id": "v1:campaigns:senderIdentity:news", "accountId": "org-1", "address": "news@example.test", "replyTo": "records@example.test", "status": "active", "fromName": "Example"}}
		body := pageRequest(h, "POST", ConfirmPath, expired(), "").Body.String()
		if strings.Contains(body, "news@") || strings.Contains(body, "records@") || !strings.Contains(body, reply) {
			t.Fatal(body)
		}
		for _, c := range e.calls {
			if strings.HasPrefix(c.query, "query senderIdentitiesForAccount") {
				t.Fatal("a page read the organization's sender records")
			}
		}
	})
	t.Run("an organization with no email connection of its own says to reply", func(t *testing.T) {
		_, h := page(&connectionContacts{})
		rr := pageRequest(h, "POST", ConfirmPath, expired(), "")
		if rr.Code != 410 || !strings.Contains(rr.Body.String(), reply) {
			t.Fatal(rr.Code, rr.Body.String())
		}
	})
	t.Run("a connection that cannot be read leaves the page saying to reply", func(t *testing.T) {
		_, h := page(&connectionContacts{contacts: map[string]string{"org-1": "help@example.test"}, err: errors.New("the connection could not be read")})
		rr := pageRequest(h, "POST", ConfirmPath, expired(), "")
		if rr.Code != 410 || strings.Contains(rr.Body.String(), "help@") || !strings.Contains(rr.Body.String(), reply) {
			t.Fatal(rr.Code, rr.Body.String())
		}
	})
	t.Run("a node with no email transport says to reply", func(t *testing.T) {
		_, h := newEmailPageEngine("pending")
		rr := pageRequest(h, "POST", ConfirmPath, expired(), "")
		if rr.Code != 410 || !strings.Contains(rr.Body.String(), reply) {
			t.Fatal(rr.Code, rr.Body.String())
		}
	})
	t.Run("a forged link names nobody", func(t *testing.T) {
		contacts := &connectionContacts{contacts: map[string]string{"org-1": "help@example.test"}}
		e, h := page(contacts)
		rr := pageRequest(h, "POST", ConfirmPath, "forged", "")
		body := rr.Body.String()
		if rr.Code != 400 || len(e.calls) != 0 || len(contacts.asked) != 0 || strings.Contains(body, "help@example.test") || strings.Contains(body, "Example") || !strings.Contains(body, reply) {
			t.Fatal(rr.Code, e.calls, contacts.asked, body)
		}
	})
	t.Run("a verified link whose recipient is gone still names its organization and contact", func(t *testing.T) {
		e, h := page(&connectionContacts{contacts: map[string]string{"org-1": "help@example.test"}})
		e.roster = nil
		rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
		if rr.Code != 400 || !strings.Contains(rr.Body.String(), "help@example.test") {
			t.Fatal(rr.Code, rr.Body.String())
		}
	})
	t.Run("a page that names nobody does not ask", func(t *testing.T) {
		contacts := &connectionContacts{contacts: map[string]string{"org-1": "help@example.test"}}
		_, h := newEmailPageEngine("subscribed")
		h.WithSender(func() email.Sender { return contacts })
		if rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), ""); rr.Code != 200 || strings.Contains(rr.Body.String(), "help@") {
			t.Fatal(rr.Code, rr.Body.String())
		}
		unsubscribe := handlerFor(emailPageFixtureEngine()).WithSender(func() email.Sender { return contacts })
		token, err := MintUnsubscribeToken(testSecret, UnsubscribePayload{OwnerUserID: testOwner, RecipientID: "r-1", CampaignID: testCampaign, AccountID: "org-1", EmailDigest: emailPagePayload().EmailDigest})
		if err != nil {
			t.Fatal(err)
		}
		if rr := pageRequest(unsubscribe, "GET", UnsubscribePath, token, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Example &amp; Co") {
			t.Fatal(rr.Code, rr.Body.String())
		}
		if len(contacts.asked) != 0 {
			t.Fatalf("pages that name no contact asked the transport for one %d times", len(contacts.asked))
		}
	})
}

func TestThePreferencesUnsubscribeSaysItCoversEveryEmail(t *testing.T) {
	_, h := newEmailPageEngine("subscribed")
	body := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "").Body.String()
	if !strings.Contains(body, "Unsubscribe from all Example &amp; Co emails") {
		t.Fatal(body)
	}
	e, h := newEmailPageEngine("subscribed")
	e.accounts = nil
	body = pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "").Body.String()
	if !strings.Contains(body, "Unsubscribe from all emails") || strings.Contains(body, "Unsubscribe from all  ") {
		t.Fatal(body)
	}
}

func TestAPendingMemberWithNoJoinDateIsOutsideTheWindow(t *testing.T) {
	e, h := newEmailPageEngine("pending")
	delete(e.roster[0], "joinedAt")
	rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
	if offersConfirm(rr.Body.String()) || !strings.Contains(rr.Body.String(), "This sign-up has expired.") {
		t.Fatal(rr.Body.String())
	}
	if rr = pageRequest(h, "POST", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "confirm"); offersConfirm(rr.Body.String()) {
		t.Fatal(rr.Body.String())
	}
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "mutation ") {
			t.Fatal(c)
		}
	}
}

// Only the confirm button confirms: "subscribe" from a pending member, "confirm"
// from anyone who is not pending or is past the window, change nothing.
func TestOnlyAnExplicitConfirmConfirmsFromThePreferencesPage(t *testing.T) {
	token := emailPageToken(t, LinkPreferences, emailPagePayload())
	writes := func(e *emailPageEngine) int {
		n := 0
		for _, c := range e.calls {
			if strings.HasPrefix(c.query, "mutation ") {
				n++
			}
		}
		return n
	}
	t.Run("subscribe from a pending member", func(t *testing.T) {
		e, h := newEmailPageEngine("pending")
		rr := pageRequest(h, "POST", PreferencesPath, token, "subscribe")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Check your inbox to confirm.") || writes(e) != 0 || str(e.roster[0], "subscriptionStatus") != "pending" {
			t.Fatal(rr.Code, rr.Body.String(), e.calls)
		}
	})
	t.Run("the confirm button posts confirm", func(t *testing.T) {
		_, h := newEmailPageEngine("pending")
		body := pageRequest(h, "GET", PreferencesPath, token, "").Body.String()
		if !strings.Contains(body, `name="action" value="confirm"`) {
			t.Fatal(body)
		}
		body = pageRequest(h, "GET", ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "").Body.String()
		if !strings.Contains(body, `name="action" value="confirm"`) {
			t.Fatal(body)
		}
	})
	t.Run("confirm inside the window confirms", func(t *testing.T) {
		e, h := newEmailPageEngine("pending")
		rr := pageRequest(h, "POST", PreferencesPath, token, "confirm")
		if rr.Code != 200 || str(e.roster[0], "subscriptionStatus") != "subscribed" || len(e.mutations("recordConsentGrant")) != 1 {
			t.Fatal(rr.Code, rr.Body.String(), e.calls)
		}
	})
	for name, status := range map[string]string{"unsubscribed": "unsubscribed", "bounced": "bounced", "complained": "complained"} {
		t.Run("confirm from a member who is "+name+" changes nothing", func(t *testing.T) {
			e, h := newEmailPageEngine(status)
			rr := pageRequest(h, "POST", PreferencesPath, token, "confirm")
			if rr.Code != 200 || writes(e) != 0 {
				t.Fatal(rr.Code, e.calls)
			}
		})
	}
	t.Run("confirm past the window changes nothing", func(t *testing.T) {
		e, h := newEmailPageEngine("pending")
		e.roster[0]["joinedAt"] = time.Now().Add(-confirmLinkLifetime - time.Hour).UTC().Format(time.RFC3339Nano)
		rr := pageRequest(h, "POST", PreferencesPath, token, "confirm")
		if rr.Code != 200 || writes(e) != 0 {
			t.Fatal(rr.Code, e.calls)
		}
	})
	t.Run("a suppressed pending member cannot confirm", func(t *testing.T) {
		e, h := newEmailPageEngine("pending")
		e.suppression = map[string]map[string]any{emailPagePayload().EmailDigest: {"reason": "hard_bounce"}}
		rr := pageRequest(h, "POST", PreferencesPath, token, "confirm")
		if rr.Code != 200 || writes(e) != 0 {
			t.Fatal(rr.Code, e.calls)
		}
	})
}

func TestEachMembershipRecordsItsOwnConsentWhenItConfirms(t *testing.T) {
	e, h := newEmailPageEngine("pending")
	token := emailPageToken(t, LinkPreferences, emailPagePayload())
	confirm := func() {
		t.Helper()
		if rr := pageRequest(h, "POST", PreferencesPath, token, "confirm"); rr.Code != 200 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	confirm()
	confirm() // a retry of the same membership's confirmation records nothing more
	if got := len(e.mutations("recordConsentGrant")); got != 1 {
		t.Fatalf("grants after a retry = %d", got)
	}
	e.roster[0]["subscriptionStatus"] = "pending"
	e.roster[0]["joinedAt"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	confirm()
	if got := len(e.mutations("recordConsentGrant")); got != 2 {
		t.Fatalf("a second membership's confirmation recorded %d consent events in all, want 2", got)
	}
}

func TestComingBackFailsClosedWhenTheAudienceCannotBeRead(t *testing.T) {
	setup := func() (*emailPageEngine, *EmailLinkHandler) {
		e, h := newEmailPageEngine("unsubscribed")
		e.roster[0]["audienceId"] = "aud-1"
		e.suppression = map[string]map[string]any{organizationSuppressionID("org-1", emailPagePayload().EmailDigest): {"accountId": "org-1", "reason": "unsubscribed", "suppressedAt": "2026-10-01T00:00:00Z"}}
		return e, h
	}
	token := emailPageToken(t, LinkPreferences, emailPagePayload())
	t.Run("a read error shows the retry page and writes nothing", func(t *testing.T) {
		e, h := setup()
		e.fail = "query audienceById"
		rr := pageRequest(h, "POST", PreferencesPath, token, "subscribe")
		if rr.Code != 503 || len(e.mutations("recordConsentGrant")) != 0 || len(e.mutations("restartRecipientMembership")) != 0 {
			t.Fatal(rr.Code, e.calls)
		}
	})
	t.Run("an audience that is not found is treated as confirmation-required", func(t *testing.T) {
		e, h := setup()
		rr := pageRequest(h, "POST", PreferencesPath, token, "subscribe")
		if rr.Code != 200 || str(e.roster[0], "subscriptionStatus") != "pending" || !strings.Contains(rr.Body.String(), "Check your inbox to confirm.") {
			t.Fatal(rr.Code, rr.Body.String(), e.roster[0])
		}
	})
}

func TestButtonTextHasTheBetterContrast(t *testing.T) {
	for colour, want := range map[string]string{"#0a7": "#111", "#000": "#fff", "#fff": "#111", "#123456": "#fff", "#ffcc00": "#111", "#0a7a5c": "#fff"} {
		if got := buttonText(colour); got != want {
			t.Errorf("%s: %s, want %s", colour, got, want)
		}
	}
}

func TestUnsubscribeFailurePagesSkipTheBrandReads(t *testing.T) {
	e := emailPageFixtureEngine()
	e.fail = "mutation recordOrganizationSuppression"
	token, err := MintUnsubscribeToken(testSecret, UnsubscribePayload{OwnerUserID: testOwner, RecipientID: "r-1", CampaignID: testCampaign, AccountID: "org-1", EmailDigest: emailPagePayload().EmailDigest})
	if err != nil {
		t.Fatal(err)
	}
	rr := pageRequest(handlerFor(e), "POST", UnsubscribePath, token, "")
	if rr.Code != 503 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "query brandKitForAccount") || strings.HasPrefix(c.query, "query senderIdentitiesForAccount") || strings.HasPrefix(c.query, "query clientAccountById") {
			t.Fatal("a failure page read its brand", c.query)
		}
	}
}

func TestAViewReadsTheBrandOnlyWhenItSaysUnavailable(t *testing.T) {
	e, h := newEmailPageEngine("subscribed")
	e.historical = map[string]any{"id": testTemplate, "accountId": "org-1", "createdAt": emailPagePayload().TemplateRevision, "textBody": "hello"}
	pageRequest(h, "GET", ViewPath, emailPageToken(t, LinkView, emailPagePayload()), "")
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "query brandKitForAccount") || strings.HasPrefix(c.query, "query senderIdentitiesForAccount") {
			t.Fatal("a view read the brand", c.query)
		}
	}
}

package campaigns

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// EmailLinkHandler serves token-authorized recipient pages. GET only reads;
// POST changes consent under the same cross-replica recipient gate as sending.
type EmailLinkHandler struct {
	store        *Store
	cfg          Config
	logger       *slog.Logger
	system       func(context.Context) context.Context
	now          func() time.Time
	gate         emailChoiceGate
	enrollments  emailEnrollmentGate
	finishChoice func()
}

// NewEmailLinkHandler requires the dedicated write-gate pool, never the request
// pool. A missing pool refuses writes; valid read-only pages remain available.
func NewEmailLinkHandler(engine Engine, cfg Config, logger *slog.Logger, database func() *sql.DB) *EmailLinkHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &EmailLinkHandler{store: NewStore(engine), cfg: cfg, logger: logger.With("component", "campaigns.emailLinks"), system: (&Worker{}).systemActorContext, now: time.Now, gate: newEmailChoiceGate(database), enrollments: newEmailEnrollmentGate(database)}
}

func (handler *EmailLinkHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	h := *handler // per-request store: concurrent pages never share a held gate.
	var kind LinkKind
	switch {
	case strings.HasSuffix(r.URL.Path, ConfirmPath):
		kind = LinkConfirm
	case strings.HasSuffix(r.URL.Path, PreferencesPath):
		kind = LinkPreferences
	case strings.HasSuffix(r.URL.Path, ViewPath):
		kind = LinkView
	default:
		http.NotFound(rw, r)
		return
	}
	if r.Method != http.MethodGet && (r.Method != http.MethodPost || kind == LinkView) {
		allow := "GET, POST"
		if kind == LinkView {
			allow = "GET"
		}
		rw.Header().Set("Allow", allow)
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		h.invalid(rw, pageBrand{})
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		token = strings.TrimSpace(r.PostForm.Get("token"))
	}
	p, err := ParseLinkToken(h.cfg.UnsubscribeKeys(), kind, token, h.now())
	if err != nil {
		if errors.Is(err, errExpiredLinkToken) {
			// An expired token still verified, so its organization is the one the
			// link was made for and the page may speak for it.
			owner := ownerActorContext(r.Context(), p.OwnerUserID)
			org, _ := h.store.AccountName(owner, p.AccountID)
			brand := h.store.brandFor(owner, h.logger, p.AccountID, org)
			writeBrandedPage(rw, 410, "Link expired", `<h1>This link has expired.</h1><p>Your subscription wasn't confirmed, so you won't get `+emailsFrom(org)+`.</p>`+brand.contactLine(), brand)
		} else {
			if errors.Is(err, errNoLinkClock) {
				h.logger.Error("email confirmation verifier has no clock", "error", err)
			}
			if errors.Is(err, errUnknownLinkKey) {
				h.logger.Warn("email link signed by a key this node no longer holds; preserve the previous signing secret during rotation", "keyId", linkKeyIDOf(token))
			}
			h.invalid(rw, pageBrand{})
		}
		return
	}
	if r.Method == http.MethodPost {
		phase, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(phase)
		hold, err := h.gate(context.WithoutCancel(phase), p.RecipientID, p.AccountID, p.EmailDigest)
		if err != nil {
			h.retry(rw, err)
			return
		}
		choiceHeld := true
		h.finishChoice = func() {
			if choiceHeld {
				choiceHeld = false
				hold.Release()
				h.store = handler.store
			}
		}
		defer h.finishChoice()
		h.store = NewStore(emailPageGateEngine{Engine: h.store.engine, hold: hold})
	}
	owner := memql.ContextWithFreshRead(ownerActorContext(r.Context(), p.OwnerUserID))
	recipient, found, err := h.store.RecipientByID(owner, p.RecipientID)
	if err != nil {
		h.retry(rw, err)
		return
	}
	if !found && kind == LinkView {
		h.unavailable(rw, h.brandOf(owner, p))
		return
	}
	if !found || !sameOrganization(recipient.AccountID, p.AccountID) || EmailDigest(recipient.Email) != p.EmailDigest {
		h.invalid(rw, h.brandOf(owner, p))
		return
	}
	org, err := h.store.AccountName(owner, p.AccountID)
	if err != nil {
		h.retry(rw, err)
		return
	}
	brand := h.store.brandFor(owner, h.logger, p.AccountID, org)
	if kind == LinkView {
		h.view(rw, r.Context(), owner, p, recipient, brand)
		return
	}
	system := memql.ContextWithFreshRead(h.system(r.Context()))
	_, blocked, err := h.store.SuppressionForSend(system, p.AccountID, p.EmailDigest)
	if err != nil {
		h.retry(rw, err)
		return
	}
	if kind == LinkConfirm {
		page := linkPage{brand: brand, title: brand.title("Confirm subscription", "Confirm your subscription to %s")}
		if recipient.SubscriptionStatus == "subscribed" && !blocked {
			if r.Method == http.MethodPost {
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
			}
			h.subscribed(rw, page)
			return
		}
		if recipient.SubscriptionStatus == "unsubscribed" {
			// They opted out after this email was sent. Say so, and offer the way
			// back; a confirm link never resubscribes anyone.
			page.write(rw, 200, `<h1>You've unsubscribed.</h1><p>You unsubscribed from `+emailsFrom(org)+`, so this link doesn't subscribe you again.</p>`+changedYourMind(h.preferencesLink(p)))
			return
		}
		if recipient.SubscriptionStatus != "pending" || blocked {
			h.cannotSend(rw, page)
			return
		}
		if r.Method == http.MethodPost {
			if err := h.confirm(owner, p, ConsentSourceDoubleOptIn); err != nil {
				h.retry(rw, err)
				return
			}
			if err := h.nudge(r.Context(), p); err != nil {
				h.retry(rw, err)
				return
			}
			h.subscribed(rw, page)
			return
		}
		heading := "Confirm your subscription?"
		detail := "You asked to get these emails."
		if org != "" {
			heading = "Confirm your subscription to " + org + "?"
			detail = "You asked to get emails from " + org + "."
		}
		page.write(rw, 200, "<h1>"+html.EscapeString(heading)+"</h1><p>"+html.EscapeString(detail)+"</p>"+emailPageForm(token, "", "Confirm"))
		return
	}
	h.preferences(rw, r, owner, system, p, linkPage{brand: brand, title: brand.title("Email preferences", "%s email preferences")}, recipient, token, blocked)
}

// linkPage is one page of the confirm and preferences flows: the brand it
// speaks in and the title it carries.
type linkPage struct {
	brand pageBrand
	title string
}

func (l linkPage) write(rw http.ResponseWriter, status int, body string) {
	writeBrandedPage(rw, status, l.title, body, l.brand)
}

// brandOf is the brand of a verified token whose recipient could not be used:
// the organization the signed token names still owns the page, so it still
// names that organization and its contact. A read that fails leaves the page
// plain.
func (h *EmailLinkHandler) brandOf(owner context.Context, p LinkPayload) pageBrand {
	org, err := h.store.AccountName(owner, p.AccountID)
	if err != nil {
		h.logger.Warn("email page could not read its organization's name", "account", bare(p.AccountID), "error", err)
		org = ""
	}
	return h.store.brandFor(owner, h.logger, p.AccountID, org)
}

// preferencesLink is the member's own preferences page, the way back from a page
// that says they are unsubscribed. Empty when it cannot be built.
func (h *EmailLinkHandler) preferencesLink(p LinkPayload) string {
	link, err := PreferencesURL(h.cfg, LinkPayload{OwnerUserID: p.OwnerUserID, RecipientID: bare(p.RecipientID), CampaignID: bare(p.CampaignID), AccountID: p.AccountID, EmailDigest: p.EmailDigest})
	if err != nil {
		return ""
	}
	return link
}

// inConfirmWindow reports whether a pending member can still confirm: the same
// 30 days from joining that a confirmation email's own link lives. A member with
// no recorded join cannot be shown to be inside it, so it is treated as outside:
// the page fails closed rather than confirming a sign-up it cannot date.
func inConfirmWindow(recipient Recipient, now time.Time) bool {
	return !recipient.JoinedAt.IsZero() && now.Before(recipient.JoinedAt.Add(confirmLinkLifetime))
}

func (h *EmailLinkHandler) confirm(owner context.Context, p LinkPayload, source string) error {
	eventID := sha256Hex("email-confirm\x00" + bare(p.RecipientID) + "\x00" + strconv.FormatInt(p.ExpiresAt, 10))
	if err := h.grant(owner, p, eventID, source); err != nil {
		return err
	}
	return h.store.ConfirmRecipient(owner, p.RecipientID, h.now().UTC())
}
func (h *EmailLinkHandler) grant(owner context.Context, p LinkPayload, eventID, source string) error {
	exists, err := h.store.consentExists(owner, eventID)
	if err != nil || exists {
		return err
	}
	return h.store.RecordConsent(owner, ConsentGrant, ConsentRecord{EventID: eventID, EmailDigest: p.EmailDigest, Source: source, RecipientID: p.RecipientID, CampaignID: p.CampaignID, AccountID: p.AccountID, OccurredAt: h.now().UTC()})
}
func (h *EmailLinkHandler) nudge(ctx context.Context, p LinkPayload) error {
	if h.finishChoice != nil {
		h.finishChoice()
		h.finishChoice = nil
	}
	return h.store.NudgeWaitingEnrollments(h.system(ctx), p.RecipientID, h.now().UTC(), h.enrollments)
}
func (h *EmailLinkHandler) preferences(rw http.ResponseWriter, r *http.Request, owner, system context.Context, p LinkPayload, page linkPage, recipient Recipient, token string, blocked bool) {
	org := page.brand.Org
	_, globalFound, err := h.store.SuppressionByDigest(system, p.EmailDigest)
	if err != nil {
		h.retry(rw, err)
		return
	}
	row, err := h.store.organizationSuppression(system, p.AccountID, p.EmailDigest)
	if err != nil {
		h.retry(rw, err)
		return
	}
	liftable := recipient.SubscriptionStatus == "unsubscribed" && row != nil && str(row, "reason") == "unsubscribed" && !globalFound
	pending := recipient.SubscriptionStatus == "pending" && !blocked
	// A pending member can confirm only while the confirmation they were sent
	// could still be confirmed. Past it the sign-up has lapsed and a new one is
	// needed, so the page offers no button and a posted one changes nothing.
	confirmable := pending && inConfirmWindow(recipient, h.now())
	if r.Method == http.MethodPost {
		switch r.PostForm.Get("action") {
		case "unsubscribe":
			if err := unsubscribeMailbox(r.Context(), h.store, h.system, h.now().UTC(), UnsubscribePayload{OwnerUserID: p.OwnerUserID, RecipientID: p.RecipientID, CampaignID: p.CampaignID, AccountID: p.AccountID, EmailDigest: p.EmailDigest, organizationBound: true}, ConsentSourcePreferences); err != nil {
				h.retry(rw, err)
				return
			}
			writeBrandedPage(rw, 200, page.brand.title("Unsubscribed", "Unsubscribe from %s"), `<h1>You're unsubscribed.</h1><p>You won't get `+emailsFrom(org)+`.</p>`+changedYourMind(h.preferencesLink(p)), page.brand)
			return
		case "subscribe":
			if recipient.SubscriptionStatus == "subscribed" && !blocked {
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
				h.subscribed(rw, page)
				return
			}
			if confirmable {
				if err := h.confirm(owner, p, ConsentSourcePreferences); err != nil {
					h.retry(rw, err)
					return
				}
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
				h.subscribed(rw, page)
				return
			}
			if liftable {
				audience, found, err := h.store.AudienceByID(owner, recipient.AudienceID)
				if err != nil {
					h.retry(rw, err)
					return
				}
				// A confirmation-required audience never mails anyone who has not
				// confirmed, so coming back is a new, unconfirmed membership: the
				// audience's joined automation sends the confirmation.
				confirmFirst := found && audience.Confirmation == "required"
				// Record consent first so a failed later write can be retried without a
				// sendable recipient missing its audit trail. SuppressedAt survives a lift.
				eventID := sha256Hex("email-preferences\x00" + bare(p.RecipientID) + "\x00" + str(row, "suppressedAt"))
				if err := h.grant(owner, p, eventID, ConsentSourcePreferences); err != nil {
					h.retry(rw, err)
					return
				}
				if err := h.store.LiftOrganizationSuppression(system, p.AccountID, p.EmailDigest, h.now().UTC()); err != nil {
					h.retry(rw, err)
					return
				}
				status, joined := "subscribed", h.now().UTC()
				if confirmFirst {
					status, joined = "pending", memql.VersionTimeAfter(recipient.JoinedAt, h.now().UTC())
				}
				if err := h.store.RejoinRecipient(owner, p.RecipientID, status, joined); err != nil {
					h.retry(rw, err)
					return
				}
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
				if confirmFirst {
					page.write(rw, 200, `<h1>Check your inbox to confirm.</h1><p>We're sending a confirmation email to this address. You'll get `+emailsFrom(org)+` once you confirm.</p>`)
					return
				}
				h.subscribed(rw, page)
				return
			}
		default:
			h.invalid(rw, page.brand)
			return
		}
	}
	title := "Email preferences"
	if org != "" {
		title = "Emails from " + org
	}
	body := "<h1>" + html.EscapeString(title) + "</h1>"
	unsubscribeLabel := "Unsubscribe from all emails"
	if org != "" {
		unsubscribeLabel = "Unsubscribe from all " + org + " emails"
	}
	switch {
	case recipient.SubscriptionStatus == "subscribed" && !blocked:
		body += `<p>You're subscribed.</p>` + emailPageForm(token, "unsubscribe", unsubscribeLabel)
	case liftable:
		body += `<p>You're unsubscribed.</p>` + emailPageForm(token, "subscribe", "Subscribe again")
	case confirmable:
		body += `<p>Your subscription isn't confirmed yet.</p>` + emailPageForm(token, "subscribe", "Confirm subscription")
	case pending:
		body = `<h1>This sign-up has expired.</h1><p>You didn't confirm your subscription in time, so you aren't subscribed to ` + emailsFrom(org) + `.</p>` + page.brand.contactLine()
	default:
		body = cannotSendBody(page.brand)
	}
	page.write(rw, 200, body)
}
func (h *EmailLinkHandler) view(rw http.ResponseWriter, ctx, owner context.Context, p LinkPayload, recipient Recipient, brand pageBrand) {
	org := brand.Org
	template, found, err := h.store.TemplateAtRevision(owner, p.TemplateID, p.TemplateRevision)
	if err != nil {
		h.retry(rw, err)
		return
	}
	if !found || !sameOrganization(template.AccountID, p.AccountID) || !parseTime(template.Revision).Equal(parseTime(p.TemplateRevision)) {
		h.unavailable(rw, brand)
		return
	}
	w := Worker{cfg: h.cfg, logger: h.logger, now: h.now}
	req := linkRequest{OwnerUserID: p.OwnerUserID, RecipientID: p.RecipientID, CampaignID: p.CampaignID, AccountID: p.AccountID, EmailDigest: p.EmailDigest, TemplateID: p.TemplateID, TemplateRevision: p.TemplateRevision, SenderName: p.SenderName, CampaignName: p.CampaignName}
	// A confirmation email read in a browser has to keep its button working. A
	// pending member still inside the window gets a real confirm link, one that
	// dies when the window does, so viewing an old email never extends it.
	// Anyone else gets their preferences page, which says where they stand.
	confirmOpen := false
	if recipient.SubscriptionStatus == "pending" && inConfirmWindow(recipient, h.now()) {
		_, blocked, err := h.store.SuppressionForSend(memql.ContextWithFreshRead(h.system(ctx)), p.AccountID, p.EmailDigest)
		if err != nil {
			h.retry(rw, err)
			return
		}
		confirmOpen = !blocked
	}
	if confirmOpen {
		req.Confirm, req.ConfirmExpiresAt = true, recipient.JoinedAt.Add(confirmLinkLifetime)
	}
	links, err := w.messageLinks(req)
	if err != nil {
		h.retry(rw, err)
		return
	}
	if !confirmOpen {
		links.Confirm = links.Preferences
	}
	msg, err := renderMessage(Campaign{ID: p.CampaignID, Name: p.CampaignName}, template, recipient, RenderOptions{AccountName: org, SenderName: p.SenderName, Links: links})
	if err != nil {
		h.retry(rw, err)
		return
	}
	emailPageHeaders(rw)
	rw.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data: https:; style-src 'unsafe-inline'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'; sandbox allow-popups allow-top-navigation-by-user-activation")
	body := msg.HTMLBody
	if body == "" {
		body = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(msg.Subject) + `</title><style>pre{white-space:pre-wrap;overflow-wrap:anywhere;font:16px/1.5 system-ui,sans-serif;margin:2rem auto;max-width:42rem;padding:0 1rem}</style></head><body><pre>` + html.EscapeString(msg.TextBody) + `</pre></body></html>`
	}
	rw.WriteHeader(200)
	_, _ = rw.Write([]byte(body))
}
func emailPageForm(token, action, label string) string {
	return `<form method="POST"><input type="hidden" name="token" value="` + html.EscapeString(token) + `"><input type="hidden" name="action" value="` + html.EscapeString(action) + `"><button type="submit">` + html.EscapeString(label) + `</button></form>`
}
func emailsFrom(org string) string {
	if org == "" {
		return "these emails"
	}
	return "emails from " + html.EscapeString(org)
}
func cannotSendText(org string) string {
	if org == "" {
		return "Emails can't be sent to this address."
	}
	return "Emails from " + html.EscapeString(org) + " can't be sent to this address."
}

// cannotSendBody is every page that cannot help: what is wrong, then somebody
// to ask.
func cannotSendBody(brand pageBrand) string {
	return "<h1>We can't email this address.</h1><p>" + cannotSendText(brand.Org) + "</p>" + brand.contactLine()
}
func (h *EmailLinkHandler) subscribed(rw http.ResponseWriter, page linkPage) {
	page.write(rw, 200, `<h1>You're subscribed.</h1><p>You'll get `+emailsFrom(page.brand.Org)+`.</p>`)
}
func (h *EmailLinkHandler) cannotSend(rw http.ResponseWriter, page linkPage) {
	page.write(rw, 200, cannotSendBody(page.brand))
}
func (h *EmailLinkHandler) invalid(rw http.ResponseWriter, brand pageBrand) {
	writeBrandedPage(rw, 400, "Link not valid", `<h1>This link isn't valid.</h1><p>It may have been changed in transit.</p>`+brand.contactLine(), brand)
}
func (h *EmailLinkHandler) unavailable(rw http.ResponseWriter, brand pageBrand) {
	writeBrandedPage(rw, 404, "Email unavailable", `<h1>This email isn't available.</h1>`+brand.contactLine(), brand)
}
func (h *EmailLinkHandler) retry(rw http.ResponseWriter, err error) {
	h.logger.Error("could not complete email page request", "error", err)
	rw.Header().Set("Retry-After", "30")
	writeEmailPage(rw, 503, "Please try again", `<h1>Please try again.</h1><p>We couldn't save your choice. Try this link again in a moment.</p>`)
}

// A lost database session must stop the remaining writes of a multi-row choice.
// A started write must finish before the page releases its gate, even if the
// request or phase ends meanwhile. This also covers the shared unsubscribe helper.
type emailPageGateEngine struct {
	Engine
	hold gateHold
}

func (e emailPageGateEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "mutation ") {
		if err := e.hold.Verify(ctx); err != nil {
			return nil, err
		}
		var result any
		err := detachedWrite(ctx, func(writeCtx context.Context) error {
			var err error
			result, err = e.Engine.Execute(writeCtx, query)
			return err
		})
		return result, err
	}
	return e.Engine.Execute(ctx, query)
}

type emailChoiceGate func(context.Context, string, string, string) (gateHold, error)

func newEmailChoiceGate(database func() *sql.DB) emailChoiceGate {
	return func(ctx context.Context, recipientID, accountID, digest string) (gateHold, error) {
		db, err := memql.ResolveWriteGatePool(database, "email choices", "pass database.Database.GateDB to campaigns.NewEmailLinkHandler/NewUnsubscribeHandler (tests use database.NewGatePool)")
		if err != nil {
			return nil, err
		}
		keys := []string{"campaign-journey:recipient:" + bare(recipientID)}
		// Different memberships can name the same mailbox. They share its opt-out,
		// so all page decisions (including one-click) must serialize on that too.
		if digest != "" {
			keys = append(keys, "campaign-email-mailbox:"+organizationSuppressionID(accountID, digest))
		}
		return memql.AcquireGateHold(ctx, db, 0, keys...)
	}
}

type emailEnrollmentGate func(context.Context, string, string, string) (gateHold, error)

func newEmailEnrollmentGate(database func() *sql.DB) emailEnrollmentGate {
	return func(ctx context.Context, journeyID, recipientID, enrollmentID string) (gateHold, error) {
		db, err := memql.ResolveWriteGatePool(database, "email confirmation recovery", "pass database.Database.GateDB to campaigns.NewEmailLinkHandler")
		if err != nil {
			return nil, err
		}
		return memql.AcquireGateHold(ctx, db, 0, journeyLock(journeyID), "campaign-journey:recipient:"+bare(recipientID), "campaign-journey:enrollment:"+bare(enrollmentID))
	}
}

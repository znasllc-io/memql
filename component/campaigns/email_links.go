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
		h.invalid(rw)
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		token = strings.TrimSpace(r.PostForm.Get("token"))
	}
	p, err := ParseLinkToken(h.cfg.UnsubscribeKeys(), kind, token, h.now())
	if err != nil {
		if errors.Is(err, errExpiredLinkToken) {
			org, _ := h.store.AccountName(ownerActorContext(r.Context(), p.OwnerUserID), p.AccountID)
			writeEmailPage(rw, 410, "Link expired", `<h1>This link has expired.</h1><p>Your subscription wasn't confirmed, so you won't get `+emailsFrom(org)+`.</p>`)
		} else {
			if errors.Is(err, errNoLinkClock) {
				h.logger.Error("email confirmation verifier has no clock", "error", err)
			}
			if errors.Is(err, errUnknownLinkKey) {
				h.logger.Warn("email link signed by a key this node no longer holds; preserve the previous signing secret during rotation", "keyId", linkKeyIDOf(token))
			}
			h.invalid(rw)
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
		h.unavailable(rw)
		return
	}
	if !found || !sameOrganization(recipient.AccountID, p.AccountID) || EmailDigest(recipient.Email) != p.EmailDigest {
		h.invalid(rw)
		return
	}
	org, err := h.store.AccountName(owner, p.AccountID)
	if err != nil {
		h.retry(rw, err)
		return
	}
	if kind == LinkView {
		h.view(rw, r.Context(), owner, p, recipient, org)
		return
	}
	system := memql.ContextWithFreshRead(h.system(r.Context()))
	_, blocked, err := h.store.SuppressionForSend(system, p.AccountID, p.EmailDigest)
	if err != nil {
		h.retry(rw, err)
		return
	}
	if kind == LinkConfirm {
		if recipient.SubscriptionStatus == "subscribed" && !blocked {
			if r.Method == http.MethodPost {
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
			}
			h.subscribed(rw, org)
			return
		}
		if recipient.SubscriptionStatus != "pending" || blocked {
			h.cannotSend(rw, org)
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
			h.subscribed(rw, org)
			return
		}
		heading := "Confirm your subscription?"
		detail := "You asked to get these emails."
		if org != "" {
			heading = "Confirm your subscription to " + org + "?"
			detail = "You asked to get emails from " + org + "."
		}
		writeEmailPage(rw, 200, "Confirm subscription", "<h1>"+html.EscapeString(heading)+"</h1><p>"+html.EscapeString(detail)+"</p>"+emailPageForm(token, "", "Confirm"))
		return
	}
	h.preferences(rw, r, owner, system, p, recipient, org, token, blocked)
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
func (h *EmailLinkHandler) preferences(rw http.ResponseWriter, r *http.Request, owner, system context.Context, p LinkPayload, recipient Recipient, org, token string, blocked bool) {
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
	if r.Method == http.MethodPost {
		switch r.PostForm.Get("action") {
		case "unsubscribe":
			if err := unsubscribeMailbox(r.Context(), h.store, h.system, h.now().UTC(), UnsubscribePayload{OwnerUserID: p.OwnerUserID, RecipientID: p.RecipientID, CampaignID: p.CampaignID, AccountID: p.AccountID, EmailDigest: p.EmailDigest, organizationBound: true}, ConsentSourcePreferences); err != nil {
				h.retry(rw, err)
				return
			}
			writeEmailPage(rw, 200, "Unsubscribed", `<h1>You're unsubscribed.</h1><p>You won't get `+emailsFrom(org)+`.</p>`)
			return
		case "subscribe":
			if recipient.SubscriptionStatus == "subscribed" && !blocked {
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
				h.subscribed(rw, org)
				return
			}
			if pending {
				if err := h.confirm(owner, p, ConsentSourcePreferences); err != nil {
					h.retry(rw, err)
					return
				}
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
				h.subscribed(rw, org)
				return
			}
			if liftable {
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
				if err := h.store.RejoinRecipient(owner, p.RecipientID, "subscribed", h.now().UTC()); err != nil {
					h.retry(rw, err)
					return
				}
				if err := h.nudge(r.Context(), p); err != nil {
					h.retry(rw, err)
					return
				}
				h.subscribed(rw, org)
				return
			}
		default:
			h.invalid(rw)
			return
		}
	}
	title := "Email preferences"
	if org != "" {
		title = "Emails from " + org
	}
	body := "<h1>" + html.EscapeString(title) + "</h1>"
	switch {
	case recipient.SubscriptionStatus == "subscribed" && !blocked:
		body += `<p>You're subscribed.</p>` + emailPageForm(token, "unsubscribe", "Unsubscribe")
	case liftable:
		body += `<p>You're unsubscribed.</p>` + emailPageForm(token, "subscribe", "Subscribe again")
	case pending:
		body += `<p>Your subscription isn't confirmed yet.</p>` + emailPageForm(token, "subscribe", "Confirm subscription")
	default:
		body += "<p>" + cannotSendText(org) + "</p>"
	}
	writeEmailPage(rw, 200, "Email preferences", body)
}
func (h *EmailLinkHandler) view(rw http.ResponseWriter, ctx, owner context.Context, p LinkPayload, recipient Recipient, org string) {
	template, found, err := h.store.TemplateAtRevision(owner, p.TemplateID, p.TemplateRevision)
	if err != nil {
		h.retry(rw, err)
		return
	}
	if !found || !sameOrganization(template.AccountID, p.AccountID) || !parseTime(template.Revision).Equal(parseTime(p.TemplateRevision)) {
		h.unavailable(rw)
		return
	}
	w := Worker{cfg: h.cfg, logger: h.logger, now: h.now}
	links, err := w.messageLinks(linkRequest{OwnerUserID: p.OwnerUserID, RecipientID: p.RecipientID, CampaignID: p.CampaignID, AccountID: p.AccountID, EmailDigest: p.EmailDigest, TemplateID: p.TemplateID, TemplateRevision: p.TemplateRevision, SenderName: p.SenderName, CampaignName: p.CampaignName})
	if err != nil {
		h.retry(rw, err)
		return
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
func (h *EmailLinkHandler) subscribed(rw http.ResponseWriter, org string) {
	writeEmailPage(rw, 200, "Subscribed", `<h1>You're subscribed.</h1><p>You'll get `+emailsFrom(org)+`.</p>`)
}
func (h *EmailLinkHandler) cannotSend(rw http.ResponseWriter, org string) {
	writeEmailPage(rw, 200, "Email preferences", "<p>"+cannotSendText(org)+"</p>")
}
func (h *EmailLinkHandler) invalid(rw http.ResponseWriter) {
	writeEmailPage(rw, 400, "Link not valid", `<h1>This link isn't valid.</h1><p>It may have been changed in transit.</p>`)
}
func (h *EmailLinkHandler) unavailable(rw http.ResponseWriter) {
	writeEmailPage(rw, 404, "Email unavailable", `<h1>This email isn't available.</h1>`)
}
func (h *EmailLinkHandler) retry(rw http.ResponseWriter, err error) {
	h.logger.Error("could not complete email page request", "error", err)
	rw.Header().Set("Retry-After", "30")
	writeEmailPage(rw, 503, "Please try again", `<h1>Please try again.</h1><p>We couldn't save your choice. Try this link again in a moment.</p>`)
}

// A lost database session must stop the remaining writes of a multi-row choice.
// Keeping this wrapper request-local also covers the shared unsubscribe helper.
type emailPageGateEngine struct {
	Engine
	hold gateHold
}

func (e emailPageGateEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "mutation ") {
		if err := e.hold.Verify(ctx); err != nil {
			return nil, err
		}
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
		return memql.AcquireGateHold(ctx, db, 0, "campaign-journey:journey:"+bare(journeyID), "campaign-journey:recipient:"+bare(recipientID), "campaign-journey:enrollment:"+bare(enrollmentID))
	}
}

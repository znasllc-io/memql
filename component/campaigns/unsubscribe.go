package campaigns

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// unsubscribe.go -- the RFC 8058 one-click endpoint.
//
// # Why this is HTTP, when the house rule is gRPC-first
//
// It is the same exception `/inbound/{source}` is: THE THIRD PARTY DIALS
// US. RFC 8058 one-click is a contract with the recipient's MAIL CLIENT
// -- Gmail, Outlook, Yahoo -- which reads the `List-Unsubscribe` header
// and POSTs `List-Unsubscribe=One-Click` to the URI it finds there. There
// is no version of that conversation in which the mail provider speaks
// gRPC, and there is no way to offer one-click unsubscribe without it.
// The alternative is a `mailto:`-only List-Unsubscribe, which the same
// providers treat as not having one.
//
// It is declared in server.UnsubscribePaths() and reaches this handler
// through HandlerAuthorizedPaths + SelfAuthenticatedPaths, exactly as the
// inbound webhook does -- not through PublicPaths, because it is not
// public: it authorizes itself, on the signed token.
//
// # GET renders, POST acts, and the split is load-bearing
//
// A GET must never unsubscribe anyone. Mail clients, link scanners and
// corporate security appliances prefetch URLs found in messages, and a
// GET with a side effect means a scanner silently opts people out of mail
// they wanted. That is exactly why RFC 8058 specifies a POST. So:
//
//	GET   renders a one-button confirmation page (what a person clicking
//	      the link in the body reaches)
//	POST  performs the suppression (what the mail client's one-click
//	      button and that page's button both do)
//
// # What the endpoint trusts
//
// The token, and nothing else. It carries the owner, recipient and
// campaign ids and a MAC over all three; the handler verifies the MAC
// before it does anything, then resolves the ADDRESS by reading the
// recipient row under the owner the token names. No query parameter
// influences whose row is read, because the impersonated identity comes
// out of the signed payload.
//
// # Why it always answers "done"
//
// Every outcome after a valid token renders the same confirmation. An
// endpoint that distinguished "suppressed" from "recipient not found"
// would be an oracle for which recipient ids exist, reachable without
// credentials. And from the recipient's point of view the two are the
// same fact: they will not be mailed again. Only an INVALID token gets a
// different page, because that one needs a different next step from the
// person holding it.

// UnsubscribeHandler serves GET + POST on UnsubscribePath.
type UnsubscribeHandler struct {
	store  *Store
	cfg    Config
	logger *slog.Logger
	system func(ctx context.Context) context.Context
	now    func() time.Time
}

// NewUnsubscribeHandler builds the endpoint over the same store and
// system identity the worker uses.
func NewUnsubscribeHandler(engine Engine, cfg Config, logger *slog.Logger) *UnsubscribeHandler {
	if logger == nil {
		logger = slog.Default()
	}
	w := &Worker{}
	return &UnsubscribeHandler{
		store:  NewStore(engine),
		cfg:    cfg,
		logger: logger.With("component", "campaigns.unsubscribe"),
		system: w.systemActorContext,
		now:    time.Now,
	}
}

func (h *UnsubscribeHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.serveGet(rw, r)
	case http.MethodPost:
		h.servePost(rw, r)
	default:
		rw.Header().Set("Allow", "GET, POST")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *UnsubscribeHandler) serveGet(rw http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if _, _, _, err := ParseUnsubscribeToken(h.cfg.UnsubscribeSecret, token); err != nil {
		h.render(rw, http.StatusBadRequest, pageInvalid, "")
		return
	}
	h.render(rw, http.StatusOK, pageConfirm, token)
}

func (h *UnsubscribeHandler) servePost(rw http.ResponseWriter, r *http.Request) {
	// The token may arrive in the query (what the header URI carries) or
	// in the form body (what the confirmation page's button posts). Both
	// are read; the query wins, because that is the one the mail client
	// used and the one the MAC was minted for.
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		_ = r.ParseForm()
		token = strings.TrimSpace(r.PostFormValue("token"))
	}
	ownerUserID, recipientID, campaignID, err := ParseUnsubscribeToken(h.cfg.UnsubscribeSecret, token)
	if err != nil {
		h.render(rw, http.StatusBadRequest, pageInvalid, "")
		return
	}

	if err := h.suppress(r.Context(), ownerUserID, recipientID, campaignID); err != nil {
		// The recipient is told it worked and the operator is told it did
		// not. Retrying is the mail client's business and it will not; the
		// honest recovery is an operator noticing this log line, which is
		// why it is Error rather than Warn.
		h.logger.Error("campaigns: unsubscribe failed after a VALID token; this address is still mailable",
			"campaign", campaignID, "error", err)
	}
	h.render(rw, http.StatusOK, pageDone, "")
}

// suppress performs the two writes an opt-out is made of: the
// cluster-wide list (authoritative, consulted at every send) and the
// operator's own recipient row (their view of their audience).
//
// The recipient row is read FIRST because it is the only place the
// address lives -- the token deliberately does not carry it, so a leaked
// link discloses no mailbox.
func (h *UnsubscribeHandler) suppress(ctx context.Context, ownerUserID, recipientID, campaignID string) error {
	ownerCtx := ownerActorContext(ctx, ownerUserID)

	// Resolve the address. The campaign's audience is the roster to look
	// in; reading the campaign first keeps this to the two owned-tier
	// queries the domain already declares rather than adding a
	// recipient-by-id read whose only caller would be this line.
	var addr string
	if campaignID != "" {
		if campaign, found, err := h.store.CampaignByID(ownerCtx, campaignID); err == nil && found {
			if rec, ok, rerr := h.store.RecipientByID(ownerCtx, campaign.AudienceID, recipientID); rerr == nil && ok {
				addr = rec.Email
			}
		}
	}
	if addr == "" {
		// The recipient row is gone, or the campaign is. Nothing further
		// is possible: with no address there is no digest, and the
		// cluster list is keyed by digest. The row-level opt-out below
		// still runs, so the person is removed from the audience that
		// mailed them.
		if err := h.store.SetRecipientSubscription(ownerCtx, recipientID, "unsubscribed", h.now().UTC()); err != nil {
			return err
		}
		return nil
	}

	digest := EmailDigest(addr)
	if digest != "" {
		if err := h.store.RecordSuppression(h.system(ctx), digest, "unsubscribed", EmailDomain(addr), campaignID, ""); err != nil {
			return err
		}
	}
	return h.store.SetRecipientSubscription(ownerCtx, recipientID, "unsubscribed", h.now().UTC())
}

// ownerActorContext is the package-level twin of the worker's method, so
// the handler does not need a Worker to borrow an identity.
func ownerActorContext(ctx context.Context, ownerUserID string) context.Context {
	return (&Worker{}).ownerActorContext(ctx, ownerUserID)
}

type unsubscribePage int

const (
	pageConfirm unsubscribePage = iota
	pageDone
	pageInvalid
)

// render writes a minimal self-contained page. No external assets: this
// is rendered in a browser reached from a link in an email, often on a
// network that will not load a stylesheet from anywhere else.
func (h *UnsubscribeHandler) render(rw http.ResponseWriter, status int, page unsubscribePage, token string) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	// A page reached from an email is a page an attacker would like to
	// frame or to have a script injected into. It renders nothing but
	// fixed text and one escaped token, and says so to the browser too.
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.WriteHeader(status)

	var body string
	switch page {
	case pageConfirm:
		body = `<h1>Unsubscribe</h1><p>Confirm that you no longer want to receive these emails.</p>` +
			`<form method="POST"><input type="hidden" name="token" value="` + htmlEscape(token) + `">` +
			`<button type="submit">Unsubscribe me</button></form>`
	case pageDone:
		body = `<h1>Unsubscribed</h1><p>You will not receive further emails from this sender. ` +
			`It may take a short time for a send already in progress to stop.</p>`
	default:
		body = `<h1>This link is not valid</h1><p>The unsubscribe link could not be verified. ` +
			`It may have been altered in transit. Replying to the message and asking to be removed will also work.</p>`
	}
	_, _ = rw.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>Unsubscribe</title>` +
		`<style>body{font:16px/1.5 system-ui,sans-serif;margin:4rem auto;max-width:34rem;padding:0 1rem}` +
		`button{font:inherit;padding:.6rem 1.1rem;cursor:pointer}</style></head><body>` +
		body + `</body></html>`))
}

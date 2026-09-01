package campaigns

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// tracking.go -- the open and click endpoints (memql#4823, design D13).
//
// # Why these are HTTP, when the house rule is gRPC-first
//
// The same exception /unsubscribe is, and for the same reason stated in the
// same words: THE THIRD PARTY DIALS US. Here the third party is the
// recipient's MAIL CLIENT fetching an image out of a message body, and their
// BROWSER following a link in one. An <img src> is a GET or it is not a
// pixel; a rewritten href is a URL or it is not a link. There is no version
// of either conversation in which the other side speaks gRPC.
//
// They are declared in server.TrackingPaths() and reach this handler through
// HandlerAuthorizedPaths + SelfAuthenticatedPaths, exactly as the unsubscribe
// endpoint does -- not through PublicPaths, because they are not public: they
// authorize themselves, on the signed token.
//
// # NEITHER ENDPOINT EVER FAILS VISIBLY, and that is the whole posture
//
// Every branch below ends in one of two answers, and no branch ends in a 500:
//
//	GET /t/o/{token}   ALWAYS a 1x1 GIF, 200, whatever happened. A tracking
//	                   pixel that errors is a BROKEN IMAGE ICON in the middle
//	                   of somebody's message -- the recipient sees our defect
//	                   rendered inside the operator's campaign, and there is
//	                   no version of "we could not count this open" worth
//	                   showing a person.
//	GET /t/c/{token}   a 302 to the SIGNED target on a valid token, and
//	                   otherwise the same plain "link not valid" page an
//	                   altered unsubscribe link gets. Never a redirect to an
//	                   unverified URL, and never a 500: the person clicked a
//	                   link in an email and is owed a page, not a stack of
//	                   ours.
//
// The consequence is that a failed WRITE is invisible to the recipient and
// visible only in the log, which is correct: recording an open is our
// bookkeeping and their message is not the place to report on it.
//
// # THE SIGNATURE OVER THE URL IS THE OPEN-REDIRECT DEFENCE
//
// The click endpoint redirects to a target it was handed, which is the
// textbook shape of an open redirect. What makes it safe is that the target
// is inside the MAC'd body: a tampered or invented URL does not redirect
// anywhere. There is deliberately no host allowlist and no scheme filter at
// this end -- the campaign author put the link in the message, and the
// signature is the statement that we are the ones who put it there. Adding a
// filter here would refuse links operators legitimately send while doing
// nothing the signature does not already do.
//
// # Whose row is written, and how the owner is established
//
// v1:campaigns:engagementEvent is owner-tier, and the request carries no
// actor at all -- both callers are unauthenticated by construction. So the
// owner comes out of the SIGNED payload's campaign id, resolved through the
// engine's own clusterOwner-tier send-job row (whose id IS the campaign's
// bare short id) under the engine identity, exactly as the worker does. No
// query parameter influences whose row is written, because nothing about the
// request does.
//
// The mutation is additionally @serverOnly, so the write also carries
// internal origin -- Store.execServerOnly. Neither gate would be sufficient
// alone: the tier without the annotation would let any authenticated caller
// inflate their own campaign's numbers over the wire, and the annotation
// without the tier would leave the row owned by nobody.

// TrackingHandler serves GET on TrackingOpenPath and TrackingClickPath.
type TrackingHandler struct {
	store  *Store
	cfg    Config
	logger *slog.Logger
	system func(ctx context.Context) context.Context
	now    func() time.Time
}

// NewTrackingHandler builds the endpoints over the same store and system
// identity the worker and the unsubscribe handler use.
func NewTrackingHandler(engine Engine, cfg Config, logger *slog.Logger) *TrackingHandler {
	if logger == nil {
		logger = slog.Default()
	}
	w := &Worker{}
	return &TrackingHandler{
		store:  NewStore(engine),
		cfg:    cfg,
		logger: logger.With("component", "campaigns.tracking"),
		system: w.systemActorContext,
		now:    time.Now,
	}
}

// trackingPixel is a 1x1 fully transparent GIF.
//
// Decoded from base64 at init rather than written as a byte literal: the
// bytes include several control values, and this tree does not put raw
// control bytes in source. A GIF rather than a PNG because it is the smaller
// of the two at this size (43 bytes) and because every mail client that
// renders images at all renders it.
var trackingPixel = func() []byte {
	raw, err := base64.StdEncoding.DecodeString(
		"R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")
	if err != nil {
		// Unreachable: the literal is fixed. An empty body still answers 200
		// with the right content type, which is a blank image rather than a
		// broken one.
		return nil
	}
	return raw
}()

func (h *TrackingHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		// GET ONLY, deliberately. A pixel is fetched by an <img src> and a
		// rewritten link is followed by a navigation; nothing else ever
		// arrives. Answering anything else would be declaring a
		// state-changing form that does not exist.
		rw.Header().Set("Allow", "GET")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch {
	case pathCarries(r.URL.Path, TrackingOpenPath):
		h.serveOpen(rw, r)
	case pathCarries(r.URL.Path, TrackingClickPath):
		h.serveClick(rw, r)
	default:
		// Reached only if the mount and these constants disagree, which is
		// the failure server.TrackingPaths' own doc warns about. A 404 rather
		// than a pixel: there is no token to verify and nothing to record.
		http.NotFound(rw, r)
	}
}

// serveOpen records an open and ALWAYS answers the pixel.
func (h *TrackingHandler) serveOpen(rw http.ResponseWriter, r *http.Request) {
	if payload, ok := h.verify(tokenFromPath(r.URL.Path, TrackingOpenPath)); ok && payload.Kind == EngagementOpen {
		h.record(r.Context(), payload)
	}
	// Unconditional. An invalid token, a missing campaign, a refused write --
	// all of them answer the same image, because the alternative is rendered
	// inside a person's inbox.
	h.writePixel(rw)
}

// serveClick redirects to the SIGNED target, or renders the invalid page.
func (h *TrackingHandler) serveClick(rw http.ResponseWriter, r *http.Request) {
	payload, ok := h.verify(tokenFromPath(r.URL.Path, TrackingClickPath))
	if !ok || payload.Kind != EngagementClick || !isTrackableURL(payload.URL) {
		// isTrackableURL is re-checked on the way OUT as well as on the way
		// in. The signature already guarantees we minted the target, but a
		// Location header is the one place a scheme this package never
		// intended to emit would be acted on by a browser -- so the
		// http(s)-only rule is asserted at the sink rather than inferred
		// from the source.
		h.renderInvalid(rw)
		return
	}
	h.record(r.Context(), payload)

	h.securityHeaders(rw)
	rw.Header().Set("Cache-Control", "no-store")
	// 302 rather than 301: a permanent redirect is CACHED BY THE BROWSER, so
	// a second click on the same link would never reach us and the count
	// would silently stop at one per recipient per device.
	http.Redirect(rw, r, payload.URL, http.StatusFound)
}

// verify parses a token and reports the one failure an operator can act on.
//
// A refused token is always the same answer to the recipient. The split is
// for the LOG, exactly as on the unsubscribe path: "the MAC did not match" is
// a prober, while "this node holds no key with that id" is a rotation that
// dropped a secret which had already signed live links -- invisible from our
// side and maximally visible from theirs. Neither branch logs the token.
func (h *TrackingHandler) verify(token string) (TrackingPayload, bool) {
	if token == "" {
		return TrackingPayload{}, false
	}
	payload, err := ParseTrackingToken(h.cfg.UnsubscribeKeys(), token)
	if err != nil {
		if errors.Is(err, errUnknownTrackingKey) {
			h.logger.Warn("campaigns: refused a tracking link signed by a key this node no longer holds; "+
				"the signing secret was rotated without keeping the old value in "+
				"MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET_PREVIOUS, so opens and clicks from every message "+
				"already sent are being dropped",
				"keyId", trackingKeyIDOf(token))
		}
		return TrackingPayload{}, false
	}
	return payload, true
}

// record writes the engagement row under the campaign owner's actor.
//
// Best-effort by construction: every failure is a log line and nothing else,
// because the response is already decided. A lost open is a number; a broken
// image or an error page is something a person sees.
func (h *TrackingHandler) record(ctx context.Context, payload TrackingPayload) {
	owner, found, err := h.store.CampaignOwnerForSend(h.system(ctx), payload.CampaignID)
	if err != nil {
		h.logger.Warn("campaigns: could not resolve the owner behind a tracked campaign",
			"campaign", payload.CampaignID, "error", err)
		return
	}
	if !found || owner == "" {
		// A signed token naming a campaign with no send job. Not an attack --
		// the signature held -- so the likeliest cause is a job row removed
		// after the campaign mailed. Debug rather than Warn: there is nothing
		// for an operator to do, and a line per pixel fetch would drown the
		// ones that matter.
		h.logger.Debug("campaigns: a tracked campaign has no send job, so its owner cannot be resolved",
			"campaign", payload.CampaignID)
		return
	}
	if err := h.store.RecordEngagementEvent(ownerActorContext(ctx, owner), EngagementEvent{
		CampaignID: payload.CampaignID,
		DeliveryID: payload.DeliveryID,
		Kind:       payload.Kind,
		URL:        payload.URL,
		OccurredAt: h.now().UTC(),
	}); err != nil {
		h.logger.Warn("campaigns: could not record an engagement event",
			"campaign", payload.CampaignID, "kind", payload.Kind, "error", err)
	}
}

func (h *TrackingHandler) writePixel(rw http.ResponseWriter) {
	h.securityHeaders(rw)
	rw.Header().Set("Content-Type", "image/gif")
	// no-store, so a client that fetches the message twice reports two opens
	// rather than one. Over-counting a re-read is the honest error here:
	// under-counting would make "opens" mean "first opens on a client that
	// did not cache", which is not a number anybody could interpret.
	rw.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	rw.Header().Set("Pragma", "no-cache")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(trackingPixel)
}

// renderInvalid is deliberately the SAME page an altered unsubscribe link
// gets: one wording for "this link could not be verified", so a recipient who
// meets both never has to work out which of our two mechanisms failed.
func (h *TrackingHandler) renderInvalid(rw http.ResponseWriter) {
	h.securityHeaders(rw)
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(http.StatusBadRequest)
	_, _ = rw.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>Link not valid</title>` +
		`<style>body{font:16px/1.5 system-ui,sans-serif;margin:4rem auto;max-width:34rem;padding:0 1rem}</style>` +
		`</head><body><h1>This link is not valid</h1><p>The link could not be verified. ` +
		`It may have been altered in transit. Opening the message again and clicking the link there will also work.</p>` +
		`</body></html>`))
}

// securityHeaders are the unsubscribe endpoint's, unchanged. A page or an
// image reached from a link in an email is a thing an attacker would like to
// frame or to have a script injected into; neither response here renders
// anything from the wire, and both say so to the browser too.
func (h *TrackingHandler) securityHeaders(rw http.ResponseWriter) {
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	rw.Header().Set("Referrer-Policy", "no-referrer")
}

// pathCarries reports whether a request path is under one of the two mounts,
// tolerating the base prefix a deployment served under
// MEMQL_SERVER_PUBLIC_PATH carries.
func pathCarries(path, mount string) bool {
	return strings.Contains(path, mount)
}

// tokenFromPath lifts the token segment out of the request path.
//
// ONE SEGMENT, enforced here rather than assumed. The self-authenticated
// bypass that lets an unauthenticated request reach this handler is bounded
// to a single further segment under the mount, so a path with more is a path
// the verifier would have refused -- treating it as a token would make this
// handler's behaviour depend on how it was reached.
func tokenFromPath(path, mount string) string {
	i := strings.Index(path, mount)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(path[i+len(mount):])
	if rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

// CampaignOwnerForSend resolves which user owns a campaign, WITHOUT reading
// the campaign row.
//
// It cannot read the campaign row: that row is owner-tier, and establishing
// the owner is the very thing being asked. So it reads the engine's own
// clusterOwner-tier send job, whose id IS the campaign's bare short id, and
// takes campaignOwnerUserId off it -- a value that was itself copied off a
// campaign row the STARTING CALLER had already read under their own actor.
// The chain of custody is the same one the drain worker relies on, and it is
// why no caller can aim this at a user they could not act as.
//
// clusterOwner-tier: issue under the engine's operator identity.
func (s *Store) CampaignOwnerForSend(ctx context.Context, campaignID string) (string, bool, error) {
	rows, err := s.rows(ctx, call("query", "sendJobById", arg{"sendJobId", campaignID}))
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	owner := bare(str(rows[len(rows)-1], "campaignOwnerUserId"))
	return owner, owner != "", nil
}

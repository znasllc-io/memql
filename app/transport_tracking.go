//go:build bff

package app

import (
	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/server"
)

// mountTrackingEndpoints wires the campaign open- and click-tracking
// endpoints (GET /t/o/{token} and GET /t/c/{token}, memql#4823) onto the
// bff's mux.
//
// The bff only, and for the same reason /unsubscribe and the inbound
// receiver are bff-only: this is the node an ingress already routes external
// traffic to, and the caller here is a third party -- the recipient's mail
// client fetching a pixel out of a message we sent, or their browser
// following a link we rewrote. No internal node has any reason to carry it.
//
// GET ONLY, both paths, and there is deliberately no POST. A pixel is
// fetched by an <img src> and a rewritten link is followed by a navigation;
// both are GETs and nothing else ever arrives. Registering a POST would be
// declaring a state-changing form that does not exist.
//
// # The pixel WRITES on a GET, and that is a considered exception
//
// The usual rule is the one app/transport_unsubscribe.go states in its own
// mount: a GET must never have the side effect, because link scanners and
// mail-client prefetchers issue GETs and would perform it for people who
// never clicked. That rule is suspended here on three specific grounds, not
// waived generally:
//
//   - The write is an APPEND-ONLY OBSERVATION, not a decision. A
//     v1:campaigns:engagementEvent row records that a fetch happened; it
//     changes no subscription, revokes no consent and is not read back as
//     an instruction. The failure a prefetch causes is an overcounted open,
//     not an unsubscribed recipient -- which is the whole reason
//     /unsubscribe splits GET from POST and this does not.
//   - The write is KEYED TO A SIGNED TOKEN, so a prefetcher can only ever
//     record against the (delivery, campaign) the token already names. There
//     is no unsigned input to aim, and an unsigned or tampered request
//     records nothing at all.
//   - The caller CANNOT issue anything else. A mail client will not POST to
//     an image URL, so a design that demanded a POST here would be a design
//     with no open tracking in it. This is the same "the other party
//     dictates the wire" reasoning that put the endpoint on HTTP in the
//     first place.
//
// Overcounting from prefetch is therefore a known, bounded property of open
// tracking as an industry practice rather than a defect this mount
// introduces; stats distinguish unique from total by (delivery, kind) for
// exactly that reason.
//
// It always mounts, like the unsubscribe endpoint beside it. Without
// MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET no token can verify, so every request
// takes the invalid-signature branch and nothing is recorded -- and no
// campaign can have been SENT either, because campaignStartSend refuses
// without the same secret. A conditional mount would make "is the secret
// set" and "does the route exist" two different questions with one 404
// answer, and here that answer would reach a person's inbox as a broken
// image rather than as a log line.
func (a *App) mountTrackingEndpoints() {
	cfg := campaigns.LoadConfig()
	handler := campaigns.NewTrackingHandler(&CampaignsEngineAdapter{Engine: a.engine}, cfg, a.Logger)
	// Iterated from the server declaration rather than from
	// campaigns.TrackingOpenPath / TrackingClickPath so the mount and the
	// front-door rule cannot disagree: server.TrackingPaths() is what
	// cmd/frontdoorpaths reads, and a path routed by the ingress but not
	// mounted here is a 404 in somebody's inbox.
	for _, path := range server.TrackingPaths() {
		a.handleRoute("GET "+path, handler)
	}
}

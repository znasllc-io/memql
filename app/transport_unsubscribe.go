//go:build bff

package app

import (
	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/server"
)

// mountUnsubscribeEndpoint wires the RFC 8058 one-click unsubscribe
// endpoint (GET+POST /unsubscribe, memql#3348) onto the bff's mux.
//
// The bff only, and for the same reason the inbound receiver is bff-only:
// this is the node an ingress already routes external traffic to, and the
// caller here is a third party -- the recipient's mail client, following
// the `List-Unsubscribe` URI in a message we sent. There is no reason an
// internal node should carry it.
//
// It always mounts. Without MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET no token
// can verify, so every request renders the "not valid" page and no write
// happens -- and no campaign can have been STARTED either, because
// campaignStartSend refuses without the same secret. A conditional mount
// would make "is the secret set" and "does the route exist" two different
// questions with the same 404 answer.
func (a *App) mountUnsubscribeEndpoint() {
	cfg := campaigns.LoadConfig()
	handler := campaigns.NewUnsubscribeHandler(&CampaignsEngineAdapter{Engine: a.engine}, cfg, a.Logger)
	for _, path := range server.UnsubscribePaths() {
		// GET renders the confirmation page a person reaches from the link
		// in the body; POST is what performs the opt-out, both for that
		// page's button and for the mail client's one-click. A GET must
		// never have the side effect -- link scanners prefetch.
		a.handleRoute("GET "+path, handler)
		a.handleRoute("POST "+path, handler)
	}
	if reason := cfg.RequireUnsubscribe(); reason != "" {
		a.Logger.Info("unsubscribe endpoint mounted, but campaign sending is disabled", "reason", reason)
	}
}

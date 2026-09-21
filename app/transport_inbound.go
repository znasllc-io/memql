//go:build bff

package app

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/inbound"
	"github.com/znasllc-io/memql/component/server"
)

// mountInboundEndpoints wires the inbound-delivery receiver
// (POST /inbound/{source}, memql#2957) onto the bff's mux: the counterpart to
// the outbound worker app/engine.go starts, and the seam a product needs to
// INGEST from a third party without standing up its own sidecar.
//
// The bff only, deliberately. This is the frontend-facing node an ingress
// already routes external traffic to, and there is no reason for an internal
// node to carry a route a third party dials. Adding it elsewhere would widen
// the externally-reachable surface for nothing.
//
// It always mounts. When nothing is configured -- the default -- the receiver
// resolves an empty source allowlist and answers 404 to everything but the one
// source a cluster can register for itself (registeredGitHubWebhook), so
// mounting unconditionally costs a route that admits nobody. That is preferable
// to mounting conditionally: a conditional mount makes "did the operator set
// the env" and "is the endpoint there" two different questions, and the
// difference only shows up as a 404 in production either way.
func (a *App) mountInboundEndpoints() {
	cfg := inbound.LoadConfig(a.Logger)
	handler := inbound.NewHandler(cfg, &InboundEngineAdapter{Engine: a.engine}, a.Logger)
	handler.SetRegisteredSource(a.registeredGitHubWebhook())
	for _, path := range server.InboundWebhookPaths() {
		a.handleRoute("POST "+path, handler)
	}
	if len(cfg.Sources) == 0 {
		// "Every request" stopped being true when a cluster could register a
		// GitHub App for itself: that one source is admitted from the app's own
		// stored secret, decided per request, so whether it applies is not
		// something this line can know at boot. It says so rather than promise
		// a 404 that a cluster with a registered app does not answer.
		a.Logger.Info("inbound receiver mounted with no sources configured in the environment; every request " +
			"is refused with 404 except the webhook of a GitHub App registered from the product " +
			"(set MEMQL_INBOUND_SOURCE_ALLOWLIST to enable others)")
		return
	}
	names := make([]string, 0, len(cfg.Sources))
	for name := range cfg.Sources {
		names = append(names, name)
	}
	a.Logger.Info("inbound receiver mounted", "sources", names)
}

// githubWebhookSource is the last segment of the URL the cluster's GitHub App
// posts its webhook to (githubconnect.WebhookPath), which is also the source
// the packages pipeline listens on by default. The test beside this file pins
// the three spellings together.
var githubWebhookSource = strings.TrimPrefix(githubconnect.WebhookPath, inbound.RoutePrefix)

// registeredGitHubWebhook answers the inbound policy for a GitHub App that a
// cluster owner REGISTERED FROM THE PRODUCT (design record
// 2026-09-20-github-app-setup, D5).
//
// GitHub generates the app's webhook secret while it creates the app, so on
// such a cluster no environment ever held it: it is one of the six rows the
// registration stored. Without this, an app registered from the product on a
// public domain would post pushes to a receiver that answers 404 -- the seam
// is deny-by-default -- and the update cue would fall back to the ten-minute
// poll for ever, with GitHub's own delivery log the only place saying why.
//
// ONLY FOR A REGISTERED APP, never for one the environment configured. An
// operator who set the six by hand also sets MEMQL_INBOUND_SOURCE_GITHUB_*
// by hand (docs/public/operate/github-connect.md), and that allowlist is a
// deliberate opt-in this must not make for them: turning the receiver on for
// every cluster that happens to have an app would change what a running
// deployment accepts, with nothing in its environment changed to say so. The
// inbound handler puts this tier BELOW the environment for the same reason.
//
// The policy is GitHub's, and fixed: HMAC-SHA256 of the body, hex, in
// X-Hub-Signature-256 behind a `sha256=` prefix, with X-GitHub-Delivery as the
// idempotency key that makes a redelivery free.
func (a *App) registeredGitHubWebhook() inbound.RegisteredSource {
	if a.engine == nil {
		return nil
	}
	resolver := &githubconnect.Resolver{Rows: githubconnect.RowReader{
		Variable: a.engine.ResolveSystemVariable,
		Secret:   a.engine.ResolveSystemSecret,
	}}
	return func(ctx context.Context, name string) (inbound.SourceConfig, bool) {
		if name != githubWebhookSource {
			return inbound.SourceConfig{}, false
		}
		cfg, source := resolver.Current(ctx)
		if source != githubconnect.SourceCluster || !cfg.Configured() {
			return inbound.SourceConfig{}, false
		}
		return inbound.SourceConfig{
			Secret:          cfg.WebhookSecret,
			Scheme:          inbound.SchemeHMACSHA256Hex,
			SignatureHeader: "X-Hub-Signature-256",
			SignaturePrefix: "sha256=",
			DedupeHeader:    "X-GitHub-Delivery",
		}, true
	}
}

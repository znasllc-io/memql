//go:build bff

package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/inbound"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/calls"
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
// resolves an empty source allowlist and answers 404 to everything but the two
// sources a cluster registers for itself (registeredGitHubWebhook and
// registeredLiveKitWebhook), so mounting unconditionally costs a route that
// admits nobody. That is preferable to mounting conditionally: a conditional
// mount makes "did the operator set the env" and "is the endpoint there" two
// different questions, and the difference only shows up as a 404 in production
// either way.
func (a *App) mountInboundEndpoints() {
	cfg := withGitHubPayloadCap(inbound.LoadConfig(a.Logger))
	handler := inbound.NewHandler(cfg, &InboundEngineAdapter{Engine: a.engine}, a.Logger)
	handler.SetRegisteredSource(firstRegisteredSource(a.registeredGitHubWebhook(), a.registeredLiveKitWebhook()))
	for _, path := range server.InboundWebhookPaths() {
		a.handleRoute("POST "+path, handler)
	}
	if len(cfg.Sources) == 0 {
		// "Every request" stopped being true when a cluster could register a
		// source for itself: a GitHub App's, and the calls media server's, are
		// each admitted from a secret the cluster stored, decided per request, so
		// whether either applies is not something this line can know at boot. It
		// says so rather than promise a 404 that such a cluster does not answer.
		a.Logger.Info("inbound receiver mounted with no sources configured in the environment; every request " +
			"is refused with 404 except the webhook of a GitHub App registered from the product and the calls " +
			"media server's (set MEMQL_INBOUND_SOURCE_ALLOWLIST to enable others)")
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
		return githubWebhookPolicy(cfg.WebhookSecret), true
	}
}

// githubDeliveryHeaders is what a GitHub delivery stages as `headersJson`.
//
// NAMED, rather than left empty, because the event is not in the body:
// GitHub says WHICH event a delivery is (push, ping, installation) only in
// X-GitHub-Event, so a row staged with `headersJson: "{}"` cannot tell a
// reader what arrived (memql#5707 residual). The delivery id correlates the
// row with GitHub's own delivery log, and the hook id says which of the
// app's webhooks sent it. None is a credential; the signature header is
// refused by the receiver whatever a list says.
//
// The ENV-configured source stages the same three only when an operator sets
// MEMQL_INBOUND_SOURCE_GITHUB_FORWARD_HEADERS, which github-connect.md's
// example does -- TestTheGitHubDeliveryHeadersAreOneListInTwoPaths holds the
// two together.
var githubDeliveryHeaders = []string{"X-GitHub-Event", "X-GitHub-Delivery", "X-GitHub-Hook-ID"}

// githubMaxPayloadBytes is the cap GitHub's source reads deliveries under: the
// most GitHub sends. GitHub caps a webhook payload at 25 MB and delivers no
// larger one, so this is the one cap for its source that cannot refuse a real
// delivery. The receiver's 256 KiB default did: a push payload lists every
// pushed commit with its changed files, and a push of a long branch went over
// it, was answered 413, staged nothing, and opened no run. 25 MiB rather than
// 25,000,000 so either reading of "MB" fits; the front door's proxy-body-size
// (48m, cmd/frontdoorhosts) is above both.
const githubMaxPayloadBytes = 25 << 20

// withGitHubPayloadCap gives the ENV-configured GitHub source GitHub's payload
// cap when its operator set none (MEMQL_INBOUND_SOURCE_GITHUB_MAX_BODY_BYTES).
//
// NOT AN OPT-IN. registeredGitHubWebhook will not admit the env-configured
// source for an operator, because admitting a sender is the operator's
// statement to make. This admits nothing: it says how large a delivery from a
// sender the operator already admitted may be, and the operator's own value,
// when there is one, wins. Leaving GitHub's source at the generic default is
// not neutral -- it is a cap that refuses some of GitHub's real deliveries.
//
// A new Config, so the one LoadConfig returned is not changed under its caller.
func withGitHubPayloadCap(cfg inbound.Config) inbound.Config {
	src, ok := cfg.Sources[githubWebhookSource]
	if !ok || src.MaxBodyBytes > 0 {
		return cfg
	}
	sources := make(map[string]inbound.SourceConfig, len(cfg.Sources))
	for name, s := range cfg.Sources {
		sources[name] = s
	}
	src.MaxBodyBytes = githubMaxPayloadBytes
	sources[githubWebhookSource] = src
	cfg.Sources = sources
	return cfg
}

// githubWebhookPolicy is the fixed policy for a registered GitHub App's
// webhook, given the secret GitHub generated for it.
func githubWebhookPolicy(secret string) inbound.SourceConfig {
	return inbound.SourceConfig{
		Secret:          secret,
		Scheme:          inbound.SchemeHMACSHA256Hex,
		SignatureHeader: "X-Hub-Signature-256",
		SignaturePrefix: "sha256=",
		DedupeHeader:    "X-GitHub-Delivery",
		ForwardHeaders:  append([]string(nil), githubDeliveryHeaders...),
		MaxBodyBytes:    githubMaxPayloadBytes,
	}
}

// firstRegisteredSource composes the sources the cluster registers for itself
// into the ONE resolver the receiver holds: inbound.Handler.SetRegisteredSource
// takes a single function, and a second call replaces the first, so wiring the
// GitHub App's and the calls media server's one after the other would silently
// leave only the last. Each is asked in turn and the first that answers a name
// decides it; each answers its own name only, so their order decides nothing. A
// nil resolver is skipped, and none at all composes to nil, which leaves the
// receiver exactly as it was with no registered tier.
func firstRegisteredSource(resolvers ...inbound.RegisteredSource) inbound.RegisteredSource {
	var wired []inbound.RegisteredSource
	for _, resolve := range resolvers {
		if resolve != nil {
			wired = append(wired, resolve)
		}
	}
	if len(wired) == 0 {
		return nil
	}
	return func(ctx context.Context, name string) (inbound.SourceConfig, bool) {
		for _, resolve := range wired {
			if src, ok := resolve(ctx, name); ok {
				return src, true
			}
		}
		return inbound.SourceConfig{}, false
	}
}

// liveKitWebhookSource is the last segment of the URL the calls media server
// posts its webhooks to (POST /inbound/livekit), and the source the calls
// automation that applies them filters on: integrations/calls WebhookSource,
// spelled once.
const liveKitWebhookSource = calls.WebhookSource

// registeredLiveKitWebhook answers the inbound policy for the calls media
// server's webhooks (integrations/calls events.go).
//
// REGISTERED, NOT ALLOWLISTED. The source is admitted with the key pair the
// calls media bootstrap sealed as calls.media.webhook from
// MEMQL_CALLS_MEDIA_WEBHOOK_KEY and MEMQL_CALLS_MEDIA_WEBHOOK_SECRET
// (integrations/calls config.go). The operator configures the media server
// once, there; a MEMQL_INBOUND_SOURCE_LIVEKIT_* policy would be a second copy
// of the same secret, free to drift from the first. resolveSource asks this
// tier whatever MEMQL_INBOUND_SOURCE_ALLOWLIST says, so no deployment lists the
// name, and a cluster with no media server answers 404 for it as for any
// unknown source. An operator who does pin `livekit` in the environment wins
// over this (resolveSource, ENV WINS), and the calls builtin then refuses every
// delivery that pin verifies, because a secret other than the media server's
// did (events.go handleApplyMediaEvent).
func (a *App) registeredLiveKitWebhook() inbound.RegisteredSource {
	if a.engine == nil {
		return nil
	}
	return liveKitWebhookResolver(a.engine.ResolveSystemSecret, a.Logger)
}

// liveKitWebhookResolver resolves the calls media server's source from the
// sealed credential, read through secret on every request, so a credential the
// bootstrap replaced (a webhook secret rotated in the environment and a
// restart) verifies from the next request on every replica. A cluster with no
// media server has no credential, and the source is not answered, silently,
// since an unconfigured cluster is the common case. One whose credential cannot
// be used -- not decryptable under this node's MEMQL_MASTER_KEY, not a key pair,
// a lookup that failed -- is not answered either, and the node says why in
// words that name no value.
func liveKitWebhookResolver(secret func(ctx context.Context, name string) (string, error), logger *slog.Logger) inbound.RegisteredSource {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	refused := func(name, reason string) (inbound.SourceConfig, bool) {
		logger.Error("inbound receiver: the calls media server's webhook credential cannot be used, refusing its webhooks with 404",
			"source", name, "secret", calls.SecretMediaWebhook, "reason", reason)
		return inbound.SourceConfig{}, false
	}
	return func(ctx context.Context, name string) (inbound.SourceConfig, bool) {
		if name != liveKitWebhookSource {
			return inbound.SourceConfig{}, false
		}
		plaintext, err := secret(ctx, calls.SecretMediaWebhook)
		switch {
		case memql.IsVariableNotFound(err):
			return inbound.SourceConfig{}, false
		case memql.IsSecretUndecryptable(err):
			return refused(name, "it cannot be decrypted with this node's MEMQL_MASTER_KEY")
		case err != nil:
			return refused(name, "it could not be read")
		}
		cred, err := calls.ParseMediaCredential(plaintext)
		if err != nil {
			// ParseMediaCredential's errors never repeat the value.
			return refused(name, err.Error())
		}
		return liveKitWebhookPolicy(cred), true
	}
}

// liveKitWebhookPolicy is the fixed policy for the calls media server's
// webhooks, given its credential: a LiveKit server signs each body's SHA-256
// into a JWT under its webhook key pair -- the key as the issuer, the secret as
// the HS256 key -- and sends the token as the bare value of Authorization
// (webhook/url_notifier.go send).
//
// NO DEDUPE HEADER AND NO TIMESTAMP HEADER, so the row's identity is the body's
// digest alone (identityKeyFor) and a retry, which LiveKit's client sends as
// the same bytes, lands on the row the first delivery made. Two events are
// never one body: each carries its own id and time. No forwarded headers
// either: the whole event is in the body.
func liveKitWebhookPolicy(cred calls.MediaCredential) inbound.SourceConfig {
	return inbound.SourceConfig{
		Secret:          cred.Secret,
		Scheme:          inbound.SchemeJWTHS256BodySHA256,
		SignatureHeader: "Authorization",
		JWTIssuer:       cred.Key,
	}
}

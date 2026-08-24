package memql

// provider_reload_propagate.go -- LIVE cross-node re-resolution of AI provider
// auth (epic memql#4440, design D5).
//
// WHAT THIS EXISTS FOR. Provider auth resolves ONCE, at boot, on each node
// independently (globalSecret -> globalVariable -> env). So an operator who
// seeds a key through the portal has changed a row that no running process
// will read again. Before this, the only way to make a seeded key take effect
// was to restart every pod -- which is why the wizard collected the key up
// front and why installation demanded one at all.
//
// THE MULTI-NODE RULE IS THE WHOLE DESIGN HERE. `ReloadAIProviders` acts on
// ONE process. The portal's Apply reaches whichever replica the front door
// routed it to, so a reload that stopped there would leave a fleet where some
// replicas can call Anthropic and some cannot, and every symptom -- "it works
// when I refresh, then it doesn't" -- would point at the vendor rather than at
// us. Nothing travels implicitly between nodes, so the fan-out is explicit:
//
//   - `providersReload` publishes providers.reload.<requestId> (below);
//   - a single broadcast routing rule (providers.reload.*, component/node/
//     routing.go) forwards it to EVERY node with zero side effects -- only
//     this subscriber consumes it, no automations;
//   - every node's StartProvidersReloadSubscriber runs the safe, build-then-
//     swap reload against its OWN registry and logs its own outcome.
//
// Modelled exactly on authoring.promote.* / cache.invalidate.* -- a
// single-consumer broadcast topic. The event carries no credential and no
// provider data: each node re-reads the shared graph itself.
//
// NO AUTOMATIC RELOAD ON EVERY globalSecret WRITE, deliberately. Rotation is a
// decision with a blast radius -- a half-typed key saved into the box would
// otherwise take down every provider on every node the moment it was saved --
// so Apply is an explicit act, with an audit line naming who asked.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// providersReloadPattern is what every node subscribes to. `#` rather than `*`
// so a future topic segment cannot silently stop matching.
const providersReloadPattern = "providers.reload.#"

// AuditActionProvidersReloaded is the audit action stamped when an operator
// applies a provider reload. Named to match the v1:identity:auditEvent.action
// convention the authored-construct actions use.
const AuditActionProvidersReloaded = "providers_reloaded"

// PublishProvidersReload broadcasts a reload request to every node.
//
// The requestId is the topic's trailing segment, which is what makes two
// Applies in quick succession two distinct events rather than one the bus may
// coalesce -- and what lets a node's log line be tied back to the click that
// caused it.
func (e *MemQLEngine) PublishProvidersReload(requestId string) {
	if e == nil || e.eventBus == nil {
		return
	}
	e.eventBus.Publish(events.NewEvent(
		events.TopicProvidersReloadFor(requestId),
		events.KindProvidersReload,
		map[string]any{"requestId": requestId},
	))
}

// StartProvidersReloadSubscriber subscribes this node to the providers-reload
// broadcast. On every event -- local or bridged from a peer -- it re-resolves
// provider auth against the shared graph and swaps its registry's contents
// atomically.
//
// The subscription is scoped to ctx: when ctx is cancelled (engine stop) the
// unsubscribe runs and the handler stops firing. Exported so the cross-node
// hop test can wire two engines onto two buses and prove a reload requested on
// one takes effect on the other.
//
// A FAILED RELOAD IS LOGGED, NOT RETRIED. `ReloadAIProviders` leaves the live
// registry untouched when it cannot build a replacement, so the node carries
// on serving what it was already serving; retrying on a timer would hammer
// concept storage on every replica over a condition an operator has to fix
// anyway, and the portal shows the per-node truth through providerAuthStatus.
func (e *MemQLEngine) StartProvidersReloadSubscriber(ctx context.Context) {
	if e == nil || e.eventBus == nil {
		return
	}

	unsubscribe := e.eventBus.Subscribe(
		providersReloadPattern,
		func(event events.Event) {
			requestId := ""
			if event.Payload != nil {
				if v, ok := event.Payload["requestId"].(string); ok {
					requestId = v
				}
			}
			available, err := e.ReloadAIProviders(ctx)
			// safeLogger, not e.Logger -- see its own comment: the field is
			// promoted from a *component.Component that a hand-built engine
			// does not have, so reading it directly panics.
			logger := e.safeLogger()
			if logger == nil {
				return
			}
			if err != nil {
				logger.Warn("provider reload propagation: reload failed; this node keeps its previous providers",
					"component", ComponentName,
					"requestId", requestId, "originNode", event.OriginNodeId, "error", err)
				return
			}
			logger.Info("provider reload propagation: providers re-resolved from broadcast",
				"component", ComponentName,
				"requestId", requestId, "originNode", event.OriginNodeId,
				"available", available)
		},
		events.WithSubscriberName("providers:reload:propagation"),
	)

	if unsubscribe == nil {
		return
	}

	go func() {
		<-ctx.Done()
		unsubscribe()
	}()
}

// emitProvidersReloadAudit records WHO asked for a reload.
//
// The audit line is the reason Apply is explicit rather than automatic: a
// credential rotation that took effect across a fleet with no record of who
// triggered it is exactly the event an incident review needs and cannot
// reconstruct afterwards. A nil sink means audit is not wired on this binary
// -- the slog line above still records that it happened.
func (e *MemQLEngine) emitProvidersReloadAudit(ctx context.Context, requestedBy, requestId string) {
	sink := e.authoredAuditSink()
	if sink == nil {
		return
	}
	sink.EmitAuthoredAudit(ctx, AuthoredAuditEvent{
		Action:      AuditActionProvidersReloaded,
		OwnerUserId: requestedBy,
		Detail: map[string]any{
			"requestId":   requestId,
			"requestedBy": requestedBy,
		},
		OccurredAt: time.Now().UTC(),
	})
}

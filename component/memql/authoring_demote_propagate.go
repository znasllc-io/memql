package memql

// authoring_demote_propagate.go -- LIVE cross-node propagation of durable
// demotions (memql#2163), the exact inverse of authoring_promote_propagate.go.
//
// DemoteConstructDurable (authoring_demote_durable.go) removes a durably-promoted
// plain construct from the SHARED registry of the node that handled the demote
// AND flips the persisted rows to retired (so the boot re-hydration skips it).
// But until this file, a demote on node A was removed on node A immediately yet
// STILL CALLABLE on node B until B restarted (B's shared registry kept the
// in-memory promotion). Per the hard "multi-node is the DEFAULT" rule that is a
// bug: a demote on node A MUST take effect on node B within seconds, no restart.
//
// This file closes the gap, modeled EXACTLY on the authoring-promote subscriber:
//
//   - DemoteConstructDurable emits a dedicated authoring.demote.<bundleId>
//     broadcast (publishAuthoringDemote);
//   - a single broadcast routing rule (authoring.demote.*, component/node/
//     routing.go) forwards it to EVERY node with zero side effects (only this
//     subscriber consumes it -- no automations);
//   - every node's StartAuthoringDemoteSubscriber receives the event off its
//     LOCAL bus (the node EventBridge bridges the broadcast across the mesh) and
//     UNREGISTERS that bundle's constructs from its own shared registry, reusing
//     the same per-construct DemoteAuthoredConstruct the local demote uses.
//
// Because all nodes share one Postgres, the broadcast carries only the bundle id
// + owner; each node loads the (now retired) construct rows itself to learn which
// names to unregister. Removal is idempotent + safety-gated: a node that never
// had the construct promoted just no-ops (the not-author-promoted error is
// expected + logged, not fatal), and DemoteAuthoredConstruct can never remove a
// sealed core construct.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/events"
)

// StartAuthoringDemoteSubscriber subscribes this node to the dedicated
// authoring-demote broadcast channel (memql#2163). On every
// authoring.demote.<bundleId> event -- local or bridged from a peer -- it
// unregisters that bundle's demoted plain constructs from this node's shared
// registry, so a demote handled on ANY node takes effect here within seconds (no
// restart). Scoped to ctx: cancelling ctx (engine stop) unsubscribes.
//
// Exported so the cross-node cluster harness can wire two engines onto two buses
// and prove a demote on one engine removes the construct on the other; the engine
// bootstrap calls it once at startup, alongside the promote subscriber.
func (e *MemQLEngine) StartAuthoringDemoteSubscriber(ctx context.Context) {
	e.startAuthoringDemoteSubscriberWithStore(ctx, &enginePromoteRehydrateStore{engine: e})
}

// startAuthoringDemoteSubscriberWithStore is the store-injectable core of
// StartAuthoringDemoteSubscriber, split out so the cross-node cluster harness can
// wire two engines onto two buses with fake stores and prove the broadcast path
// removes the construct on the receiving engine (no live DB). Production passes
// the live enginePromoteRehydrateStore (the demote rows are the same persisted
// promote rows, now retired).
func (e *MemQLEngine) startAuthoringDemoteSubscriberWithStore(ctx context.Context, store promoteRehydrateStore) {
	if e == nil || e.eventBus == nil {
		return
	}

	unsubscribe := e.eventBus.Subscribe(
		authoringDemotePattern,
		func(event events.Event) {
			bundleId, ok := events.BundleFromAuthoringDemoteTopic(event.Topic)
			if !ok {
				return
			}
			owner := ""
			if event.Payload != nil {
				if v, ok := event.Payload["owner"].(string); ok {
					owner = v
				}
			}
			removed, err := e.demoteBundleFromRegistryNow(ctx, store, owner, bundleId)
			if e.Component != nil && e.Logger != nil {
				if err != nil {
					e.Logger.Warn("authoring demote propagation: bundle removal failed",
						"bundleId", bundleId, "owner", owner, "originNode", event.OriginNodeId, "error", err)
					return
				}
				e.Logger.Info("authoring demote propagation: bundle removed from broadcast",
					"bundleId", bundleId, "owner", owner, "originNode", event.OriginNodeId, "removed", removed)
			}
		},
		events.WithSubscriberName("authoring:demote:propagation"),
	)

	if unsubscribe == nil {
		return
	}

	go func() {
		<-ctx.Done()
		unsubscribe()
	}()
}

// authoringDemotePattern matches every authoring-demote topic
// (authoring.demote.<bundleId>) regardless of how many segments the bundle id
// spans (bundle ids carry hyphen + short-id segments, not dots).
const authoringDemotePattern = "authoring.demote.#"

// demoteBundleFromRegistryNow unregisters ONE demoted bundle's plain constructs
// from this node's shared registry -- the live-propagation counterpart to the
// local DemoteConstructDurable. It loads the bundle's persisted member constructs
// (now retired) and removes each via DemoteAuthoredConstruct. A construct this
// node never had promoted is a harmless no-op (the not-author-promoted error is
// expected on a node that did not handle the original promote -- it is counted as
// "already absent", not a failure). Returns the count actually removed.
func (e *MemQLEngine) demoteBundleFromRegistryNow(ctx context.Context, store promoteRehydrateStore, owner, bundleId string) (int, error) {
	constructs, err := store.LoadConstructsForBundle(ctx, owner, bundleId)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, row := range constructs {
		if !isDurablePromotableKind(row.Kind) {
			continue
		}
		if derr := e.DemoteAuthoredConstruct(ctx, row.Kind, row.Name); derr != nil {
			// Not author-promoted on this node (never handled the original promote,
			// or already demoted) -- expected + idempotent, not a failure.
			if strings.Contains(derr.Error(), "not an author-promoted construct") {
				continue
			}
			return removed, derr
		}
		removed++
	}
	return removed, nil
}

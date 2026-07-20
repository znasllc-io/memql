package memql

// authoring_promote_propagate.go -- LIVE cross-node propagation of durable
// promotions (issue znasllc-io/memql-cockpit#232).
//
// PromoteConstructDurable (authoring_promote_durable.go) persists a promoted
// plain construct AND registers it into the SHARED registry of the node that
// handled the promote. RehydratePromotedConstructs re-registers every such
// promotion on BOOT, so a promotion survives a restart. But until this file,
// that boot re-hydration was the ONLY path that taught a node about a promotion
// it did not handle itself: a promote on node A was callable on node A
// immediately, but invisible on node B until B restarted.
//
// Per this repo's hard "multi-node is the DEFAULT" rule, that is a bug: a
// promote on node A MUST become callable on node B within seconds, no restart.
// This file closes the gap, modeled EXACTLY on the result-cache invalidation
// broadcast (result_cache_invalidation.go / memql#1970):
//
//   - PromoteConstructDurable emits a dedicated authoring.promote.<bundleId>
//     broadcast (publishAuthoringPromote);
//   - a single broadcast routing rule (authoring.promote.*, component/node/
//     routing.go) forwards it to EVERY node with zero side effects (only this
//     subscriber consumes it -- no automations);
//   - every node's StartAuthoringPromoteSubscriber receives the event off its
//     LOCAL bus (the node EventBridge bridges the broadcast across the mesh) and
//     re-hydrates THAT bundle's constructs from the SHARED DB into its own
//     shared registry, reusing the same per-construct recompile-and-promote
//     logic the boot re-hydration uses (recompileAndPromoteRow).
//
// Because all nodes share one Postgres, the broadcast carries only the bundle
// id + owner; each node loads the persisted construct rows itself. Re-hydration
// is idempotent + core-first (PromoteAuthoredConstruct never shadows core), so
// the origin node re-applying its own promote is a harmless no-op.

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/events"
)

// errAuthoringPromoteNoRecompile guards the store-driven bundle re-hydration:
// it requires a recompile-and-promote step (the engine wires
// recompileAndPromoteRow). Mirrors the boot-path guard.
var errAuthoringPromoteNoRecompile = errors.New("authoring: bundle re-hydration requires a recompile-and-promote step")

// StartAuthoringPromoteSubscriber subscribes this node to the dedicated
// authoring-promote broadcast channel (issue #232). On every
// authoring.promote.<bundleId> event -- local or bridged from a peer -- it
// re-hydrates that bundle's durably-promoted plain constructs from the shared
// DB into this node's shared registry, so a promote handled on ANY node becomes
// callable here within seconds (no restart).
//
// The subscription is scoped to ctx: when ctx is cancelled (engine stop) the
// unsubscribe runs and the handler stops firing. Exported so the cross-node
// cluster harness can wire two engines onto two buses and prove a promote on
// one engine re-hydrates on the other; the engine bootstrap calls it once at
// startup, alongside the boot re-hydration.
func (e *MemQLEngine) StartAuthoringPromoteSubscriber(ctx context.Context) {
	e.startAuthoringPromoteSubscriberWithStore(ctx, &enginePromoteRehydrateStore{engine: e})
}

// startAuthoringPromoteSubscriberWithStore is the store-injectable core of
// StartAuthoringPromoteSubscriber, split out so the cross-node cluster harness
// can wire two engines onto two buses with fake rehydrate stores and prove the
// broadcast path re-hydrates on the receiving engine (no live DB). Production
// passes the live enginePromoteRehydrateStore.
func (e *MemQLEngine) startAuthoringPromoteSubscriberWithStore(ctx context.Context, store promoteRehydrateStore) {
	if e == nil || e.eventBus == nil {
		return
	}

	unsubscribe := e.eventBus.Subscribe(
		authoringPromotePattern,
		func(event events.Event) {
			bundleId, ok := events.BundleFromAuthoringPromoteTopic(event.Topic)
			if !ok {
				return
			}
			owner := ""
			if event.Payload != nil {
				if v, ok := event.Payload["owner"].(string); ok {
					owner = v
				}
			}
			res, err := e.rehydratePromotedBundleNow(ctx, store, owner, bundleId)
			if e.Component != nil && e.Logger != nil {
				if err != nil {
					e.Logger.Warn("authoring promote propagation: bundle re-hydration failed",
						"bundleId", bundleId, "owner", owner, "originNode", event.OriginNodeId, "error", err)
					return
				}
				e.Logger.Info("authoring promote propagation: bundle re-hydrated from broadcast",
					"bundleId", bundleId, "owner", owner, "originNode", event.OriginNodeId,
					"seen", res.Seen, "rehydrated", res.Rehydrated, "failed", len(res.Failed))
			}
		},
		events.WithSubscriberName("authoring:promote:propagation"),
	)

	if unsubscribe == nil {
		return
	}

	go func() {
		<-ctx.Done()
		unsubscribe()
	}()
}

// authoringPromotePattern matches every authoring-promote topic
// (authoring.promote.<bundleId>) regardless of how many segments the bundle id
// spans (bundle ids carry hyphen + short-id segments, not dots).
const authoringPromotePattern = "authoring.promote.#"

// rehydratePromotedBundleNow re-hydrates ONE durably-promoted bundle's plain
// constructs into the shared registry -- the live-propagation counterpart to
// the boot-time rehydratePromotedNow (which walks ALL bundles). It loads the
// bundle's persisted member constructs under the owner's envelope and recompiles
// + promotes each via the same recompileAndPromoteRow logic the boot path uses,
// so the propagation path and the restart path share one re-registration code
// path. Idempotent + core-first.
func (e *MemQLEngine) rehydratePromotedBundleNow(ctx context.Context, store promoteRehydrateStore, owner, bundleId string) (RehydrateResult, error) {
	return rehydratePromotedBundleWithStore(ctx, store, owner, bundleId, withRehydratePromote(e.recompileAndPromoteRow))
}

// rehydratePromotedBundleWithStore is the store-driven core for a single-bundle
// re-hydration, split out so it is unit testable with a fake
// promoteRehydrateStore (no live DB). It mirrors
// rehydratePromotedConstructsWithStore but scopes to one bundle.
func rehydratePromotedBundleWithStore(ctx context.Context, store promoteRehydrateStore, owner, bundleId string, opts ...rehydrateOption) (RehydrateResult, error) {
	cfg := rehydrateConfig{promote: nil}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.promote == nil {
		return RehydrateResult{}, errAuthoringPromoteNoRecompile
	}
	constructs, err := store.LoadConstructsForBundle(ctx, owner, bundleId)
	if err != nil {
		return RehydrateResult{}, err
	}
	var result RehydrateResult
	for _, row := range constructs {
		if !isDurablePromotableKind(row.Kind) {
			continue
		}
		if isRetiredConstructStatus(row.Status) {
			// A demoted (retired) construct must NOT re-register from a
			// stale promote broadcast -- skip it.
			continue
		}
		result.Seen++
		if perr := cfg.promote(ctx, row); perr != nil {
			result.Failed = append(result.Failed, row.Kind+":"+row.Name)
			continue
		}
		result.Rehydrated++
	}
	return result, nil
}

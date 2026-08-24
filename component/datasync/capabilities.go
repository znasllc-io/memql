package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// capabilities.go -- the DSL-callable surface of the sync runtime.
//
// Four capabilities, and the split between them is the split between the
// two ways work arrives:
//
//	dispatchInbound  EVENT-DRIVEN. An automation on
//	                 v1:platform:inboundRequest.created offers every
//	                 staged delivery to the connector its source names.
//	reconcileDomains SCHEDULED. A cron automation sweeps whichever
//	                 domains their own DomainSpec says are due.
//	startBackfill    OPERATOR-DRIVEN. Nothing schedules a backfill; it is
//	                 something a person decides to do to a domain.
//	setSyncPaused    OPERATOR-DRIVEN, and the switch that stops the
//	                 other three for one domain.
//
// The first two are no-ops on a node whose build has no connector bound,
// which is most of them: the automations load everywhere because every
// binary loads every concept, and a node that cannot serve a connector
// simply has nothing to offer it.

// Integration is the sync runtime's IntegrationProvider.
type Integration struct {
	store      *Store
	applier    *Applier
	dispatcher *Dispatcher
	runner     *Runner
	logger     *slog.Logger
}

// NewIntegration wires the runtime over an engine.
func NewIntegration(engine Engine, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	store := NewStore(engine)
	applier := NewApplier(store, NewEngineMirrorWriter(engine))
	return &Integration{
		store:      store,
		applier:    applier,
		dispatcher: NewDispatcher(store, applier),
		runner:     NewRunner(store, applier, logger),
		logger:     logger.With("component", "datasync"),
	}
}

func (i *Integration) IntegrationName() string { return "datasync" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "dispatchInbound",
			Description: "Route one staged inbound request to the connector its source names, apply the returned mirror writes behind the version guard, and stamp the request. A no-op for a source no connector serves.",
			Handler:     i.handleDispatchInbound,
			ArgsSchema: map[string]string{
				"inboundRequestId": "string - staged v1:platform:inboundRequest row id",
				"source":           "string - the /inbound/{source} name the delivery arrived under",
				"body":             "string - the verified raw request body",
			},
		},
		{
			Name:        "reconcileDomains",
			Description: "Sweep every mirrored domain whose DomainSpec interval says it is due, healing drift through the same version-guarded apply path inbound uses. Skips paused domains and connectors that do not implement Reconcile.",
			Handler:     i.handleReconcileDomains,
		},
		{
			Name:        "startBackfill",
			Description: "Drive one domain's backfill page by page from its stored cursor, persisting progress after every page so a restart resumes. Operator-driven; nothing schedules it.",
			Handler:     i.handleStartBackfill,
			ArgsSchema: map[string]string{
				"connector": "string - connector name",
				"conceptId": "string - canonical id of the concept to backfill",
			},
		},
		{
			Name:        "retryOutboxEntry",
			Description: "Return one dead-lettered entry to the queue. Attempts reset to zero, because the operator has presumably fixed what was wrong and carrying the old count forward would dead-letter it again on the first hiccup.",
			Handler:     i.handleRetryOutboxEntry,
			ArgsSchema:  map[string]string{"entryId": "string - the dead entry's row id"},
		},
		{
			Name:        "discardOutboxEntry",
			Description: "Discard one dead-lettered entry: the operator has decided this change will never be delivered. The row survives as audit history carrying the reason.",
			Handler:     i.handleDiscardOutboxEntry,
			ArgsSchema: map[string]string{
				"entryId": "string - the dead entry's row id",
				"reason":  "string (optional) - why it will never be delivered",
			},
		},
		{
			Name:        "setSyncPaused",
			Description: "Pause or resume one domain: stops its backfill, its reconciliation and its drain without unregistering the connector or changing any declaration.",
			Handler:     i.handleSetPaused,
			ArgsSchema: map[string]string{
				"connector": "string - connector name",
				"conceptId": "string - canonical id of the concept",
				"paused":    "boolean - true to pause",
			},
		},
	}
}

func (i *Integration) handleDispatchInbound(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	source := strings.TrimSpace(argString(args, "source"))
	if source == "" {
		return resultNode("skipped", "no source on the staged row")
	}
	req := memqlsync.InboundRequest{
		RequestId:  strings.TrimSpace(argString(args, "inboundRequestId")),
		Source:     source,
		Topic:      strings.TrimSpace(argString(args, "topic")),
		Body:       []byte(argString(args, "body")),
		ReceivedAt: time.Now().UTC(),
	}
	res, err := i.dispatcher.Dispatch(ctx, req)
	if err != nil {
		// Returned rather than swallowed: the automation step records it,
		// and the request row has already been stamped `failed` with the
		// same message, so an operator sees it in both places.
		return nil, err
	}
	if !res.Handled {
		return resultNode("skipped", "no connector serves source "+source)
	}
	return resultNode("processed", fmt.Sprintf("applied=%d stale=%d", res.Applied, res.Stale))
}

func (i *Integration) handleReconcileDomains(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	var swept, drifted int
	for _, name := range memqlsync.BoundNames() {
		connector, ok := memqlsync.Lookup(name)
		if !ok || connector == nil {
			continue
		}
		for _, spec := range connector.Domains() {
			if spec.Direction == memqlsync.DirectionOutbound {
				// Reconciliation compares an ORIGIN against a MIRROR.
				// An outbound domain's mirror is somebody else's and the
				// outbox is what keeps it current; sweeping it here would
				// be MemQL auditing a system it does not read.
				continue
			}
			if !i.runner.ReconcileDue(ctx, name, spec) {
				continue
			}
			report, err := i.runner.Reconcile(ctx, name, spec.Concept)
			if err != nil {
				i.logger.Warn("datasync: a reconciliation sweep failed; the others still run",
					"connector", name, "concept", spec.Concept, "error", err)
				continue
			}
			if report.Skipped {
				continue
			}
			swept++
			drifted += report.Drifted
		}
	}
	return resultNode("swept", fmt.Sprintf("domains=%d drifted=%d", swept, drifted))
}

func (i *Integration) handleStartBackfill(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	connector := strings.TrimSpace(argString(args, "connector"))
	conceptID := strings.TrimSpace(argString(args, "conceptId"))
	if connector == "" || conceptID == "" {
		return nil, fmt.Errorf("datasync: startBackfill needs both connector and conceptId")
	}
	res, err := i.runner.StartBackfill(ctx, connector, conceptID)
	if err != nil {
		return nil, err
	}
	return resultNode("backfill", fmt.Sprintf("pages=%d applied=%d stale=%d done=%t", res.Pages, res.Applied, res.Stale, res.Done))
}

// handleRetryOutboxEntry and handleDiscardOutboxEntry are the
// operator's two answers to a dead letter, and they exist as
// CAPABILITIES rather than as directly-callable mutations because the
// mutations are @serverOnly: an engine delivery queue is not a surface a
// client drives row by row. The capability is the door, and the owner /
// admin gate sits at the console that opens it.
//
// Both refuse an entry that is not DEAD. A `pending` entry is already
// the worker's, and a retry that raced the worker would deliver twice --
// which the receiver's idempotency key would probably absorb, and
// "probably" is not the standard for a button an operator presses.
func (i *Integration) handleRetryOutboxEntry(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	entry, err := i.requireDeadEntry(ctx, argString(args, "entryId"))
	if err != nil {
		return nil, err
	}
	opCtx := OperatorContext(ctx)
	if _, err := i.store.exec(opCtx, call("mutation", "retryOutboxEntry", arg{"entryId", entry.ID})); err != nil {
		return nil, err
	}
	i.logger.Info("datasync: a dead-lettered entry was returned to the queue by an operator",
		"entry", entry.ID, "connector", entry.Target, "concept", entry.ConceptID, "row", entry.RowRef)
	return resultNode("retried", entry.ID)
}

func (i *Integration) handleDiscardOutboxEntry(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	entry, err := i.requireDeadEntry(ctx, argString(args, "entryId"))
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(argString(args, "reason"))
	opCtx := OperatorContext(ctx)
	if _, err := i.store.exec(opCtx, call("mutation", "discardOutboxEntry",
		arg{"entryId", entry.ID}, arg{"reason", reason})); err != nil {
		return nil, err
	}
	i.logger.Warn("datasync: a change will NEVER be delivered -- discarded by an operator",
		"entry", entry.ID, "connector", entry.Target, "concept", entry.ConceptID, "row", entry.RowRef, "reason", reason)
	return resultNode("discarded", entry.ID)
}

// requireDeadEntry resolves an id to a DEAD entry, or refuses.
//
// Resolved from the dead-letter queue rather than by id lookup, so an
// operator can only ever act on an entry that is actually in it -- which
// is the same set the console shows them.
func (i *Integration) requireDeadEntry(ctx context.Context, entryID string) (OutboxEntry, error) {
	entryID = strings.TrimSpace(entryID)
	if entryID == "" {
		return OutboxEntry{}, fmt.Errorf("datasync: an entryId is required")
	}
	opCtx := OperatorContext(ctx)
	for _, name := range memqlsync.BoundNames() {
		entries, err := i.store.DeadLetters(opCtx, name)
		if err != nil {
			return OutboxEntry{}, err
		}
		for _, e := range entries {
			if e.ID == entryID || strings.HasSuffix(e.ID, ":"+entryID) {
				return e, nil
			}
		}
	}
	return OutboxEntry{}, fmt.Errorf(
		"datasync: %q is not a dead-lettered entry. Only a dead entry is the operator's to act on -- "+
			"a pending or failed one is still the drain worker's, and acting on it would race a delivery in flight",
		entryID)
}

func (i *Integration) handleSetPaused(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	connector := strings.TrimSpace(argString(args, "connector"))
	conceptID := strings.TrimSpace(argString(args, "conceptId"))
	if connector == "" || conceptID == "" {
		return nil, fmt.Errorf("datasync: setSyncPaused needs both connector and conceptId")
	}
	paused := argBool(args, "paused")
	if err := i.runner.SetPaused(ctx, connector, conceptID, paused); err != nil {
		return nil, err
	}
	return resultNode("paused", fmt.Sprintf("%s/%s paused=%t", connector, conceptID, paused))
}

// resultNode renders a capability's answer as the single ephemeral node
// the builtin surface expects. Never persisted -- the concept id is a
// synthetic one, the same shape the shopify capabilities use for their
// skip results.
func resultNode(outcome, detail string) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(map[string]any{"outcome": outcome, "detail": detail})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:      "datasync:" + outcome,
		Concept: "integration:datasync:result",
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}, nil
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	switch v := args[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func argBool(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

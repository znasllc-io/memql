package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// capabilities.go -- the DSL-callable surface of the sync runtime.
//
// The split between the capabilities is the split between the ways work
// arrives (the outbox's retry and discard are an operator's answers to a dead
// letter, below):
//
//	dispatchInbound  EVENT-DRIVEN. An automation on
//	                 v1:platform:inboundRequest.created offers every
//	                 staged delivery to the connector its source names.
//	reconcileDomains SCHEDULED. A cron automation sweeps whichever
//	                 domains their own DomainSpec says are due.
//	startBackfill    OPERATOR-DRIVEN. A person running one domain's
//	                 backfill now.
//	queueBackfill    ENGINE-DRIVEN. A connector asking for a backfill --
//	                 at connect, at boot -- without running it.
//	continueBackfills SCHEDULED. A cron automation carries every queued
//	                 or running backfill on by one page budget per tick.
//	setSyncPaused    OPERATOR-DRIVEN, and the switch that stops the
//	                 others for one domain.
//
// The scheduled and event-driven ones are no-ops on a node whose build has no
// connector bound,
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
	// bound and lookup default to the connector registry; a test sets
	// them to sweep one connector without the process-global registry.
	bound  func() []string
	lookup func(name string) (memqlsync.Connector, bool)
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
		dispatcher: NewDispatcher(store, applier).WithLogger(logger),
		runner:     NewRunner(store, applier, logger),
		logger:     logger.With("component", "datasync"),
	}
}

func (i *Integration) IntegrationName() string { return "datasync" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "dispatchInbound",
			Description: "Route one staged inbound request to the connector its source names, apply the returned mirror writes behind the version guard, and stamp the request. Refuses a call without internal origin; reads the delivery from the staged row, and applies a claimed source's row only when the connector's secret verified it. A no-op for a source no connector serves.",
			Handler:     i.handleDispatchInbound,
			ArgsSchema: map[string]string{
				"inboundRequestId": "string (required) - staged v1:platform:inboundRequest row id; its source, body, metadata, verifying tier and signature verdict are read from the row",
			},
		},
		{
			Name:        "reconcileDomains",
			Description: "Sweep every mirrored domain whose DomainSpec interval says it is due, healing drift through the same version-guarded apply path inbound uses. A partitioned connector is swept one partition at a time, each from its own watermark. Skips paused domains and connectors that do not implement Reconcile.",
			Handler:     i.handleReconcileDomains,
		},
		{
			Name:        "startBackfill",
			Description: "Drive one domain's backfill page by page from its stored cursor, persisting progress after every page so a restart resumes. Operator-driven; nothing schedules it.",
			Handler:     i.handleStartBackfill,
			ArgsSchema: map[string]string{
				"connector":    "string - connector name",
				"conceptId":    "string - canonical id of the concept to backfill",
				"partitionKey": "string (optional) - one partition of a partitioned connector; absent backfills every partition it names",
			},
		},
		{
			Name:        "queueBackfill",
			Description: "Queue one domain's backfill: write `queued` and an empty cursor, and run nothing; the continuing pass starts it. A running backfill is left as it is. Refuses a call without internal origin.",
			Handler:     i.handleQueueBackfill,
			ArgsSchema: map[string]string{
				"connector":    "string - connector name",
				"conceptId":    "string - canonical id of the concept to backfill",
				"partitionKey": "string (optional) - one partition of a partitioned connector; absent queues every partition it names",
			},
		},
		{
			Name:        "continueBackfills",
			Description: "Continue every queued or running backfill: for each partition of each bound connector, advance the one domain that is running, else start the first queued, by one page budget. Leaves paused domains alone. Refuses a call without internal origin.",
			Handler:     i.handleContinueBackfills,
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
				"connector":    "string - connector name",
				"conceptId":    "string - canonical id of the concept",
				"partitionKey": "string (optional) - the one partition of a partitioned connector to pause",
				"paused":       "boolean - true to pause",
			},
		},
	}
}

// requireInternalOrigin is the gate of the two automation-driven builtins,
// checked BEFORE any argument is read. A builtin cannot be @serverOnly, so
// per the root CLAUDE.md its gate lives in the handler: dispatchInbound hands
// a staged delivery to a connector, which for Shopify reaches a privacy purge.
// It takes no delivery from its caller -- only the row id, the row read by the
// dispatcher itself (handleDispatchInbound) -- but a client re-dispatching a
// real row by id would still re-run a connector's Apply and re-stamp the row,
// which is the operator's record of what happened to it. The automation
// executor stamps internal origin on a trusted automation's step context
// (originForSource), which is how dispatchInboundToConnector and the
// reconcile cron reach these; nothing else may.
func requireInternalOrigin(ctx context.Context, name string) error {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return fmt.Errorf("datasync: %s is driven by the engine's own automations and refuses a client-origin call", name)
	}
	return nil
}

// handleDispatchInbound is HANDED A ROW ID AND NOTHING ELSE (epic memql#5477).
// It used to take the source, the body, the headers, the receipt time and the
// verifying tier as arguments, so whatever could call it chose the delivery a
// connector saw -- and the tier check below it judged a value its caller
// supplied. Now every one of them is read from the STAGED row, which only the
// receiver (or the mailbox reader) can write, under internal origin.
func (i *Integration) handleDispatchInbound(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOrigin(ctx, "dispatchInbound"); err != nil {
		return nil, err
	}
	requestID := strings.TrimSpace(argString(args, "inboundRequestId"))
	if requestID == "" {
		return nil, fmt.Errorf("datasync: dispatchInbound needs the staged row's inboundRequestId")
	}
	staged, found, err := i.dispatcher.Staged(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("datasync: no inbound request is staged as %q", requestID)
	}
	source := strings.TrimSpace(staged.Source)
	if source == "" {
		return resultNode("skipped", "no source on the staged row")
	}
	// The row is judged INSIDE DispatchStaged, after the connector lookup, so
	// a row of a source no connector serves stays the no-op Dispatch's
	// contract promises whatever its tier, its verdict or its metadata say; a
	// served row that fails any of them is stamped `failed` there.
	res, err := i.dispatcher.DispatchStaged(ctx, staged)
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

// handleReconcileDomains is the scheduled sweep. A connector that
// implements memqlsync.Partitioned is swept one partition at a time, each
// through ReconcilePartition from its own watermark, and one partition's
// failure is logged while the next still runs. Any other connector is swept
// with partition "", as it always was.
func (i *Integration) handleReconcileDomains(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOrigin(ctx, "reconcileDomains"); err != nil {
		return nil, err
	}
	var swept, drifted int
	sweep := func(name string, connector memqlsync.Connector, partition string) {
		for _, spec := range connector.Domains() {
			if spec.Direction == memqlsync.DirectionOutbound {
				// Reconciliation compares an ORIGIN against a MIRROR.
				// An outbound domain's mirror is somebody else's and the
				// outbox is what keeps it current; sweeping it here would
				// be MemQL auditing a system it does not read.
				continue
			}
			if !i.runner.ReconcileDue(ctx, name, spec, partition) {
				continue
			}
			report, err := i.runner.Reconcile(ctx, name, spec.Concept, partition)
			if err != nil {
				i.logger.Warn("datasync: a reconciliation sweep failed; the others still run",
					"connector", name, "concept", spec.Concept, "partition", partition, "error", err)
				continue
			}
			if report.Skipped {
				continue
			}
			swept++
			drifted += report.Drifted
		}
	}
	for _, name := range i.boundNames() {
		connector, ok := i.lookupConnector(name)
		if !ok || connector == nil {
			continue
		}
		p, partitioned := connector.(memqlsync.Partitioned)
		if !partitioned {
			sweep(name, connector, "")
			continue
		}
		parts, err := p.Partitions(auth.ContextWithConnectorActor(ctx, name))
		if err != nil {
			i.logger.Warn("datasync: could not list a connector's partitions; its sweep waits for the next tick",
				"connector", name, "error", err)
			continue
		}
		for _, part := range cleanPartitions(parts) {
			sweep(name, connector, part)
		}
	}
	return resultNode("swept", fmt.Sprintf("domains=%d drifted=%d", swept, drifted))
}

// boundNames and lookupConnector are the registry, unless a test set the
// seams -- the drain worker's precedent.
func (i *Integration) boundNames() []string {
	if i.bound != nil {
		return i.bound()
	}
	return memqlsync.BoundNames()
}

func (i *Integration) lookupConnector(name string) (memqlsync.Connector, bool) {
	if i.lookup != nil {
		return i.lookup(name)
	}
	return memqlsync.Lookup(name)
}

func (i *Integration) handleStartBackfill(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	connector := strings.TrimSpace(argString(args, "connector"))
	conceptID := strings.TrimSpace(argString(args, "conceptId"))
	partition := strings.TrimSpace(argString(args, "partitionKey"))
	if connector == "" || conceptID == "" {
		return nil, fmt.Errorf("datasync: startBackfill needs both connector and conceptId")
	}
	res, err := i.runner.StartBackfill(ctx, connector, conceptID, partition)
	if err != nil {
		return nil, err
	}
	return resultNode("backfill", fmt.Sprintf("pages=%d applied=%d stale=%d done=%t", res.Pages, res.Applied, res.Stale, res.Done))
}

// handleQueueBackfill is how something other than an operator asks for a
// backfill: a connector at connect or at boot, through the builtin under its
// own operator identity. It only records the wish; handleContinueBackfills,
// on its schedule, does the work. Internal origin only, so a client cannot
// queue a domain's backfill over and over to have the origin re-read.
func (i *Integration) handleQueueBackfill(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOrigin(ctx, "queueBackfill"); err != nil {
		return nil, err
	}
	connector := strings.TrimSpace(argString(args, "connector"))
	conceptID := strings.TrimSpace(argString(args, "conceptId"))
	partition := strings.TrimSpace(argString(args, "partitionKey"))
	if connector == "" || conceptID == "" {
		return nil, fmt.Errorf("datasync: queueBackfill needs both connector and conceptId")
	}
	if err := i.runner.QueueBackfill(ctx, connector, conceptID, partition); err != nil {
		return nil, err
	}
	return resultNode("queued", fmt.Sprintf("%s/%s%s", connector, conceptID, partitionSuffix(partition)))
}

// handleContinueBackfills is the scheduled continuing pass. One partition's
// failure is logged and recorded on its row while the others still run, so
// the pass answers what it did rather than the first thing that went wrong.
func (i *Integration) handleContinueBackfills(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOrigin(ctx, "continueBackfills"); err != nil {
		return nil, err
	}
	res, err := i.runner.ContinueBackfills(ctx)
	if err != nil {
		i.logger.Warn("datasync: a continuing pass finished with failures; each is on its domain's row", "error", err)
	}
	return resultNode("continued", fmt.Sprintf("started=%d advanced=%d completed=%d", res.Started, res.Advanced, res.Completed))
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
	partition := strings.TrimSpace(argString(args, "partitionKey"))
	paused := argBool(args, "paused")
	if err := i.runner.SetPaused(ctx, connector, conceptID, partition, paused); err != nil {
		return nil, err
	}
	if partition != "" {
		return resultNode("paused", fmt.Sprintf("%s/%s/%s paused=%t", connector, conceptID, partition, paused))
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

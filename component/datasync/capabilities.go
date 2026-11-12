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
	"github.com/znasllc-io/memql/core/num"
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
//	                 or running backfill on, in rounds, for four minutes
//	                 of each tick.
//	setSyncPaused    OPERATOR-DRIVEN, and the switch that stops the
//	                 others for one domain.
//	retryInbound     SCHEDULED. A cron automation tries again the failed
//	                 deliveries whose next try has come.
//	requeueInbound   OPERATOR-DRIVEN. A cluster owner putting one source's
//	                 given-up deliveries back in line.
//	returnHeldInbound ENGINE-DRIVEN. A connector putting back in line the
//	                 deliveries held while one of its tenants was paused: a
//	                 page when the tenant is resumed, and on every pass of
//	                 the connector's watch for every tenant that ingests.
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
			Description: "Drive one domain's backfill page by page from its stored cursor, persisting progress after every page so a restart resumes; with restart, start it again from nothing first. Operator-driven; nothing schedules it. For a cluster owner or the engine's own call (internal origin); anybody else is refused before anything is read.",
			Handler:     i.handleStartBackfill,
			ArgsSchema: map[string]string{
				"connector":    "string - connector name",
				"conceptId":    "string - canonical id of the concept to backfill",
				"partitionKey": "string (optional) - one partition of a partitioned connector; absent backfills every partition it names",
				"restart":      "boolean (optional) - reset the backfill to queued with an empty cursor and run it from nothing; refused while it is running, and for a partitioned connector without a partition",
			},
		},
		{
			Name:        "queueBackfill",
			Description: "Queue one domain's backfill: write `queued` and an empty cursor, and run nothing; the continuing pass starts it. A running backfill is left as it is unless restart is set. Refuses a call without internal origin.",
			Handler:     i.handleQueueBackfill,
			ArgsSchema: map[string]string{
				"connector":    "string - connector name",
				"conceptId":    "string - canonical id of the concept to backfill",
				"partitionKey": "string (optional) - one partition of a partitioned connector; absent queues every partition it names",
				"restart":      "boolean (optional) - also reset a running backfill to queued with an empty cursor; for an origin that no longer holds what the cursor counted",
			},
		},
		{
			Name:        "continueBackfills",
			Description: "Continue every queued or running backfill, in rounds, for four minutes: in each round every partition of each bound connector with work left advances the one domain that is running, else starts the first queued, by one page budget. Leaves paused domains alone. Refuses a call without internal origin.",
			Handler:     i.handleContinueBackfills,
		},
		{
			Name:        "retryInbound",
			Description: "Try again the failed inbound deliveries whose next try has come: one page, each row claimed behind its row version before it is dispatched as dispatchInbound dispatches a staged row. Four tries at most, 1, 5 and 30 minutes apart. Refuses a call without internal origin.",
			Handler:     i.handleRetryInbound,
		},
		{
			Name:        "requeueInbound",
			Description: "Put one source's given-up inbound deliveries -- failed, their four tries spent -- back in line: no tries counted, due now. For a cluster owner or the engine's own call (internal origin); anybody else is refused before anything is read.",
			Handler:     i.handleRequeueInbound,
			ArgsSchema:  map[string]string{"source": "string (required) - the inbound source whose given-up deliveries go back in line"},
		},
		{
			Name:        "returnHeldInbound",
			Description: "Put one source's held inbound deliveries -- the ones a connector would not apply while its tenant's recording was paused -- back in line: no tries counted, due now. A connector calls it for a tenant that ingests: one page when a person resumes the tenant, and on every pass of its watch; a delivery whose tenant is still paused is held again. Moves at most `limit` deliveries, five thousand when no limit is named, and answers how many it moved and whether it stopped at that bound. With nothing held it is one read and no write. For a cluster owner or the engine's own call (internal origin); anybody else is refused before anything is read.",
			Handler:     i.handleReturnHeldInbound,
			ArgsSchema: map[string]string{
				"source": "string (required) - the inbound source whose held deliveries go back in line",
				"limit":  "int (optional) - the most deliveries this call moves, from 1 to 5000; 5000 when left out",
			},
		},
		{
			Name:        "retryOutboxEntry",
			Description: "Return one dead-lettered entry to the queue. Attempts reset to zero, because the operator has presumably fixed what was wrong and carrying the old count forward would dead-letter it again on the first hiccup. For a cluster owner or the engine's own call (internal origin); anybody else is refused before anything is read.",
			Handler:     i.handleRetryOutboxEntry,
			ArgsSchema:  map[string]string{"entryId": "string - the dead entry's row id"},
		},
		{
			Name:        "discardOutboxEntry",
			Description: "Discard one dead-lettered entry: the operator has decided this change will never be delivered. The row survives as audit history carrying the reason. For a cluster owner or the engine's own call (internal origin); anybody else is refused before anything is read.",
			Handler:     i.handleDiscardOutboxEntry,
			ArgsSchema: map[string]string{
				"entryId": "string - the dead entry's row id",
				"reason":  "string (optional) - why it will never be delivered",
			},
		},
		{
			Name:        "setSyncPaused",
			Description: "Pause or resume one domain: stops its backfill, its reconciliation and its drain without unregistering the connector or changing any declaration. For a cluster owner or the engine's own call (internal origin); anybody else is refused before anything is read.",
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

// requireOperator is the gate of the operator's sync acts -- the five
// builtins behind Cluster > Data's buttons (startBackfill, setSyncPaused,
// retryOutboxEntry, discardOutboxEntry, requeueInbound) -- and of
// returnHeldInbound, which a connector calls as the operator -- checked FIRST,
// before any argument is read into a read or a write. None carries a
// @requiresCapability, and a builtin without one has no rank floor: every
// signed-in caller of any role reaches its handler, and the page's part only
// hides the buttons. So the handler asks: a call with internal origin (a
// connector acting as the operator, an automation), or a cluster owner -- the
// tier the sync rows are read at -- and nobody else.
//
// Internal origin is read from the context the engine hands the handler,
// which a client cannot set; no argument is consulted for authority.
func requireOperator(ctx context.Context, name string) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if access, ok := auth.AccessFromContext(ctx); ok && access != nil && access.IsClusterOwner() {
		return nil
	}
	return fmt.Errorf("datasync: %s is for a cluster owner", name)
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
	// ONLY A DELIVERY NOBODY HAS WORKED. The automation fires on every write
	// of the row, not only its first -- every write publishes node.created --
	// so it also fires for the dispatcher's own stamps, the retry pass's
	// claims and a requeue. The stamps' echoes share the first fire's
	// correlation and are deduplicated; a retry pass's writes carry the pass's
	// own and are not. A worked row is therefore left alone here: a failed one
	// is the retry pass's, and dispatching it from here as well would try it
	// from two places at once.
	if status := strings.TrimSpace(staged.Status); status != "" && status != "received" {
		return resultNode("skipped", fmt.Sprintf("the delivery is already %s; a failed one is tried again by the retry pass", status))
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
	if res.Held {
		return resultNode("held", "recording is paused for the delivery's store; it goes back in line when recording resumes")
	}
	return resultNode("processed", fmt.Sprintf("applied=%d stale=%d", res.Applied, res.Stale))
}

// handleRetryInbound is the scheduled retry pass.
func (i *Integration) handleRetryInbound(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOrigin(ctx, "retryInbound"); err != nil {
		return nil, err
	}
	res, err := i.dispatcher.RetryDue(ctx)
	if err != nil {
		return nil, err
	}
	return resultNode("retried", fmt.Sprintf("tried=%d recovered=%d held=%d", res.Tried, res.Recovered, res.Held))
}

// handleRequeueInbound is an operator putting given-up deliveries back in
// line. The Store page's Try again reaches it through the Shopify connector,
// which asks its own question (the store's owner or a cluster owner) and calls
// it as the operator, with internal origin.
func (i *Integration) handleRequeueInbound(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOperator(ctx, "requeueInbound"); err != nil {
		return nil, err
	}
	source := strings.TrimSpace(argString(args, "source"))
	n, err := i.dispatcher.Requeue(ctx, source)
	if err != nil {
		return nil, err
	}
	i.logger.Info("datasync: given-up inbound deliveries were put back in line", "source", source, "requeued", n)
	return resultNode("requeued", fmt.Sprintf("source=%s requeued=%d", source, n))
}

// handleReturnHeldInbound is a connector returning the deliveries held while
// one of its tenants was paused: the Shopify connector, as the operator and
// with internal origin -- one page when a person resumes a store, and its
// watch on every pass for every store that ingests. A cluster owner may run it
// too; nobody else reaches the read.
//
// `limit` is the most deliveries this call moves: left out, the runtime's own
// bound; named, a whole number from one to that bound, refused otherwise
// before anything is read. The reply says how many went back in line and
// whether the call stopped at its bound, with deliveries possibly still held.
func (i *Integration) handleReturnHeldInbound(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOperator(ctx, "returnHeldInbound"); err != nil {
		return nil, err
	}
	limit, err := returnLimit(args)
	if err != nil {
		return nil, err
	}
	source := strings.TrimSpace(argString(args, "source"))
	n, more, err := i.dispatcher.ReturnHeld(ctx, source, limit)
	if n > 0 {
		i.logger.Info("datasync: held inbound deliveries were put back in line", "source", source, "returned", n, "more", more)
	}
	if err != nil {
		return nil, err
	}
	return resultNode("returned", fmt.Sprintf("source=%s returned=%d more=%t", source, n, more))
}

// returnLimit reads returnHeldInbound's optional bound: 0 for one left out,
// else a whole number from 1 to maxBackInLine. Anything else is refused.
func returnLimit(args map[string]any) (int, error) {
	raw, named := args["limit"]
	if !named || raw == nil {
		return 0, nil
	}
	refused := fmt.Errorf("datasync: returnHeldInbound's limit is a whole number from 1 to %d, or left out", maxBackInLine)
	var n int64
	switch v := raw.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		whole, ok := num.WholeInt64(v)
		if !ok {
			return 0, refused
		}
		n = whole
	case json.Number:
		whole, err := v.Int64()
		if err != nil {
			return 0, refused
		}
		n = whole
	default:
		return 0, refused
	}
	if n < 1 || n > maxBackInLine {
		return 0, refused
	}
	return int(n), nil
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
	if err := requireOperator(ctx, "startBackfill"); err != nil {
		return nil, err
	}
	connector := strings.TrimSpace(argString(args, "connector"))
	conceptID := strings.TrimSpace(argString(args, "conceptId"))
	partition := strings.TrimSpace(argString(args, "partitionKey"))
	if connector == "" || conceptID == "" {
		return nil, fmt.Errorf("datasync: startBackfill needs both connector and conceptId")
	}
	start := i.runner.StartBackfill
	if argBool(args, "restart") {
		start = i.runner.StartBackfillFromTheStart
	}
	res, err := start(ctx, connector, conceptID, partition)
	if err != nil {
		return nil, err
	}
	if res.Abandoned {
		return resultNode("abandoned", "the backfill was not run to its end: its row was queued again, or another backfill of this domain holds it; nothing more was written")
	}
	if res.Waiting {
		return resultNode("waiting", fmt.Sprintf("the backfill is waiting behind another export of this origin; it is queued and the continuing pass starts it (pages=%d applied=%d)", res.Pages, res.Applied))
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
	queue := i.runner.QueueBackfill
	if argBool(args, "restart") {
		queue = i.runner.RestartBackfill
	}
	if err := queue(ctx, connector, conceptID, partition); err != nil {
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
// client drives row by row. The capability is the door, and requireOperator
// is its lock: the console that opens it only hides the buttons.
//
// Both refuse an entry that is not DEAD. A `pending` entry is already
// the worker's, and a retry that raced the worker would deliver twice --
// which the receiver's idempotency key would probably absorb, and
// "probably" is not the standard for a button an operator presses.
func (i *Integration) handleRetryOutboxEntry(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOperator(ctx, "retryOutboxEntry"); err != nil {
		return nil, err
	}
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
	if err := requireOperator(ctx, "discardOutboxEntry"); err != nil {
		return nil, err
	}
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
	if err := requireOperator(ctx, "setSyncPaused"); err != nil {
		return nil, err
	}
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

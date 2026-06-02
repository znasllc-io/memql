// refresh_cron.go
//
// The daily knowledge-refresh path (issue #644 -- activates the stub).
//
// Per Q9 of the planner-redesign brainstorm, a knowledge domain gets
// re-trained on a cadence backstop: if no stale-signal event fires
// within refreshCadenceDays, a scheduled run spawns a trainSpecialist
// Plan with mode='refresh' for the domain. Before #644 this was a no-op:
//
//   - The DSL automation (dsl/knowledge/automations.memql) fired the
//     logicRefreshDueKnowledgeDomains logic, which returned a candidate
//     COUNT and did nothing else ("the actual spawn loop lives in the
//     Go cron-handler ... ships when the Phase 4b wiring lands").
//
// This file IS that Go cron-handler. It runs as a poller inside the
// planner integration (the same node that owns the trainSpecialist
// dispatcher), so a spawned Plan is picked up by the dispatcher in the
// same process. The schema-side cadence + runtime-side spawn split is
// deliberate (per Q9): the DSL filter is broad (active + non-null
// refreshCadenceDays), and the precise elapsed-time math runs in Go,
// where arithmetic on datetime fields is cheap (the MemQL parser has
// neither datetime arithmetic nor now()-as-comparison-RHS, which is why
// queryDueRefreshDomains can't do the elapsed check itself).
//
// The poll cadence is hourly, not daily: the original DSL cron was a
// nightly 02:30 schedule, but an in-process poller can't depend on the
// engine's automation scheduler reaching this node-type binary, so we
// run our own ticker. Hourly polling with a per-domain "spawned
// recently" guard means a due domain gets exactly one refresh Plan and
// isn't re-spawned every hour while the prior run is in flight or the
// lastSeededAt stamp hasn't updated yet.

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	// refreshPollInterval is how often the cron wakes to check for due
	// domains. Hourly is frequent enough to make the "daily" cadence
	// backstop feel timely without hammering the query.
	refreshPollInterval = time.Hour

	// refreshRespawnGuard is how long we suppress re-spawning a refresh
	// Plan for the same domain after spawning one. Covers the window
	// between spawning the Plan and the Trainer stamping a fresh
	// lastSeededAt (which is what makes queryDueRefreshDomains stop
	// returning the domain). Generous so a slow Trainer run doesn't get
	// a duplicate refresh queued behind it.
	refreshRespawnGuard = 6 * time.Hour

	// defaultRefreshCadenceDays mirrors the knowledgeDomain default; used
	// when a row's refreshCadenceDays is missing / unparseable.
	defaultRefreshCadenceDays = 90

	// staleSignalRefreshThreshold is the staleSignalCount at (or above)
	// which the event-driven path spawns an immediate refresh rather
	// than waiting for the cadence backstop. Per Q9 the default is 3:
	// the Planner Agent bumps the count via markKnowledgeDomainStale
	// when it sees degraded responses citing the domain, and once enough
	// signals accumulate we refresh proactively.
	staleSignalRefreshThreshold = 3

	// refreshSystemSpaceId / refreshSystemRequester are the synthetic
	// space + requester stamped on a system-spawned refresh Plan. The
	// Plan concept requires spaceId + requestedBy; a cron refresh has no
	// real user/space, so these sentinels keep the row well-formed
	// without pretending a person asked for it (triggerSource='system'
	// is the authoritative provenance marker).
	refreshSystemSpaceId    = "system:knowledge-refresh"
	refreshSystemRequester  = "system"
)

// RefreshCron polls queryDueRefreshDomains and spawns
// trainSpecialist(mode='refresh') Plans for domains past their cadence.
type RefreshCron struct {
	engine Engine
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Debug(msg string, args ...any)
	}

	cancel context.CancelFunc

	mu             sync.Mutex
	lastSpawned    map[string]time.Time // domainId -> when we last spawned a refresh
	startedOnce    sync.Once
}

// NewRefreshCron constructs a cron poller pinned to the planner
// integration's engine + logger.
func NewRefreshCron(engine Engine, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}) *RefreshCron {
	return &RefreshCron{
		engine:      engine,
		logger:      logger,
		lastSpawned: make(map[string]time.Time),
	}
}

// Start kicks off the poll goroutine. Idempotent -- repeated calls are
// no-ops after the first (the integration's Start can fire more than
// once). The goroutine lives until Stop or ctx cancellation.
func (c *RefreshCron) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.startedOnce.Do(func() {
		runCtx, cancel := context.WithCancel(context.Background())
		c.mu.Lock()
		c.cancel = cancel
		c.mu.Unlock()
		_ = ctx // parent ctx is informational; the cron runs for the
		// process lifetime like the training retry loop.
		go c.loop(runCtx)
	})
}

// Stop cancels the poll goroutine.
func (c *RefreshCron) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *RefreshCron) loop(ctx context.Context) {
	if c.logger != nil {
		c.logger.Info("planner refresh cron: started", "pollInterval", refreshPollInterval.String())
	}
	ticker := time.NewTicker(refreshPollInterval)
	defer ticker.Stop()
	// Run one pass shortly after startup so a long-overdue domain
	// doesn't wait a full interval. A small delay lets the engine + DSL
	// finish loading first.
	startup := time.NewTimer(2 * time.Minute)
	defer startup.Stop()
	for {
		select {
		case <-ctx.Done():
			if c.logger != nil {
				c.logger.Info("planner refresh cron: stopped")
			}
			return
		case <-startup.C:
			c.pollOnce(ctx)
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

// pollOnce queries the candidate domains, applies the Go-side
// elapsed-time check, and spawns a refresh Plan per due domain.
func (c *RefreshCron) pollOnce(ctx context.Context) {
	if c.engine == nil {
		return
	}
	res, err := c.engine.Execute(systemActorContext(ctx), `queryDueRefreshDomains({})`)
	if err != nil {
		if c.logger != nil {
			c.logger.Debug("planner refresh cron: query failed (engine likely not ready)", "error", err)
		}
		return
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return
	}
	now := time.Now().UTC()
	spawned := 0
	for _, row := range rows {
		domainId := getString(row, "id")
		if domainId == "" {
			continue
		}
		if !c.domainIsDue(row, now) {
			continue
		}
		if !c.shouldSpawn(domainId, now) {
			continue
		}
		if err := c.spawnRefreshPlan(ctx, row); err != nil {
			if c.logger != nil {
				c.logger.Warn("planner refresh cron: spawn failed", "domainId", domainId, "error", err)
			}
			continue
		}
		c.markSpawned(domainId, now)
		spawned++
	}
	if spawned > 0 && c.logger != nil {
		c.logger.Info("planner refresh cron: spawned refresh plans", "count", spawned, "candidates", len(rows))
	}
}

// HandleDomainUpdated is the event-driven stale-signal path (Q9). It
// subscribes to graph.node.updated.v1:common:knowledgeDomain. When a
// domain's staleSignalCount crosses the threshold (bumped by the
// Planner Agent's markKnowledgeDomainStale tool), it spawns an
// immediate trainSpecialist(mode='refresh') Plan rather than waiting
// for the cadence backstop. The respawn guard dedups against the cron's
// own spawns + repeated stale-signal events for the same domain.
func (c *RefreshCron) HandleDomainUpdated(ev events.Event) {
	if c == nil || ev.Payload == nil {
		return
	}
	domainId := getString(ev.Payload, "id")
	payload, _ := ev.Payload["payload"].(map[string]any)
	if domainId == "" || payload == nil {
		return
	}
	count := intField(payload, "staleSignalCount", 0)
	if count < staleSignalRefreshThreshold {
		return
	}
	now := time.Now().UTC()
	if !c.shouldSpawn(domainId, now) {
		return
	}
	// Build a minimal row for spawnRefreshPlan from the event payload.
	row := map[string]any{
		"id":      domainId,
		"name":    getString(payload, "name"),
		"ownerId": getString(payload, "ownerId"),
	}
	if err := c.spawnRefreshPlan(context.Background(), row); err != nil {
		if c.logger != nil {
			c.logger.Warn("planner refresh cron: stale-signal spawn failed",
				"domainId", domainId, "staleSignalCount", count, "error", err)
		}
		return
	}
	c.markSpawned(domainId, now)
	if c.logger != nil {
		c.logger.Info("planner refresh cron: stale-signal triggered refresh",
			"domainId", domainId, "staleSignalCount", count)
	}
}

// domainIsDue applies the elapsed-time backstop: lastSeededAt +
// refreshCadenceDays < now. A domain that's never been seeded
// (lastSeededAt empty) is treated as due -- a refresh seeds it.
func (c *RefreshCron) domainIsDue(row map[string]any, now time.Time) bool {
	cadence := intField(row, "refreshCadenceDays", defaultRefreshCadenceDays)
	if cadence <= 0 {
		// Cadence disabled for this domain (shouldn't reach here -- the
		// query filters non-null -- but guard anyway).
		return false
	}
	lastSeeded := getString(row, "lastSeededAt")
	if lastSeeded == "" {
		return true
	}
	ts, err := time.Parse(time.RFC3339, lastSeeded)
	if err != nil {
		// Unparseable stamp -- treat as due so a malformed row still
		// gets refreshed rather than wedged forever.
		return true
	}
	window := time.Duration(cadence) * 24 * time.Hour
	return now.Sub(ts) > window
}

// shouldSpawn returns true unless we spawned a refresh for this domain
// within the respawn-guard window. Prevents re-queuing the same refresh
// every poll while the prior run is in flight.
func (c *RefreshCron) shouldSpawn(domainId string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.lastSpawned[domainId]
	if !ok {
		return true
	}
	return now.Sub(last) > refreshRespawnGuard
}

func (c *RefreshCron) markSpawned(domainId string, now time.Time) {
	c.mu.Lock()
	c.lastSpawned[domainId] = now
	c.mu.Unlock()
}

// spawnRefreshPlan inserts a trainSpecialist Plan with mode='refresh'
// for the domain. The trainSpecialist dispatcher (same process) picks
// it up off the plan-created event. requestedBy uses the domain's
// ownerId when set (a user-owned private domain), else the system
// sentinel.
func (c *RefreshCron) spawnRefreshPlan(ctx context.Context, row map[string]any) error {
	domainId := getString(row, "id")
	domainName := getString(row, "name")
	requestedBy := getString(row, "ownerId")
	if requestedBy == "" {
		requestedBy = refreshSystemRequester
	}

	planId := fmt.Sprintf("refresh:%s:%d", stripDomainId(domainId), time.Now().UnixNano())
	goal := fmt.Sprintf("Refresh knowledge domain %q (cadence backstop)", domainNameOrId(domainName, domainId))

	input := map[string]any{
		"domainId":     domainId,
		"mode":         "refresh",
		"specialistId": "", // domain-level refresh -- no single specialist
		"topic":        domainNameOrId(domainName, domainId),
	}
	inputJSON := mustJSONObject(input)

	// mode lives on input.mode (the dispatcher reads it there); the Plan
	// concept's top-level `mode` field is set by mutationCreatePlan only
	// when wired -- mutationCreatePlan's arg surface doesn't carry mode,
	// so we don't pass it. The dispatcher branches on input.mode.
	call := fmt.Sprintf(
		`mutationCreatePlan({"planId": %q, "spaceId": %q, "kind": "trainSpecialist", "goal": %q, "requestedBy": %q, "triggerSource": "system", "input": %s})`,
		planId, refreshSystemSpaceId, goal, requestedBy, inputJSON,
	)
	_, err := c.engine.Execute(systemActorContext(ctx), call)
	if err != nil {
		return err
	}
	if c.logger != nil {
		c.logger.Info("planner refresh cron: spawned trainSpecialist(refresh)",
			"planId", planId, "domainId", domainId, "requestedBy", requestedBy)
	}
	return nil
}

// --- small helpers --------------------------------------------------------

// intField reads an int-valued field, tolerating the float64 JSON
// decode shape. Returns def when missing / wrong type.
func intField(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

// domainNameOrId returns the human name when present, else the id.
func domainNameOrId(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// stripDomainId returns the last colon-segment of a domain id for use
// in a readable plan id (the full canonical id is colon-heavy).
func stripDomainId(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == ':' {
			return id[i+1:]
		}
	}
	return id
}

// mustJSONObject marshals a map to a JSON object literal for inline
// embedding in a mutationCreatePlan call. The MemQL parser accepts a
// JSON object literal as a function argument's object value (quoted
// keys, typed values).
func mustJSONObject(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

package automations

// authored_scheduler.go -- the per-owner scheduler for ACTIVATED authored
// automations (epic memql#954, issue #959, increment 2).
//
// Core automations are loaded once at engine Init and driven by the core
// Scheduler (scheduler.go). Authored automations are different: they are
// registered at RUNTIME when a bundle activates (#955 lifecycle -> active),
// owner-scoped, and must run under the AUTHOR's authz envelope -- never the
// system actor the core scheduler injects. So they get their OWN scheduler,
// running ALONGSIDE the core one, sharing the same event bus + cron primitives
// but with a distinct subscription set and a distinct actor.
//
// Lifecycle, mirroring the core scheduler's event/cron split:
//   - Activate(construct): compile the automation source, then subscribe it to
//     its event topic (event-triggered) or add a cron entry (scheduled). The
//     run executes under an AccessContext stamped with the author's userId and
//     a non-privileged role -- per-row authz then enforces, so an authored
//     automation can only touch what its author could (NO escalation).
//   - Deactivate(owner, name): tear down the subscription / cron entry.
//
// The actual step execution is delegated to a RunAutomationFunc the app wires
// to the existing Executor; the authored scheduler owns subscription
// management + the author-envelope construction, not step mechanics.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/robfig/cron/v3"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// RunAutomationFunc executes one compiled authored automation under the
// already-author-scoped ctx, with the triggering event (nil for a scheduled
// firing). The app wires this to the live Executor; tests supply a recorder.
// It returns an error so the per-automation circuit breaker (increment 3) can
// observe faults.
type RunAutomationFunc func(ctx context.Context, automation *Automation, triggeringEvent *events.Event) error

// AuthoredSchedulerOptions configures the authored scheduler.
type AuthoredSchedulerOptions struct {
	Logger   *slog.Logger
	Loader   *Loader
	EventBus *events.Bus
	// Run executes a compiled authored automation. Required.
	Run RunAutomationFunc
}

// authoredEntry is one live authored automation's subscription bookkeeping:
// the compiled automation, the owner it runs as, and the teardown handle
// (event unsubscribe or cron entry id) so Deactivate can remove exactly it.
type authoredEntry struct {
	owner      string
	automation *Automation
	unsub      func()       // event-triggered teardown; nil for scheduled
	cronEntry  cron.EntryID // scheduled teardown; 0 for event-triggered
	scheduled  bool
}

// AuthoredScheduler subscribes activated authored automations to their
// triggers and runs them under the author's authz envelope. It is thread-safe
// and mutable after startup (activations arrive at runtime).
type AuthoredScheduler struct {
	logger   *slog.Logger
	loader   *Loader
	eventBus *events.Bus
	run      RunAutomationFunc

	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]*authoredEntry // keyed by authoredEntryKey(owner, name)
}

// NewAuthoredScheduler builds the authored scheduler. Loader, EventBus and Run
// are required. A nil logger is tolerated.
func NewAuthoredScheduler(opts AuthoredSchedulerOptions) (*AuthoredScheduler, error) {
	if opts.Loader == nil {
		return nil, fmt.Errorf("authored scheduler: loader is required")
	}
	if opts.EventBus == nil {
		return nil, fmt.Errorf("authored scheduler: event bus is required")
	}
	if opts.Run == nil {
		return nil, fmt.Errorf("authored scheduler: run func is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = NewLogger()
	}
	c := cron.New(cron.WithSeconds())
	c.Start()
	return &AuthoredScheduler{
		logger:   logger,
		loader:   opts.Loader,
		eventBus: opts.EventBus,
		run:      opts.Run,
		cron:     c,
		entries:  make(map[string]*authoredEntry),
	}, nil
}

// authoredEntryKey scopes a live entry by owner + automation name so two
// owners' identically-named authored automations never collide.
func authoredEntryKey(owner, name string) string {
	return owner + "\x00" + name
}

// Activate compiles the authored construct's automation source and wires its
// trigger (event subscription or cron entry). The construct MUST be an
// `automation` kind carrying its `.memql` source on construct.Source; its
// ownerUserId is the authz envelope the runs execute under. Re-activating the
// same (owner, name) replaces the prior subscription in place (a version bump).
func (s *AuthoredScheduler) Activate(construct *memql.AuthoredConstruct) error {
	if construct == nil {
		return fmt.Errorf("authored scheduler: construct is nil")
	}
	if construct.Kind != "automation" {
		return fmt.Errorf("authored scheduler: construct %q is kind %q, only automation is schedulable", construct.Name, construct.Kind)
	}
	if construct.OwnerUserId == "" {
		return fmt.Errorf("authored scheduler: construct %q has no ownerUserId", construct.Name)
	}

	origin := fmt.Sprintf("authored:%s:%s", construct.OwnerUserId, construct.Name)
	automation, err := s.loader.CompileSource(construct.Source, origin)
	if err != nil {
		return fmt.Errorf("authored scheduler: compile %s: %w", origin, err)
	}
	if automation.Name != construct.Name {
		return fmt.Errorf("authored scheduler: construct name %q does not match automation name %q in source", construct.Name, automation.Name)
	}

	// Replace any prior subscription for this (owner, name) before re-wiring.
	s.Deactivate(construct.OwnerUserId, construct.Name)

	entry := &authoredEntry{owner: construct.OwnerUserId, automation: automation}

	switch {
	case automation.IsEventTriggered():
		unsub := s.subscribeEvent(construct.OwnerUserId, automation)
		entry.unsub = unsub
	case automation.IsScheduled():
		id, err := s.scheduleCron(construct.OwnerUserId, automation)
		if err != nil {
			return fmt.Errorf("authored scheduler: schedule %s: %w", origin, err)
		}
		entry.cronEntry = id
		entry.scheduled = true
	default:
		return fmt.Errorf("authored scheduler: automation %q has neither an event trigger nor a schedule", construct.Name)
	}

	s.mu.Lock()
	s.entries[authoredEntryKey(construct.OwnerUserId, construct.Name)] = entry
	s.mu.Unlock()

	if s.logger != nil {
		s.logger.Info("authored automation activated",
			"owner", construct.OwnerUserId, "automation", construct.Name,
			"scheduled", entry.scheduled, "version", construct.Version)
	}
	return nil
}

// Deactivate tears down a live authored automation's trigger (event
// unsubscribe or cron-entry removal). A no-op when nothing is registered for
// (owner, name), so it doubles as the idempotent pre-step of Activate.
func (s *AuthoredScheduler) Deactivate(owner, name string) {
	s.mu.Lock()
	key := authoredEntryKey(owner, name)
	entry, ok := s.entries[key]
	if ok {
		delete(s.entries, key)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if entry.unsub != nil {
		entry.unsub()
	}
	if entry.scheduled {
		s.cron.Remove(entry.cronEntry)
	}
	if s.logger != nil {
		s.logger.Info("authored automation deactivated", "owner", owner, "automation", name)
	}
}

// ActiveCount returns how many authored automations are currently wired.
func (s *AuthoredScheduler) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// IsActive reports whether (owner, name) is currently wired.
func (s *AuthoredScheduler) IsActive(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[authoredEntryKey(owner, name)]
	return ok
}

// Stop tears down every subscription + the cron scheduler. After Stop the
// scheduler is unusable.
func (s *AuthoredScheduler) Stop() {
	s.mu.Lock()
	entries := s.entries
	s.entries = make(map[string]*authoredEntry)
	s.mu.Unlock()
	for _, e := range entries {
		if e.unsub != nil {
			e.unsub()
		}
	}
	ctx := s.cron.Stop()
	<-ctx.Done()
}

// subscribeEvent wires an event-triggered authored automation. The handler
// runs under the author's authz envelope.
func (s *AuthoredScheduler) subscribeEvent(owner string, automation *Automation) func() {
	a := automation
	pattern := automation.Trigger.Event
	return s.eventBus.Subscribe(pattern, func(event events.Event) {
		ev := event
		s.runUnderAuthor(owner, a, &ev)
	}, events.WithSubscriberName("authored:"+owner+":"+a.Name))
}

// scheduleCron wires a scheduled authored automation.
func (s *AuthoredScheduler) scheduleCron(owner string, automation *Automation) (cron.EntryID, error) {
	a := automation
	return s.cron.AddFunc(automation.Schedule, func() {
		s.runUnderAuthor(owner, a, nil)
	})
}

// runUnderAuthor executes one authored automation firing under the AUTHOR's
// authz envelope: a ctx carrying an AccessContext + claims stamped with the
// author's userId and a non-privileged WRITER role. This is the no-escalation
// guarantee -- the authored automation runs as its author, so the same per-row
// authz that gates the author's interactive calls gates this run. It can never
// act as a cluster owner or the system actor.
func (s *AuthoredScheduler) runUnderAuthor(owner string, automation *Automation, event *events.Event) {
	ctx := AuthorContext(context.Background(), owner)
	if err := s.run(ctx, automation, event); err != nil {
		if s.logger != nil {
			s.logger.Error("authored automation run failed",
				"owner", owner, "automation", automation.Name, "error", err)
		}
	}
}

// AuthorContext returns a ctx carrying the AUTHOR's authz envelope: an
// AccessContext + JWT-style claims stamped with the author's userId and a
// non-privileged writer role. Per-row authz reads this to confine the run to
// what the author may touch. Exported so the app's RunAutomationFunc wiring and
// tests can build the same envelope.
//
// Deliberately NOT the system actor (role:system) the core scheduler injects:
// authored automations must not get the engine-wide system bypass.
func AuthorContext(ctx context.Context, ownerUserId string) context.Context {
	claims := map[string]any{
		"sub":   ownerUserId,
		"email": ownerUserId,
		"role":  string(auth.RoleWriter),
	}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: ownerUserId,
		Role:   auth.RoleWriter,
	})
}

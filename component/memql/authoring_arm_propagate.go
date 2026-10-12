package memql

// authoring_arm_propagate.go -- arming and disarming an authored bundle on
// EVERY replica (memql#6022, F1).
//
// ActivateApprovedBundle registers a bundle's constructs into the authored
// runtime it is handed -- the registry and scheduler of the node that handled
// the activation -- and RetireActiveBundle tears them down there. Until this
// file nothing told any other replica: each learned of an activation, and of a
// retire, only by restarting (RearmActiveBundles arms what is active at boot).
// An event rule fires on the node its triggering write lands on, so a rule
// activated on one node fired for no write on any other, and a rule retired on
// one node kept firing on every replica that had armed it at boot.
//
// Two mechanisms, one rule:
//
//   - BROADCASTS, for speed. A successful activation emits
//     authoring.arm.<bundleId>, a successful retire authoring.disarm.<bundleId>,
//     carrying only the bundle id and its owner; two broadcast routing rules
//     (component/node/routing.go) forward both to every node, and every node
//     reads the bundle from the shared database and arms or disarms it in its
//     own runtime.
//   - A RECONCILE PASS, for truth. Broadcasts are best effort: a transport
//     drops one, a node still starting misses one. So every node also walks
//     the database's active bundles when the arming starts and again every
//     armReconcileInterval (jittered), arming what it lacks or holds stale and
//     taking out what the database no longer calls active.
//
// THE RULE BOTH APPLY: the database decides, and a construct's identity is its
// bundle, version AND source. Version alone is not an identity -- an email rule
// restarts at version 1 under a reused bundle id after a pause, and a routing
// rule is always version 1 under a name-derived id -- so "the registry holds
// this name at an equal or newer version" says nothing about whether it holds
// THIS construct. Concretely:
//
//   - An entry is already armed only when bundle, version and source all match
//     (sameAuthoredConstruct).
//   - Arming replaces any other entry under the name -- a stale copy of the
//     same bundle, an older version, or anything whose own bundle no longer
//     reads active -- except one whose own bundle still reads active at the
//     same or a newer version (armConstruct): that is two live bundles
//     claiming one name, which the registry's version rule always refused. An
//     older version is replaced by registering over it, so when this node
//     refuses the newer one the older one keeps firing while its own bundle
//     still reads active (armOver).
//   - Disarming -- a retire's teardown, a disarm broadcast, a superseded
//     version's leftovers, the reconcile sweep -- takes an entry out only when
//     its OWN bundle no longer reads active (disarmStaleEntries), never by name.
//   - Every read that decides is fresh: never this node's result cache, which
//     hears about another node's write by a broadcast of its own.
//
// And the mechanics that keep it correct with two replicas of every node:
//
//   - The origin skips its own copy of a broadcast: it armed (or disarmed) in
//     the call that published, and an event with no origin node is its own.
//   - Only the engine's kind is accepted: an authored event step can publish
//     any topic, but not KindAuthoringArm / KindAuthoringDisarm.
//   - One change at a time. The bus delivers every event on its own goroutine,
//     so an arm and the disarm right behind it can arrive in either order.
//     Broadcasts, reconcile passes and this node's own activations and retires
//     all take one lock, and each reads the database inside it, so the last
//     one applied agrees with the last write.
//
// Not authoring.promote: promote re-hydrates PLAIN constructs into the shared
// registry every caller resolves through; arming registers owner-scoped
// constructs -- automations among them -- into each node's authored registry
// and scheduler, the state the boot re-arm produces.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

const (
	// authoringArmPattern / authoringDisarmPattern match every arm and disarm
	// topic regardless of how many segments a bundle id spans.
	authoringArmPattern    = "authoring.arm.#"
	authoringDisarmPattern = "authoring.disarm.#"

	// armApplyTimeout bounds the reads one broadcast makes. They run under the
	// arming lock, so a stalled database must not hold every later change
	// behind it indefinitely.
	armApplyTimeout = 30 * time.Second

	// armReconcileInterval is how often each node reconciles its authored
	// runtime against the database, and armReconcileJitter how far a pass may
	// land either side of it -- spread, so the replicas do not all read at once.
	armReconcileInterval = 5 * time.Minute
	armReconcileJitter   = time.Minute

	// armReconcileTimeout bounds one reconcile pass, which holds the arming
	// lock while it walks every active bundle.
	armReconcileTimeout = 2 * time.Minute

	// maxActiveBundlePages bounds the walk over the paginated active-bundle
	// listing (50 a page), so a cursor that never ends cannot spin a pass.
	maxActiveBundlePages = 1000
)

// authoredArming is a node's half of the arm/disarm broadcast: the deps a
// peer's activation is armed into, and the lock that applies one change to
// them at a time.
type authoredArming struct {
	deps     atomic.Pointer[func() AuthoredRuntimeDeps]
	applying sync.Mutex

	// The listeners told when this node records a breaker's pause
	// (SetAuthoredPauseListener).
	listenMu       sync.Mutex
	pauseListeners map[string]AuthoredPauseListener
}

// SetAuthoredRuntimeDeps hands the engine this node's authored runtime -- the
// registry and the scheduler register / deregister hooks -- so broadcasts and
// reconcile passes can arm bundles activated elsewhere into it. app/ calls it
// once the registry and scheduler exist (wireAuthoredRuntime); a binary that
// never builds the authored runtime never calls it, and arming is inert there.
// Safe to call against a running engine; nil clears it.
func (e *MemQLEngine) SetAuthoredRuntimeDeps(deps func() AuthoredRuntimeDeps) {
	if e == nil {
		return
	}
	if deps == nil {
		e.arming.deps.Store(nil)
		return
	}
	e.arming.deps.Store(&deps)
}

// authoredRuntimeDeps returns the deps SetAuthoredRuntimeDeps installed, and
// false when there is no authored runtime to arm into.
func (e *MemQLEngine) authoredRuntimeDeps() (AuthoredRuntimeDeps, bool) {
	p := e.arming.deps.Load()
	if p == nil || *p == nil {
		return AuthoredRuntimeDeps{}, false
	}
	deps := (*p)()
	return deps, deps.Registry != nil
}

// StartAuthoringArmSubscriber starts this node's arming: it subscribes to the
// authoring.arm.* and authoring.disarm.* broadcasts and THEN reconciles against
// the database -- immediately, which closes the window between the boot re-arm
// and the subscription, and every armReconcileInterval after. Scoped to ctx:
// cancelling ctx (engine stop) unsubscribes and ends the loop. The engine
// bootstrap calls it once at startup, beside the promote and demote
// subscribers; exported so a cross-node harness can stand two engines up the
// same way.
func (e *MemQLEngine) StartAuthoringArmSubscriber(ctx context.Context) {
	e.startAuthoringArming(ctx, &engineArmStore{engine: e})
}

// startAuthoringArming is StartAuthoringArmSubscriber over an injected store:
// the subscription, then the reconcile loop.
func (e *MemQLEngine) startAuthoringArming(ctx context.Context, store armStore) {
	if e == nil || e.eventBus == nil {
		return
	}
	e.startAuthoringArmSubscriberWithStore(ctx, store)
	go e.reconcileArmingLoop(ctx, store)
}

// startAuthoringArmSubscriberWithStore subscribes to the two broadcasts and
// nothing else -- no reconcile loop -- so a test sees exactly what the
// broadcasts do.
func (e *MemQLEngine) startAuthoringArmSubscriberWithStore(ctx context.Context, store armStore) {
	if e == nil || e.eventBus == nil {
		return
	}
	unsubArm := e.eventBus.Subscribe(authoringArmPattern, func(ev events.Event) {
		e.applyAuthoringBroadcast(ctx, store, ev, false)
	}, events.WithSubscriberName("authoring:arm:propagation"))
	unsubDisarm := e.eventBus.Subscribe(authoringDisarmPattern, func(ev events.Event) {
		e.applyAuthoringBroadcast(ctx, store, ev, true)
	}, events.WithSubscriberName("authoring:disarm:propagation"))
	go func() {
		<-ctx.Done()
		unsubArm()
		unsubDisarm()
	}()
}

// ReconcileAuthoredRuntime runs one reconcile pass now: arm every active bundle
// this node lacks or holds stale, and take out every entry whose bundle the
// database no longer calls active. The arming loop runs one at startup and
// every armReconcileInterval; exported so a cross-node harness can drive a pass
// deterministically. A pass on a node with no authored runtime does nothing.
func (e *MemQLEngine) ReconcileAuthoredRuntime(ctx context.Context) error {
	if e == nil {
		return nil
	}
	_, err := e.reconcileAuthoredRuntime(ctx, &engineArmStore{engine: e})
	return err
}

// reconcileAuthoredRuntime is one pass over an injected store, under the
// arming lock.
func (e *MemQLEngine) reconcileAuthoredRuntime(ctx context.Context, store armStore) (armOutcome, error) {
	deps, ok := e.authoredRuntimeDeps()
	if !ok {
		return armOutcome{Ignored: "no authored runtime on this node"}, nil
	}
	e.arming.applying.Lock()
	defer e.arming.applying.Unlock()
	passCtx, cancel := context.WithTimeout(ctx, armReconcileTimeout)
	defer cancel()
	return reconcileWithStore(passCtx, store, deps)
}

// reconcileArmingLoop runs a pass now and then every armReconcileInterval,
// jittered, until ctx ends.
func (e *MemQLEngine) reconcileArmingLoop(ctx context.Context, store armStore) {
	for {
		out, err := e.reconcileAuthoredRuntime(ctx, store)
		e.logReconcile(out, err)
		timer := time.NewTimer(nextArmReconcile())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// nextArmReconcile is armReconcileInterval, give or take half of
// armReconcileJitter.
func nextArmReconcile() time.Duration {
	return armReconcileInterval - armReconcileJitter/2 + rand.N(armReconcileJitter)
}

func (e *MemQLEngine) logReconcile(out armOutcome, err error) {
	logger := e.safeLogger()
	if logger == nil {
		return
	}
	switch {
	case err != nil:
		logger.Warn("authored runtime reconcile failed; the next pass retries", "error", err)
	case out.Registered > 0 || out.Retired > 0 || out.Conflicts > 0 || len(out.Failed) > 0:
		logger.Info("authored runtime reconciled against the database",
			"registered", out.Registered, "retired", out.Retired, "alreadyHeld", out.AlreadyHeld,
			"conflicts", out.Conflicts, "failedBundles", out.Failed)
	default:
		logger.Debug("authored runtime already agrees with the database",
			"alreadyHeld", out.AlreadyHeld, "ignored", out.Ignored)
	}
}

// applyAuthoringBroadcast applies one arm or disarm broadcast to this node.
func (e *MemQLEngine) applyAuthoringBroadcast(ctx context.Context, store armStore, ev events.Event, disarm bool) {
	// The origin armed or disarmed in the call that published this; its own
	// copy -- the only one that arrives with no origin node -- has nothing left
	// to do here.
	if ev.OriginNodeId == "" {
		return
	}
	verb, kind, parse := "arm", events.KindAuthoringArm, events.BundleFromAuthoringArmTopic
	if disarm {
		verb, kind, parse = "disarm", events.KindAuthoringDisarm, events.BundleFromAuthoringDisarmTopic
	}
	// The engine is the only publisher of these kinds: an authored event step
	// can put any topic on the bus, but not this kind.
	if ev.Kind != kind {
		return
	}
	bundleId, ok := parse(ev.Topic)
	if !ok {
		return
	}
	owner, _ := ev.Payload["owner"].(string)
	owner = strings.TrimSpace(owner)
	logger := e.safeLogger()
	if owner == "" {
		if logger != nil {
			logger.Warn("authoring "+verb+" propagation: broadcast names no owner; nothing to read the bundle as",
				"bundleId", bundleId, "originNode", ev.OriginNodeId)
		}
		return
	}
	deps, ok := e.authoredRuntimeDeps()
	if !ok {
		if logger != nil {
			logger.Debug("authoring "+verb+" propagation: no authored runtime on this node; skipped",
				"bundleId", bundleId, "originNode", ev.OriginNodeId)
		}
		return
	}

	e.arming.applying.Lock()
	defer e.arming.applying.Unlock()
	applyCtx, cancel := context.WithTimeout(ctx, armApplyTimeout)
	defer cancel()

	var (
		out armOutcome
		err error
	)
	if disarm {
		out, err = disarmBundleWithStore(applyCtx, store, owner, bundleId, deps)
	} else {
		out, err = armBundleWithStore(applyCtx, store, owner, bundleId, deps)
	}
	if logger == nil {
		return
	}
	if err != nil {
		logger.Warn("authoring "+verb+" propagation failed; the next reconcile pass applies the bundle's current state",
			"bundleId", bundleId, "owner", owner, "originNode", ev.OriginNodeId,
			"registered", out.Registered, "retired", out.Retired, "error", err)
		return
	}
	logger.Info("authoring "+verb+" propagation applied",
		"bundleId", bundleId, "owner", owner, "originNode", ev.OriginNodeId,
		"registered", out.Registered, "alreadyHeld", out.AlreadyHeld, "conflicts", out.Conflicts,
		"retired", out.Retired, "ignored", out.Ignored)
}

// armOutcome is what one broadcast or reconcile pass changed on this node, for
// its log line.
type armOutcome struct {
	// Registered counts constructs armed into this node's runtime.
	Registered int
	// AlreadyHeld counts constructs this node already held, identically.
	AlreadyHeld int
	// Conflicts counts constructs left alone because another bundle that
	// still reads active holds the name.
	Conflicts int
	// Retired counts entries taken out because their own bundle no longer
	// reads active.
	Retired int
	// Failed names the bundles a reconcile pass could not apply; the next
	// pass retries them.
	Failed []string
	// Ignored says why a broadcast changed nothing, when the database says
	// there is nothing to change.
	Ignored string
}

func (o *armOutcome) add(other armOutcome) {
	o.Registered += other.Registered
	o.AlreadyHeld += other.AlreadyHeld
	o.Conflicts += other.Conflicts
	o.Retired += other.Retired
}

// armBundleWithStore arms one bundle into deps from its rows as the database
// holds them now. Only an active bundle arms.
func armBundleWithStore(ctx context.Context, store armStore, owner, bundleId string, deps AuthoredRuntimeDeps) (armOutcome, error) {
	bundle, err := store.LoadBundleForOwner(ctx, owner, bundleId)
	if err != nil {
		return armOutcome{}, fmt.Errorf("authoring: load bundle %q to arm: %w", bundleId, err)
	}
	if bundle.Id == "" {
		return armOutcome{Ignored: "the bundle is not readable as its owner"}, nil
	}
	if bundle.Status != BundleActive {
		return armOutcome{Ignored: fmt.Sprintf("the bundle is %s, not active", bundle.Status)}, nil
	}
	live := newBundleLiveness(store.LoadBundleForOwner)
	live.note(bundle.Id, true)
	return armActiveBundle(ctx, store, bundle, deps, live)
}

// armActiveBundle arms a bundle the caller has read as active -- a broadcast's
// bundle, or one a reconcile pass listed -- the way a fresh activation does,
// then takes out the leftovers of the version it supersedes: constructs it no
// longer registers, wherever their own bundle no longer reads active.
func armActiveBundle(ctx context.Context, store armStore, bundle AuthoringBundleRow, deps AuthoredRuntimeDeps, live *bundleLiveness) (armOutcome, error) {
	var out armOutcome
	constructs, err := store.LoadConstructsForOwner(ctx, bundle.OwnerUserId, bundle.Id)
	if err != nil {
		return out, fmt.Errorf("authoring: load constructs to arm %q: %w", bundle.Id, err)
	}
	// The same planning step the boot re-arm takes (rearmOneBundle): an active
	// bundle is presented as dryRunPassed to the pure planner, so the runtime
	// state matches a fresh activation. The persisted row is not touched.
	planning := bundle
	planning.Status = BundleDryRunPassed
	plan, err := PlanBundleActivation(planning, constructs)
	if err != nil {
		return out, fmt.Errorf("authoring: plan arm of %q: %w", bundle.Id, err)
	}
	for _, c := range plan.Register {
		verdict, other, err := armConstruct(ctx, c, deps, live)
		if err != nil {
			return out, err
		}
		switch verdict {
		case armRegistered:
			out.Registered++
		case armAlreadyHeld:
			out.AlreadyHeld++
		case armConflict:
			out.Conflicts++
			if deps.Logger != nil {
				deps.Logger.Warn("authored construct left armed for another active bundle: two active bundles define one name",
					"owner", c.OwnerUserId, "construct", c.Kind+":"+c.Name, "bundleId", bundle.Id, "heldFor", other.BundleId)
			}
		}
	}
	if plan.SupersedesBundleId == "" {
		return out, nil
	}
	prior, err := store.LoadConstructsForOwner(ctx, bundle.OwnerUserId, plan.SupersedesBundleId)
	if err != nil {
		return out, fmt.Errorf("authoring: load constructs of superseded bundle %q: %w", plan.SupersedesBundleId, err)
	}
	retired, err := disarmStaleEntries(ctx, prior, plan.registeredKeys(), deps, live)
	out.Retired += retired
	return out, err
}

// disarmBundleWithStore applies a disarm broadcast: the retired bundle's
// constructs come out of deps wherever the entry under the name belongs to a
// bundle that no longer reads active -- the retired one, or an older one this
// node never heard superseded. A bundle the database still calls active is
// left alone: the broadcast is late (it was re-armed since) or not the
// engine's.
func disarmBundleWithStore(ctx context.Context, store armStore, owner, bundleId string, deps AuthoredRuntimeDeps) (armOutcome, error) {
	var out armOutcome
	bundle, err := store.LoadBundleForOwner(ctx, owner, bundleId)
	if err != nil {
		return out, fmt.Errorf("authoring: load bundle %q to disarm: %w", bundleId, err)
	}
	if bundle.Id == "" {
		out.Ignored = "the bundle is not readable as its owner"
		return out, nil
	}
	if bundle.Status == BundleActive {
		out.Ignored = "the bundle is active"
		return out, nil
	}
	constructs, err := store.LoadConstructsForOwner(ctx, owner, bundleId)
	if err != nil {
		return out, fmt.Errorf("authoring: load constructs to disarm %q: %w", bundleId, err)
	}
	live := newBundleLiveness(store.LoadBundleForOwner)
	live.note(bundle.Id, false)
	out.Retired, err = disarmStaleEntries(ctx, constructs, nil, deps, live)
	return out, err
}

// reconcileWithStore makes deps agree with the database: every active bundle is
// armed (armActiveBundle -- so a stale or missing entry is fixed and a held one
// left alone), and every entry whose own bundle no longer reads active is taken
// out. A bundle that fails is named in Failed and the pass goes on; the error
// return is for a pass that could not list the active bundles at all.
//
// The sweep reads each entry's bundle by id rather than trusting its absence
// from the listing, so a listing that came back short can arm too little but
// never retire a live construct.
func reconcileWithStore(ctx context.Context, store armStore, deps AuthoredRuntimeDeps) (armOutcome, error) {
	var out armOutcome
	bundles, err := store.LoadActiveBundles(ctx)
	if err != nil {
		return out, fmt.Errorf("authoring: list the active bundles to reconcile against: %w", err)
	}
	live := newBundleLiveness(store.LoadBundleForOwner)
	for _, b := range bundles {
		live.note(b.Id, true)
	}
	for _, b := range bundles {
		got, err := armActiveBundle(ctx, store, b, deps, live)
		out.add(got)
		if err != nil {
			out.Failed = append(out.Failed, b.Id)
			if deps.Logger != nil {
				deps.Logger.Warn("authored bundle could not be reconciled; the next pass retries it",
					"bundleId", b.Id, "owner", b.OwnerUserId, "error", err)
			}
		}
	}
	for _, h := range deps.Registry.ListAll() {
		if strings.TrimSpace(h.BundleId) == "" {
			continue // not armed from a bundle; nothing to check it against
		}
		active, err := live.active(ctx, h.OwnerUserId, h.BundleId)
		if err != nil {
			out.Failed = append(out.Failed, h.BundleId)
			continue
		}
		if !active && disarmEntry(h, deps) {
			out.Retired++
		}
	}
	return out, nil
}

// armVerdict is what arming one construct did.
type armVerdict int

const (
	// armRegistered: armed, replacing whatever stale entry held the name.
	armRegistered armVerdict = iota
	// armAlreadyHeld: this exact construct was already armed.
	armAlreadyHeld
	// armConflict: another bundle that still reads active holds the name at
	// the same or a newer version.
	armConflict
)

// armConstruct puts c into deps -- the registry entry and, for an automation,
// the scheduler entry -- unless deps already holds exactly c, or holds the name
// for another bundle that still reads active at the same or a newer version.
// Returns the verdict and the entry that was held under the name, if any.
//
// What happens to a held entry it replaces depends on what that entry is:
//
//   - A stale copy of c's own bundle, or the name held for another bundle that
//     no longer reads active: it comes out FIRST, scheduler included, so if c
//     is then refused nothing fires rather than a definition the database has
//     replaced.
//   - An OLDER version of another bundle -- the registry's own replacement
//     rule, and what lets an activation register its new version before it
//     retires the one it supersedes: c registers OVER it (armOver), and if c
//     is refused on this node the held entry stays wired unless its own
//     bundle no longer reads active. Taking it out first meant a version a node
//     could not run took the one it could run down with it -- and, while both
//     read active, every reconcile pass armed the old one again only to tear
//     it down again.
func armConstruct(ctx context.Context, c *AuthoredConstruct, deps AuthoredRuntimeDeps, live *bundleLiveness) (armVerdict, *AuthoredConstruct, error) {
	held, ok := deps.Registry.Lookup(c.OwnerUserId, c.Kind, c.Name)
	if !ok {
		return armRegistered, nil, registerArmed(c, deps)
	}
	if sameAuthoredConstruct(held, c) {
		return armAlreadyHeld, held, nil
	}
	if held.BundleId != c.BundleId {
		if held.Version < c.Version {
			return armRegistered, held, armOver(ctx, held, c, deps, live)
		}
		active, err := live.active(ctx, held.OwnerUserId, held.BundleId)
		if err != nil {
			return armConflict, held, fmt.Errorf("authoring: read bundle %q, which holds %s %q: %w", held.BundleId, c.Kind, c.Name, err)
		}
		if active {
			return armConflict, held, nil
		}
	}
	disarmEntry(held, deps)
	return armRegistered, held, registerArmed(c, deps)
}

// registerArmed registers c into deps and leaves nothing half-armed: an entry
// the scheduler refused would read as held from then on and never be retried.
func registerArmed(c *AuthoredConstruct, deps AuthoredRuntimeDeps) error {
	if err := registerBundleConstructs([]*AuthoredConstruct{c}, deps); err != nil {
		if cur, ok := deps.Registry.Lookup(c.OwnerUserId, c.Kind, c.Name); ok && sameAuthoredConstruct(cur, c) {
			_ = deps.Registry.Retire(c.OwnerUserId, c.Kind, c.Name)
		}
		return err
	}
	return nil
}

// armOver registers c over held, an older version of another bundle, without
// taking held out first. The scheduler is asked first, and a refusal there
// changes nothing -- a refused register hook leaves the entry it would have
// replaced in place (AuthoredRegisterHook) -- so held keeps firing, unless its
// own bundle no longer reads active: a definition the database has retired
// must not keep running because its successor cannot. A held entry whose
// bundle cannot be read is left for the next reconcile pass to decide.
func armOver(ctx context.Context, held, c *AuthoredConstruct, deps AuthoredRuntimeDeps, live *bundleLiveness) error {
	if c.Kind == "automation" && deps.RegisterHook != nil {
		if err := deps.RegisterHook(c); err != nil {
			if active, rerr := live.active(ctx, held.OwnerUserId, held.BundleId); rerr == nil && !active {
				disarmEntry(held, deps)
			}
			return fmt.Errorf("authoring: schedule automation %s: %w", c.Name, err)
		}
	}
	// A newer version replaces the held entry in place (the registry's own
	// rule), so this cannot be refused for the version; should it be refused
	// anyway, the scheduler goes back to what the registry still holds.
	if err := deps.Registry.Register(c); err != nil {
		if c.Kind == "automation" && deps.RegisterHook != nil {
			_ = deps.RegisterHook(held)
		}
		return fmt.Errorf("authoring: register %s/%s into runtime: %w", c.Kind, c.Name, err)
	}
	return nil
}

// sameAuthoredConstruct reports whether held is exactly c: the same bundle,
// version and source.
func sameAuthoredConstruct(held, c *AuthoredConstruct) bool {
	return held.BundleId == c.BundleId && held.Version == c.Version && held.Source == c.Source
}

// disarmStaleEntries takes out the entry under each construct's name whose own
// bundle no longer reads active, skipping names in keep. An entry armed for a
// bundle that is still live -- the superseding version, the resumed version of
// a paused rule -- stays. Every construct is tried; the first read error is
// returned after the rest.
func disarmStaleEntries(ctx context.Context, constructs []AuthoringConstructRow, keep map[string]bool, deps AuthoredRuntimeDeps, live *bundleLiveness) (int, error) {
	retired := 0
	var firstErr error
	for _, c := range constructs {
		if keep[constructKey(c.Kind, c.Name)] {
			continue
		}
		held, ok := deps.Registry.Lookup(c.OwnerUserId, c.Kind, c.Name)
		if !ok || strings.TrimSpace(held.BundleId) == "" {
			continue
		}
		active, err := live.active(ctx, held.OwnerUserId, held.BundleId)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("authoring: read bundle %q, which holds %s %q: %w", held.BundleId, c.Kind, c.Name, err)
			}
			continue
		}
		if !active && disarmEntry(held, deps) {
			retired++
		}
	}
	return retired, firstErr
}

// disarmEntry takes one held construct out of deps: its scheduler entry when it
// is an automation, then its registry entry.
func disarmEntry(held *AuthoredConstruct, deps AuthoredRuntimeDeps) bool {
	if held.Kind == "automation" && deps.DeregisterHook != nil {
		deps.DeregisterHook(held.OwnerUserId, held.Name)
	}
	return deps.Registry.Retire(held.OwnerUserId, held.Kind, held.Name) == nil
}

// bundleLiveness answers whether a bundle reads active right now, with one
// read per bundle per pass and whatever the caller already knows noted up
// front.
type bundleLiveness struct {
	read  func(ctx context.Context, owner, bundleId string) (AuthoringBundleRow, error)
	known map[string]bool
}

func newBundleLiveness(read func(ctx context.Context, owner, bundleId string) (AuthoringBundleRow, error)) *bundleLiveness {
	return &bundleLiveness{read: read, known: map[string]bool{}}
}

func (l *bundleLiveness) note(bundleId string, active bool) {
	l.known[bundleId] = active
}

func (l *bundleLiveness) active(ctx context.Context, owner, bundleId string) (bool, error) {
	if v, ok := l.known[bundleId]; ok {
		return v, nil
	}
	b, err := l.read(ctx, owner, bundleId)
	if err != nil {
		return false, err
	}
	v := b.Id != "" && b.Status == BundleActive
	l.known[bundleId] = v
	return v, nil
}

// publishAuthoringArm emits the authoring.arm.<bundleId> broadcast for a
// bundle this node just activated. ONLY the authoring-arm subscriber consumes
// the channel, and a single broadcast routing rule (authoring.arm.*) forwards
// it to every node. Best-effort: an engine without a bus (a non-mesh or test
// engine) arms only itself, and every other node arms the bundle at its next
// reconcile pass.
func (e *MemQLEngine) publishAuthoringArm(bundleId, owner string) {
	e.publishAuthoringBroadcast(events.TopicAuthoringArmForBundle, events.KindAuthoringArm, bundleId, owner)
}

// publishAuthoringDisarm emits the authoring.disarm.<bundleId> broadcast for a
// bundle this node just retired; the inverse of publishAuthoringArm.
func (e *MemQLEngine) publishAuthoringDisarm(bundleId, owner string) {
	e.publishAuthoringBroadcast(events.TopicAuthoringDisarmForBundle, events.KindAuthoringDisarm, bundleId, owner)
}

func (e *MemQLEngine) publishAuthoringBroadcast(topic func(string) string, kind events.Kind, bundleId, owner string) {
	if e == nil || e.eventBus == nil {
		return
	}
	bundleId = strings.TrimSpace(bundleId)
	if bundleId == "" {
		return
	}
	e.eventBus.Publish(events.NewEvent(topic(bundleId), kind,
		map[string]any{"bundleId": bundleId, "owner": owner}))
}

// armStore is the narrow graph surface arming reads: the active bundles across
// owners (a reconcile pass), and one bundle and its constructs under the bundle
// AUTHOR's envelope. *MemQLEngine satisfies it via engineArmStore; tests fake
// it.
type armStore interface {
	LoadActiveBundles(ctx context.Context) ([]AuthoringBundleRow, error)
	LoadBundleForOwner(ctx context.Context, owner, bundleId string) (AuthoringBundleRow, error)
	LoadConstructsForOwner(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error)
}

// engineArmStore is the production armStore over a live engine. Every read is
// fresh: it decides what this node arms, and the write it follows reaches this
// node's result cache only by an asynchronous broadcast of its own.
type engineArmStore struct {
	engine *MemQLEngine
}

// LoadActiveBundles is the boot re-arm's read -- systemActiveAuthoringBundles
// under the cluster-owner envelope, durable-promote bundles skipped -- made
// fresh and walked to its last page.
func (s *engineArmStore) LoadActiveBundles(ctx context.Context) ([]AuthoringBundleRow, error) {
	listCtx := ContextWithFreshRead(rearmClusterOwnerContext(ctx))
	return pageActiveBundles(listCtx, func(pageCtx context.Context) (*ExecuteResult, error) {
		return s.engine.Execute(pageCtx, "systemActiveAuthoringBundles()")
	})
}

func (s *engineArmStore) LoadBundleForOwner(ctx context.Context, owner, bundleId string) (AuthoringBundleRow, error) {
	return (&engineActivationStore{engine: s.engine}).LoadBundle(armReadContext(ctx, owner), bundleId)
}

func (s *engineArmStore) LoadConstructsForOwner(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	return (&engineActivationStore{engine: s.engine}).LoadConstructs(armReadContext(ctx, owner), bundleId)
}

// pageActiveBundles walks the paginated active-bundle listing to its end. A
// page that reports more rows without a cursor to reach them is refused rather
// than read as the end of the list.
func pageActiveBundles(ctx context.Context, read func(context.Context) (*ExecuteResult, error)) ([]AuthoringBundleRow, error) {
	var out []AuthoringBundleRow
	cursor := ""
	for page := 0; page < maxActiveBundlePages; page++ {
		res, err := read(ContextWithCursor(ctx, cursor))
		if err != nil {
			return nil, err
		}
		rows, err := activeBundleRows(res)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		next, more := "", false
		if res != nil && res.Meta != nil {
			next, more = strings.TrimSpace(res.Meta.Cursor), res.Meta.HasMore
		}
		if next == "" {
			if more {
				return nil, fmt.Errorf("authoring: the active-bundle listing has more rows and no cursor to reach them")
			}
			return out, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("authoring: the active-bundle listing's cursor did not advance")
		}
		cursor = next
	}
	return nil, fmt.Errorf("authoring: the active-bundle listing ran past %d pages", maxActiveBundlePages)
}

// armReadContext is the envelope a bundle read runs under: the bundle author's
// writer envelope, as engineRearmStore loads constructs -- the owner-scoped
// queries filter on actor.userId, so nothing is read through an admin bypass --
// and fresh.
func armReadContext(ctx context.Context, owner string) context.Context {
	return ContextWithFreshRead(auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: owner,
		Role:   auth.RoleWriter,
	}))
}

package memql

// authoring_arm_propagate.go -- arming and disarming an authored bundle on
// EVERY replica (memql#6022, F1).
//
// ActivateApprovedBundle registers a bundle's constructs into the authored
// runtime it is handed -- the registry and scheduler of the node that handled
// the activation -- and RetireActiveBundle tears them down there. Until this
// file nothing told any other replica: each learned of an activation, and of a
// retire, only by restarting (RearmActiveBundles arms what is active at boot).
// The authored owner gate runs an owner's automations on exactly ONE node of
// the mesh, so an email rule activated anywhere but that node did not fire,
// and a rule retired on one node kept firing on every replica that had armed
// it at boot.
//
// Modeled on the promote/demote broadcasts (authoring_promote_propagate.go),
// on channels of their own:
//
//   - a successful activation emits authoring.arm.<bundleId>, a successful
//     retire authoring.disarm.<bundleId> (publishAuthoringArm /
//     publishAuthoringDisarm), carrying only the bundle id and its owner;
//   - two broadcast routing rules (component/node/routing.go) forward both to
//     every node, and only this subscriber consumes them;
//   - every node reads the bundle from the SHARED database under the author's
//     envelope and arms or disarms it in its OWN authored runtime.
//
// Not authoring.promote: promote re-hydrates PLAIN constructs into the shared
// registry every caller resolves through; arming registers owner-scoped
// constructs -- automations among them -- into each node's authored registry
// and scheduler. Arming leaves a node in exactly the state the boot re-arm
// would.
//
// What keeps it correct with two replicas of every node:
//
//   - The origin skips its own copy. It armed (or disarmed) in the call that
//     published, and an event that arrives with no origin node is its own.
//   - The database decides, not the event. A bundle is armed only while its
//     row reads active and disarmed only once it no longer does, and both
//     reads are fresh -- never this node's result cache, which hears about the
//     origin's write by a broadcast of its own that may not have landed yet.
//     A late or reordered broadcast therefore applies the CURRENT state, and a
//     forged one changes nothing the database does not already say.
//   - Versions only move forward. A construct this node already holds at the
//     same or a newer version is not registered again, and a disarm takes down
//     only the version the retired bundle registered -- never a newer one an
//     edit has armed since.
//   - One broadcast at a time. The bus delivers every event on its own
//     goroutine, so an arm and the disarm right behind it can arrive in either
//     order; applying them under one lock, each reading the database inside
//     it, is what makes the last one applied agree with the last write.

import (
	"context"
	"fmt"
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
	// arming lock, so a stalled database must not hold every later broadcast
	// behind it indefinitely.
	armApplyTimeout = 30 * time.Second
)

// authoredArming is a node's half of the arm/disarm broadcast: the deps a
// peer's activation is armed into, and the lock that applies one broadcast at
// a time.
type authoredArming struct {
	deps     atomic.Pointer[func() AuthoredRuntimeDeps]
	applying sync.Mutex
}

// SetAuthoredRuntimeDeps hands the engine this node's authored runtime -- the
// registry and the scheduler register / deregister hooks -- so the arm
// subscriber can arm a bundle activated on another node into it. app/ calls it
// once the registry and scheduler exist (wireAuthoredRuntime); until then, and
// on a binary that never builds the authored runtime, arm and disarm
// broadcasts are skipped here and the boot re-arm is what arms the node.
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

// StartAuthoringArmSubscriber subscribes this node to the authoring.arm.* and
// authoring.disarm.* broadcasts. On a peer's broadcast it reads the bundle from
// the shared database and arms or disarms it in this node's authored runtime.
// Scoped to ctx: cancelling ctx (engine stop) unsubscribes. The engine
// bootstrap calls it once at startup, beside the promote and demote
// subscribers; exported so a cross-node harness can stand two engines up the
// same way.
func (e *MemQLEngine) StartAuthoringArmSubscriber(ctx context.Context) {
	e.startAuthoringArmSubscriberWithStore(ctx, &engineArmStore{engine: e})
}

// startAuthoringArmSubscriberWithStore is the store-injectable core of
// StartAuthoringArmSubscriber, split out so a test can drive two engines over
// one fake store standing in for the shared database.
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
		logger.Warn("authoring "+verb+" propagation failed; this node arms the bundle's current state at its next boot",
			"bundleId", bundleId, "owner", owner, "originNode", ev.OriginNodeId,
			"registered", out.Registered, "retired", out.Retired, "error", err)
		return
	}
	logger.Info("authoring "+verb+" propagation applied",
		"bundleId", bundleId, "owner", owner, "originNode", ev.OriginNodeId,
		"registered", out.Registered, "alreadyHeld", out.AlreadyHeld, "retired", out.Retired, "ignored", out.Ignored)
}

// armOutcome is what one broadcast changed on this node, for its log line.
type armOutcome struct {
	// Registered counts constructs armed into this node's runtime.
	Registered int
	// AlreadyHeld counts constructs this node already held at the bundle's
	// version or a newer one.
	AlreadyHeld int
	// Retired counts constructs taken out: a disarmed bundle's, or the
	// leftovers of the version an armed bundle supersedes.
	Retired int
	// Ignored says why the broadcast changed nothing, when the database says
	// there is nothing to change.
	Ignored string
}

// armBundleWithStore arms one bundle into deps the way a fresh activation does,
// from the bundle's rows as the database holds them now. Only an active bundle
// arms. Each construct registers unless the registry already holds it at the
// bundle's version or newer; then the leftovers of the version this one
// supersedes -- constructs it no longer registers -- are disarmed, as the
// origin's activation retired them.
func armBundleWithStore(ctx context.Context, store armStore, owner, bundleId string, deps AuthoredRuntimeDeps) (armOutcome, error) {
	var out armOutcome
	bundle, err := store.LoadBundleForOwner(ctx, owner, bundleId)
	if err != nil {
		return out, fmt.Errorf("authoring: load bundle %q to arm: %w", bundleId, err)
	}
	if bundle.Id == "" {
		out.Ignored = "the bundle is not readable as its owner"
		return out, nil
	}
	if bundle.Status != BundleActive {
		out.Ignored = fmt.Sprintf("the bundle is %s, not active", bundle.Status)
		return out, nil
	}
	constructs, err := store.LoadConstructsForOwner(ctx, owner, bundleId)
	if err != nil {
		return out, fmt.Errorf("authoring: load constructs to arm %q: %w", bundleId, err)
	}

	// The same planning step the boot re-arm takes (rearmOneBundle): an active
	// bundle is presented as dryRunPassed to the pure planner, so the runtime
	// state matches a fresh activation. The persisted row is not touched.
	planning := bundle
	planning.Status = BundleDryRunPassed
	plan, err := PlanBundleActivation(planning, constructs)
	if err != nil {
		return out, fmt.Errorf("authoring: plan arm of %q: %w", bundleId, err)
	}

	for _, c := range plan.Register {
		if held, ok := deps.Registry.Lookup(c.OwnerUserId, c.Kind, c.Name); ok && held.Version >= c.Version {
			out.AlreadyHeld++
			continue
		}
		// One at a time through the shared registration step, so the count in
		// the log is what actually registered when a later construct fails.
		if err := registerBundleConstructs([]*AuthoredConstruct{c}, deps); err != nil {
			return out, err
		}
		out.Registered++
	}

	if plan.SupersedesBundleId == "" {
		return out, nil
	}
	prior, err := store.LoadBundleForOwner(ctx, owner, plan.SupersedesBundleId)
	if err != nil {
		return out, fmt.Errorf("authoring: load superseded bundle %q: %w", plan.SupersedesBundleId, err)
	}
	// A superseded bundle the database still calls active was not retired by
	// the origin either (its retire failed and was logged there), so it stays
	// armed here too.
	if prior.Id == "" || prior.Status == BundleActive {
		return out, nil
	}
	priorConstructs, err := store.LoadConstructsForOwner(ctx, owner, plan.SupersedesBundleId)
	if err != nil {
		return out, fmt.Errorf("authoring: load constructs of superseded bundle %q: %w", plan.SupersedesBundleId, err)
	}
	out.Retired = disarmConstructs(priorConstructs, prior.Version, plan.registeredKeys(), deps)
	return out, nil
}

// disarmBundleWithStore takes a retired bundle out of deps. A bundle the
// database still calls active is left alone: the broadcast is late (the bundle
// was re-armed since) or not the engine's.
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
	out.Retired = disarmConstructs(constructs, bundle.Version, nil, deps)
	return out, nil
}

// disarmConstructs takes a bundle's constructs out of deps -- the scheduler
// entry for an automation, and the registry entry -- where the registry still
// holds them at that bundle's version. A newer version armed since shares the
// kind and name and must survive; constructs whose key is in keep are skipped
// outright. Returns how many it took out.
func disarmConstructs(constructs []AuthoringConstructRow, version int, keep map[string]bool, deps AuthoredRuntimeDeps) int {
	retired := 0
	for _, c := range constructs {
		if keep[constructKey(c.Kind, c.Name)] {
			continue
		}
		held, ok := deps.Registry.Lookup(c.OwnerUserId, c.Kind, c.Name)
		if !ok || held.Version != version {
			continue
		}
		if c.Kind == "automation" && deps.DeregisterHook != nil {
			deps.DeregisterHook(c.OwnerUserId, c.Name)
		}
		if err := deps.Registry.Retire(c.OwnerUserId, c.Kind, c.Name); err == nil {
			retired++
		}
	}
	return retired
}

// publishAuthoringArm emits the authoring.arm.<bundleId> broadcast for a
// bundle this node just activated. ONLY the authoring-arm subscriber consumes
// the channel, and a single broadcast routing rule (authoring.arm.*) forwards
// it to every node. Best-effort: an engine without a bus (a non-mesh or test
// engine) arms only itself, and every other node still arms the bundle at its
// next boot.
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

// armStore is the narrow graph surface a broadcast arm or disarm reads: one
// bundle and its constructs, both under the bundle AUTHOR's envelope.
// *MemQLEngine satisfies it via engineArmStore; tests fake it.
type armStore interface {
	LoadBundleForOwner(ctx context.Context, owner, bundleId string) (AuthoringBundleRow, error)
	LoadConstructsForOwner(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error)
}

// engineArmStore is the production armStore over a live engine: the queries the
// activation reads (authoringBundleById, authoringConstructsForBundle), run
// under armReadContext.
type engineArmStore struct {
	engine *MemQLEngine
}

func (s *engineArmStore) LoadBundleForOwner(ctx context.Context, owner, bundleId string) (AuthoringBundleRow, error) {
	return (&engineActivationStore{engine: s.engine}).LoadBundle(armReadContext(ctx, owner), bundleId)
}

func (s *engineArmStore) LoadConstructsForOwner(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	return (&engineActivationStore{engine: s.engine}).LoadConstructs(armReadContext(ctx, owner), bundleId)
}

// armReadContext is the envelope a broadcast's reads run under: the bundle
// author's writer envelope, as engineRearmStore loads constructs -- the
// owner-scoped queries filter on actor.userId, so nothing is read through an
// admin bypass -- and FRESH, because the read decides what this node arms and
// the write it follows reaches this node's result cache only by an
// asynchronous broadcast of its own.
func armReadContext(ctx context.Context, owner string) context.Context {
	return ContextWithFreshRead(auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: owner,
		Role:   auth.RoleWriter,
	}))
}

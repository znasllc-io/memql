package memql

// authoring_breaker_pause.go -- a tripped authored automation stops on EVERY
// replica, and stays stopped across a restart (memql#6022, F2).
//
// The per-automation circuit breaker (component/automations, authored_breaker.go)
// trips after consecutive failed runs. It used to pause the automation in the
// tripping node's scheduler and registry and nowhere else, which was enough
// while the owner gate ran an owner's automations on one node. It is not now:
// an event rule fires on whichever replica its triggering write landed on, so
// the others kept firing it -- and the pause lived only in that node's memory,
// so a reboot armed it again, and so did that node's own reconcile pass, which
// reads the bundle as active.
//
// So a trip makes the change the database decides by:
//
//  1. It records the pause on the bundle row -- setAuthoringBundleStatus,
//     status paused, with why -- as the bundle's owner. `paused` is the
//     bundle lifecycle's documented breaker state, and it is not active, so
//     the boot re-arm and every reconcile pass leave the bundle out.
//  2. It tells the pause listeners registered on this engine -- the node
//     that writes the pause is the only one that does, so whatever a listener
//     records (emailrules marks the rule paused) is written once.
//  3. It takes the bundle out of this node exactly as a disarm broadcast
//     would, and
//  4. broadcasts authoring.disarm.<bundleId>, so every other node does the
//     same within seconds rather than at its next reconcile pass.
//
// THE TRIP PAUSES THE VERSION THAT FAILED, OR NOTHING. It runs after the
// failing run, off its goroutine, under the arming lock -- so by the time it
// runs, a fix may have been armed under the same name. The trip carries the
// bundle and version the failing runs were armed from, and it is skipped when
// this node holds anything else under the name, or when the breaker is no
// longer open (re-activation resets it).
//
// A bundle that is already not active (retired or superseded elsewhere, and
// this node had not heard) gets no pause write -- a retired bundle flipped to
// paused would read as resumable -- but is still taken out and broadcast. The
// bundle is read right before the write, fresh, under the lock; a retire
// written elsewhere in the instant between that read and the write is the one
// gap left, since the write cannot be made conditional.
//
// A pause that cannot be recorded leaves the automation stopped HERE --
// out of the scheduler, paused in the registry, which this node's reconcile
// pass leaves alone because it finds the construct held -- and broadcasts
// nothing, since every other node would read the bundle as active and ignore
// the disarm. The error says so; the other nodes' own breakers still stand.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// pauseStore is the arming store plus the one write a trip makes.
type pauseStore interface {
	armStore
	// PauseBundleForOwner sets the bundle row's status to paused with the
	// reason, written as the bundle's owner.
	PauseBundleForOwner(ctx context.Context, owner, bundleId, reason string) error
}

// AuthoredTrip is one breaker trip, as PauseTrippedAutomation applies it.
type AuthoredTrip struct {
	// Owner and Automation are the (owner, name) the breaker keyed on.
	Owner      string
	Automation string
	// BundleId and Version are the construct the failing runs were armed
	// from; the pause applies only while this node still holds exactly that.
	BundleId string
	Version  int
	// Reason is the sentence the bundle row carries as its failureReason.
	Reason string
	// StillTripped reports whether the breaker is still open when the pause
	// is applied; false skips it. nil = the trip is applied as given.
	StillTripped func() bool
}

// AuthoredPauseListener is told, by the node that recorded it, that a
// breaker's pause was written onto a bundle: the bundle's owner and its id.
// It runs once per recorded pause, under the arming lock, on a context the
// caller cannot cancel; an error it returns is logged and changes nothing.
type AuthoredPauseListener func(ctx context.Context, owner, bundleId string) error

// SetAuthoredPauseListener registers a listener under name, replacing one
// already registered under it; nil removes it. emailrules registers the
// listener that marks an email rule paused (component/emailrules).
func (e *MemQLEngine) SetAuthoredPauseListener(name string, listener AuthoredPauseListener) {
	if e == nil {
		return
	}
	e.arming.listenMu.Lock()
	defer e.arming.listenMu.Unlock()
	if listener == nil {
		delete(e.arming.pauseListeners, name)
		return
	}
	if e.arming.pauseListeners == nil {
		e.arming.pauseListeners = map[string]AuthoredPauseListener{}
	}
	e.arming.pauseListeners[name] = listener
}

// notifyPaused tells every registered listener about a pause this node wrote.
func (e *MemQLEngine) notifyPaused(ctx context.Context, owner, bundleId string) {
	e.arming.listenMu.Lock()
	listeners := make(map[string]AuthoredPauseListener, len(e.arming.pauseListeners))
	for name, l := range e.arming.pauseListeners {
		listeners[name] = l
	}
	e.arming.listenMu.Unlock()
	for name, l := range listeners {
		if err := l(ctx, owner, bundleId); err != nil {
			if logger := e.safeLogger(); logger != nil {
				logger.Warn("authored bundle paused, but a pause listener failed",
					"listener", name, "bundleId", bundleId, "owner", owner, "error", err)
			}
		}
	}
}

// PauseTrippedAutomation is what a breaker trip does to an authored
// automation: stop the version that failed on this node, record the pause on
// its bundle row, tell the pause listeners, and broadcast the disarm so every
// other node stops it too. deps is this node's authored runtime. Runs under a
// context the caller cannot cancel halfway: the trip is the system's own
// decision.
func (e *MemQLEngine) PauseTrippedAutomation(ctx context.Context, deps AuthoredRuntimeDeps, trip AuthoredTrip) error {
	return e.pauseTrippedAutomationWithStore(ctx, &engineArmStore{engine: e}, deps, trip)
}

// pauseTrippedAutomationWithStore is PauseTrippedAutomation over an injected
// store, under the arming lock every change to this node's runtime takes.
func (e *MemQLEngine) pauseTrippedAutomationWithStore(ctx context.Context, store pauseStore, deps AuthoredRuntimeDeps, trip AuthoredTrip) error {
	if deps.Registry == nil {
		return fmt.Errorf("authoring: a tripped automation cannot be paused without an authored runtime registry")
	}
	owner, name := trip.Owner, trip.Automation
	e.arming.applying.Lock()
	defer e.arming.applying.Unlock()

	// Only the version that failed, and only while its breaker is open: a fix
	// armed in the meantime under the same name reset the breaker and is
	// what this node holds now.
	held, ok := deps.Registry.Lookup(owner, "automation", name)
	if !ok {
		return nil
	}
	if held.BundleId != trip.BundleId || held.Version != trip.Version {
		return nil
	}
	if trip.StillTripped != nil && !trip.StillTripped() {
		return nil
	}

	// Stopped here first: out of the scheduler, and held paused in the
	// registry until the database agrees.
	if deps.DeregisterHook != nil {
		deps.DeregisterHook(owner, name)
	}
	_ = deps.Registry.Pause(owner, "automation", name)
	bundleId := strings.TrimSpace(held.BundleId)
	if bundleId == "" {
		return nil // not armed from a bundle: there is no row to record the pause on
	}

	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), armApplyTimeout)
	defer cancel()
	bundle, err := store.LoadBundleForOwner(work, owner, bundleId)
	if err != nil {
		return fmt.Errorf("authoring: read bundle %q to pause %s: %w; it is paused on this node only", bundleId, name, err)
	}
	if bundle.Id == "" {
		return fmt.Errorf("authoring: bundle %q is not readable as its owner %q; %s is paused on this node only", bundleId, owner, name)
	}
	if bundle.Status == BundleActive {
		if err := store.PauseBundleForOwner(work, owner, bundle.Id, trip.Reason); err != nil {
			return fmt.Errorf("authoring: record the pause of %s on bundle %q: %w; it is paused on this node only", name, bundle.Id, err)
		}
		e.notifyPaused(work, owner, bundle.Id)
	}
	out, err := disarmBundleWithStore(work, store, owner, bundle.Id, deps)
	if err != nil {
		return fmt.Errorf("authoring: take paused bundle %q out of this node: %w", bundle.Id, err)
	}
	if out.Ignored != "" {
		return fmt.Errorf("authoring: bundle %q still reads active after its pause was written (%s); %s is paused on this node only",
			bundle.Id, out.Ignored, name)
	}
	e.publishAuthoringDisarm(bundle.Id, owner)
	return nil
}

// PauseBundleForOwner records a breaker's pause on the bundle row, as the
// bundle's owner: every authoring mutation stamps ownerUserId from the actor,
// so writing it as anyone else would hand the row to them. ContextWithUserActor
// stamps all three actor surfaces -- a write reads its actor off the token, a
// stamp off the access context.
func (s *engineArmStore) PauseBundleForOwner(ctx context.Context, owner, bundleId, reason string) error {
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("authoring: pause bundle %q: the bundle names no owner to write it as", bundleId)
	}
	_, err := s.engine.Execute(auth.ContextWithUserActor(ctx, owner), mutationCall("setAuthoringBundleStatus",
		[2]string{"bundleId", bundleId},
		[2]string{"status", string(BundlePaused)},
		[2]string{"failureReason", reason},
	))
	return err
}

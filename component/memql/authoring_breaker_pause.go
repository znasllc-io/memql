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
//     status paused, with why -- as the bundle's owner, under internal origin
//     (the mutation is @serverOnly). `paused` is the bundle lifecycle's
//     documented breaker state, and it is not active, so the boot re-arm and
//     every reconcile pass leave the bundle out.
//  2. It takes the bundle out of this node exactly as a disarm broadcast
//     would, and
//  3. broadcasts authoring.disarm.<bundleId>, so every other node does the
//     same within seconds rather than at its next reconcile pass.
//  4. Only then does it tell the pause listeners registered on this engine,
//     each on a bound of its own: a listener that stalls must not delay the
//     stop on every replica, nor spend the time the broadcast needs. The node
//     that writes the pause is the only one that tells them, so whatever a
//     listener records (emailrules marks the rule paused) is written once.
//
// THE TRIP PAUSES THE VERSION THAT FAILED, OR NOTHING. It runs after the
// failing run, off its goroutine, under the arming lock -- so by the time it
// runs, a fix may have been armed under the same name. The trip carries the
// bundle and version the failing runs were armed from, and it is skipped when
// this node holds anything else under the name, or when the breaker is no
// longer open (re-activation resets it).
//
// THE PAUSE IS WRITTEN AGAINST THE ROW IT READ. The bundle is read fresh under
// the lock, and the write is fenced on the version that read returned
// (ContextWithRowVersionFence): a retire written elsewhere in between -- the
// person turned the rule off -- makes the write fail with
// ErrRowVersionChanged instead of overwriting it with paused, which would read
// as resumable. A row that changed is treated as no longer active: no pause is
// recorded, and the bundle is taken out and the disarm broadcast, as for a
// bundle that read retired.
//
// A pause that cannot be recorded leaves the automation stopped HERE --
// out of the scheduler, paused in the registry, which this node's reconcile
// pass leaves alone because it finds the construct held -- and broadcasts
// nothing, since every other node would read the bundle as active and ignore
// the disarm. The error says so; the other nodes' own breakers still stand.

import (
	"context"
	"errors"
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
// It runs once per recorded pause, after the disarm went out to every replica,
// under the arming lock, on a bounded context of its own the caller cannot
// cancel; an error it returns is logged and changes nothing. By the time it
// runs the bundle may have been written again, so a listener reads what it
// acts on fresh.
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

// AuthoredTripOutcome is what applying a breaker trip did.
type AuthoredTripOutcome string

const (
	// TripPaused: the pause was recorded on the bundle row and the listeners
	// were told; the bundle was taken out of this node and the disarm
	// broadcast unless an error says otherwise.
	TripPaused AuthoredTripOutcome = "paused"
	// TripBundleNotActive: the bundle no longer read active, or changed
	// before the pause could be written, so nothing was recorded; the bundle
	// was taken out of this node and the disarm broadcast.
	TripBundleNotActive AuthoredTripOutcome = "bundleNotActive"
	// TripStoppedHere: the automation is stopped on this node only -- it was
	// not armed from a bundle, or (with an error) the pause could not be
	// recorded.
	TripStoppedHere AuthoredTripOutcome = "stoppedHere"
	// TripVersionNotHeld: this node no longer holds the version that failed
	// (a fix was armed, or it was taken out); nothing was done.
	TripVersionNotHeld AuthoredTripOutcome = "versionNotHeld"
	// TripBreakerReset: the breaker was reset -- the automation was turned on
	// again -- before the trip applied; nothing was done.
	TripBreakerReset AuthoredTripOutcome = "breakerReset"
)

// PauseTrippedAutomation is what a breaker trip does to an authored
// automation: stop the version that failed on this node, record the pause on
// its bundle row, broadcast the disarm so every other node stops it too, and
// tell the pause listeners. deps is this node's authored runtime. Runs under a
// context the caller cannot cancel halfway: the trip is the system's own
// decision. The outcome says which of those happened.
func (e *MemQLEngine) PauseTrippedAutomation(ctx context.Context, deps AuthoredRuntimeDeps, trip AuthoredTrip) (AuthoredTripOutcome, error) {
	return e.pauseTrippedAutomationWithStore(ctx, &engineArmStore{engine: e}, deps, trip)
}

// pauseTrippedAutomationWithStore is PauseTrippedAutomation over an injected
// store, under the arming lock every change to this node's runtime takes.
func (e *MemQLEngine) pauseTrippedAutomationWithStore(ctx context.Context, store pauseStore, deps AuthoredRuntimeDeps, trip AuthoredTrip) (AuthoredTripOutcome, error) {
	if deps.Registry == nil {
		return TripStoppedHere, fmt.Errorf("authoring: a tripped automation cannot be paused without an authored runtime registry")
	}
	owner, name := trip.Owner, trip.Automation
	e.arming.applying.Lock()
	defer e.arming.applying.Unlock()

	// Only the version that failed, and only while its breaker is open: a fix
	// armed in the meantime under the same name reset the breaker and is
	// what this node holds now.
	held, ok := deps.Registry.Lookup(owner, "automation", name)
	if !ok {
		return TripVersionNotHeld, nil
	}
	if held.BundleId != trip.BundleId || held.Version != trip.Version {
		return TripVersionNotHeld, nil
	}
	if trip.StillTripped != nil && !trip.StillTripped() {
		return TripBreakerReset, nil
	}

	// Stopped here first: out of the scheduler, and held paused in the
	// registry until the database agrees.
	if deps.DeregisterHook != nil {
		deps.DeregisterHook(owner, name)
	}
	_ = deps.Registry.Pause(owner, "automation", name)
	bundleId := strings.TrimSpace(held.BundleId)
	if bundleId == "" {
		return TripStoppedHere, nil // not armed from a bundle: there is no row to record the pause on
	}

	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), armApplyTimeout)
	defer cancel()
	bundle, err := store.LoadBundleForOwner(work, owner, bundleId)
	if err != nil {
		return TripStoppedHere, fmt.Errorf("authoring: read bundle %q to pause %s: %w; it is paused on this node only", bundleId, name, err)
	}
	if bundle.Id == "" {
		return TripStoppedHere, fmt.Errorf("authoring: bundle %q is not readable as its owner %q; %s is paused on this node only", bundleId, owner, name)
	}
	outcome := TripBundleNotActive
	if bundle.Status == BundleActive {
		switch err := store.PauseBundleForOwner(fencedOnBundle(work, bundle), owner, bundle.Id, trip.Reason); {
		case err == nil:
			outcome = TripPaused
		case errors.Is(err, ErrRowVersionChanged):
			// Written elsewhere since the read -- retired, paused by another
			// node's breaker, re-activated. Not this trip's to overwrite.
		default:
			return TripStoppedHere, fmt.Errorf("authoring: record the pause of %s on bundle %q: %w; it is paused on this node only", name, bundle.Id, err)
		}
	}
	out, err := disarmBundleWithStore(work, store, owner, bundle.Id, deps)
	if err == nil && out.Ignored == "" {
		e.publishAuthoringDisarm(bundle.Id, owner)
	}
	// The listeners last, once every replica has been told to stop -- and
	// whatever the local take-out did, since the pause IS recorded.
	if outcome == TripPaused {
		tell, cancelTell := context.WithTimeout(context.WithoutCancel(ctx), armApplyTimeout)
		e.notifyPaused(tell, owner, bundle.Id)
		cancelTell()
	}
	if err == nil && out.Ignored != "" {
		err = fmt.Errorf("bundle %q reads active again (%s)", bundle.Id, out.Ignored)
	}
	if err != nil {
		// A recorded pause stays the outcome: every replica's reconcile pass
		// reads the bundle paused even though this node could not finish.
		if outcome != TripPaused {
			outcome = TripStoppedHere
		}
		return outcome, fmt.Errorf("authoring: take bundle %q out of this node after the trip of %s: %w; it is paused on this node, and the disarm was not broadcast",
			bundle.Id, name, err)
	}
	return outcome, nil
}

// fencedOnBundle makes a write conditional on the version of the bundle row
// that was read. A read that carried no version leaves the write as it was:
// a zero witness would require the row to be absent, which refuses every
// write to a bundle that exists.
func fencedOnBundle(ctx context.Context, bundle AuthoringBundleRow) context.Context {
	if bundle.CreatedAt.IsZero() {
		return ctx
	}
	return ContextWithRowVersionFence(ctx, conceptAuthoringBundle, bundle.Id, bundle.CreatedAt)
}

// PauseBundleForOwner records a breaker's pause on the bundle row, as the
// bundle's owner: every authoring mutation stamps ownerUserId from the actor,
// so writing it as anyone else would hand the row to them. ContextWithUserActor
// stamps all three actor surfaces -- a write reads its actor off the token, a
// stamp off the access context. setAuthoringBundleStatus is @serverOnly
// (memql#6022), so the one write stamps internal origin, inline. A fence on
// ctx (fencedOnBundle) carries through: the write lands only on the version
// the caller read.
func (s *engineArmStore) PauseBundleForOwner(ctx context.Context, owner, bundleId, reason string) error {
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("authoring: pause bundle %q: the bundle names no owner to write it as", bundleId)
	}
	_, err := s.engine.Execute(auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)), mutationCall("setAuthoringBundleStatus",
		[2]string{"bundleId", bundleId},
		[2]string{"status", string(BundlePaused)},
		[2]string{"failureReason", reason},
	))
	return err
}

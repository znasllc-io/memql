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
//  2. It takes the bundle out of this node exactly as a disarm broadcast
//     would, and
//  3. broadcasts authoring.disarm.<bundleId>, so every other node does the
//     same within seconds rather than at its next reconcile pass.
//
// A bundle that is already not active (retired or superseded elsewhere, and
// this node had not heard) gets no pause write -- a retired bundle flipped to
// paused would read as resumable -- but is still taken out and broadcast.
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

// PauseTrippedAutomation is what a breaker trip does to an authored
// automation: stop it on this node, record the pause on its bundle row, and
// broadcast the disarm so every other node stops it too. deps is this node's
// authored runtime; reason is the sentence the bundle row carries as its
// failureReason. Runs under a context the caller cannot cancel halfway: the
// trip is the system's own decision.
func (e *MemQLEngine) PauseTrippedAutomation(ctx context.Context, deps AuthoredRuntimeDeps, owner, name, reason string) error {
	return e.pauseTrippedAutomationWithStore(ctx, &engineArmStore{engine: e}, deps, owner, name, reason)
}

// pauseTrippedAutomationWithStore is PauseTrippedAutomation over an injected
// store, under the arming lock every change to this node's runtime takes.
func (e *MemQLEngine) pauseTrippedAutomationWithStore(ctx context.Context, store pauseStore, deps AuthoredRuntimeDeps, owner, name, reason string) error {
	if deps.Registry == nil {
		return fmt.Errorf("authoring: a tripped automation cannot be paused without an authored runtime registry")
	}
	e.arming.applying.Lock()
	defer e.arming.applying.Unlock()

	// Stopped here first, before anything is read: out of the scheduler, and
	// held paused in the registry until the database agrees.
	if deps.DeregisterHook != nil {
		deps.DeregisterHook(owner, name)
	}
	held, ok := deps.Registry.Lookup(owner, "automation", name)
	if !ok {
		return nil
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
		if err := store.PauseBundleForOwner(work, owner, bundle.Id, reason); err != nil {
			return fmt.Errorf("authoring: record the pause of %s on bundle %q: %w; it is paused on this node only", name, bundle.Id, err)
		}
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

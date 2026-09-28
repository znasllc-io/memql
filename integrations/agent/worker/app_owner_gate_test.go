//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// sweepActor is a scheduled automation's actor (component/automations'
// system actor): a non-empty UserId that names no person.
const sweepActor = "system:automation:workerAppSessionStaleSweep"

// AN APP SESSION FOR NOBODY NEVER REACHES A MACHINE, on any of the three ways
// in: the chat / structured door, the session door (design D7) and the
// executor a routed Task runs through. The engine's router already passes the
// app door for such a call (component/memql AppNoOwnerReason); this is the
// agent refusing on its own, so a caller that reaches it some other way meets
// the same answer.
//
// The fixture's machine is REGISTERED TO THE SYSTEM ID and online on this
// replica, so "no machine" cannot be what stops it: only the owner rule can.
func TestAnAppSessionForNobodyNeverReachesAMachine(t *testing.T) {
	doors := []struct {
		name string
		run  func(f *appGateFixture, writes *journalWrites) error
	}{
		{"the chat door", func(f *appGateFixture, _ *journalWrites) error {
			req := chatTurn()
			req.ActingUserId = sweepActor
			_, err := f.inference(nil).Call(context.Background(), req)
			return err
		}},
		{"the session door", func(f *appGateFixture, writes *journalWrites) error {
			h := sessionHandover(memqlengine.AppDoorPin{})
			h.ActingUserId = sweepActor
			_, err := f.delegate(t, writes).RunStep(context.Background(), h)
			return err
		}},
		{"a routed task", func(f *appGateFixture, _ *journalWrites) error {
			step := handedOverStep()
			step.OwnerUserId = sweepActor
			_, err := f.executor(t, nil).Run(context.Background(), step, nil)
			return err
		}},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			f := newAppGateFixture(t, sweepActor)
			writes := &journalWrites{}
			err := door.run(f, writes)
			if err == nil || !strings.Contains(err.Error(), memqlengine.AppNoOwnerReason) {
				t.Fatalf("err = %v, want a refusal naming %q", err, memqlengine.AppNoOwnerReason)
			}
			if got := f.capture.started(); len(got) != 0 {
				t.Fatalf("a session was started on the machine for nobody: %+v", got)
			}
			if writes.count() != 0 {
				t.Fatalf("a child run was opened for nobody (%d journal write(s))", writes.count())
			}
		})
	}
}

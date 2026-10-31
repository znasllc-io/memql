//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// signInHop is the hop as a sign-in rides it: the call on a node with no stream, the
// machine's stream on the holder.
func signInHop(t *testing.T) (*appHop, *[]workerservice.AppSignInCommand, *AppSignInRelay) {
	t.Helper()
	h := newAppHop(t)
	var told []workerservice.AppSignInCommand
	h.registry.WorkerById(hopMachine).SetAppSignInFunc(func(_ context.Context, cmd workerservice.AppSignInCommand) error {
		told = append(told, cmd)
		return nil
	})
	return h, &told, NewAppSignInRelay(h.link.router, nil, testLogger())
}

func signInCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), hopOwner, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithTimeout(ctx, 10*time.Second)
}

// The person's stream ends on a bff with no registry, and the machine's stream on an
// agent. What they did must cross, with what they pasted intact.
func TestAppSignInCrossesTheHop(t *testing.T) {
	h, told, relay := signInHop(t)
	ctx, cancel := signInCtx(t)
	defer cancel()
	for _, action := range []memqlengine.AppSignInAction{
		{Action: memqlengine.AppSignInStart, RegistrationId: hopMachine, OwnerUserId: hopOwner, SignInId: "s1", App: "codex"},
		{Action: memqlengine.AppSignInCode, RegistrationId: hopMachine, OwnerUserId: hopOwner, SignInId: "s1", Code: "pasted-code"},
		{Action: memqlengine.AppSignInCancel, RegistrationId: hopMachine, OwnerUserId: hopOwner, SignInId: "s1"},
	} {
		if err := relay.RelayAppSignIn(ctx, hopHolder, action); err != nil {
			t.Fatalf("%s: %v", action.Action, err)
		}
	}
	if len(*told) != 3 || (*told)[0].App != "codex" || (*told)[1].Code != "pasted-code" || (*told)[2].Action != workerservice.AppSignInActionCancel {
		t.Fatalf("got %+v", *told)
	}
	_ = h
}

// Only the machine's owner may start a sign-in, and the holder decides that from the
// verified authority, whatever the envelope says.
func TestAppSignInRefusesAnyoneButTheMachinesOwner(t *testing.T) {
	_, told, relay := signInHop(t)
	stranger, err := auth.ContextWithPersistedOwner(context.Background(), "v1:identity:user:stranger", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(stranger, 10*time.Second)
	defer cancel()
	// Claiming to be the owner in the envelope changes nothing.
	err = relay.RelayAppSignIn(ctx, hopHolder, memqlengine.AppSignInAction{Action: memqlengine.AppSignInStart, RegistrationId: hopMachine, OwnerUserId: hopOwner, SignInId: "s1", App: "codex"})
	if err == nil {
		t.Fatal("a stranger started a sign-in on somebody's machine")
	}
	if len(*told) != 0 {
		t.Fatalf("the machine was told: %+v", *told)
	}
}

// A code must not come back in anything a person or a log can read.
func TestAppSignInRefusalDoesNotEchoACode(t *testing.T) {
	h, _, relay := signInHop(t)
	h.registry.WorkerById(hopMachine).SetAppSignInFunc(func(context.Context, workerservice.AppSignInCommand) error {
		return workerservice.ErrAppSignInUnavailable
	})
	ctx, cancel := signInCtx(t)
	defer cancel()
	err := relay.RelayAppSignIn(ctx, hopHolder, memqlengine.AppSignInAction{Action: memqlengine.AppSignInCode, RegistrationId: hopMachine, OwnerUserId: hopOwner, SignInId: "s1", Code: "pasted-secret-code"})
	if err == nil || strings.Contains(err.Error(), "pasted-secret-code") {
		t.Fatalf("got %v", err)
	}
}

// A machine this replica holds needs no hop, and the owner check still holds.
func TestAppSignInReachesAMachineThisReplicaHolds(t *testing.T) {
	h, told, _ := signInHop(t)
	relay := NewAppSignInRelay(nil, h.registry, testLogger())
	ctx, cancel := signInCtx(t)
	defer cancel()
	if err := relay.RelayAppSignIn(ctx, "", memqlengine.AppSignInAction{Action: memqlengine.AppSignInStart, RegistrationId: hopMachine, OwnerUserId: hopOwner, SignInId: "s1", App: "codex"}); err != nil {
		t.Fatal(err)
	}
	if len(*told) != 1 {
		t.Fatalf("got %+v", *told)
	}
	if err := relay.RelayAppSignIn(ctx, "", memqlengine.AppSignInAction{Action: memqlengine.AppSignInStart, RegistrationId: hopMachine, OwnerUserId: "v1:identity:user:stranger", SignInId: "s2", App: "codex"}); err == nil {
		t.Fatal("started on a machine the caller does not own")
	}
}

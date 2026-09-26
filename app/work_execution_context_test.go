package app

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/common"
)

func TestExecutionHopRestoresOwnerGoalAndReplay(t *testing.T) {
	j := &automations.RunJournal{RunId: "r", GoalId: "g", OwnerUserId: "u", Mode: "replay", ReplayPolicy: "strict", ForkedFromRunId: "source"}
	source := &automations.RunJournal{RunId: "source", GoalId: "g", OwnerUserId: "u", StepOrder: []string{"draft", "file"}}
	ctx, err := workExecutionContext(context.Background(), j, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := common.RunFromContext(ctx)
	if !ok || run.RunId != "r" || run.GoalId != "g" || run.OwnerUserId != "u" || run.Mode != "replay" || run.SourceRunId != "source" || run.SourceGoalId != "g" || len(run.StepOrder) != 2 {
		t.Fatalf("lost run context: %+v", run)
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.UserId != "u" || ac.Synthetic {
		t.Fatalf("lost owner: %+v", ac)
	}
}

func TestExecutionHopRefusesAnotherGoalsJournal(t *testing.T) {
	j := &automations.RunJournal{RunId: "r", GoalId: "g", OwnerUserId: "u", Mode: "replay", ForkedFromRunId: "source"}
	_, err := workExecutionContext(context.Background(), j, &automations.RunJournal{RunId: "source", GoalId: "other", OwnerUserId: "u"}, nil)
	if err == nil {
		t.Fatal("replay accepted a different goal's model journal")
	}
}

func TestExecutionHopBindsOnlyThePersistedOwnersForwardedAuthority(t *testing.T) {
	parent := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:maintenance", Role: auth.RoleOwner, Synthetic: true}))
	j := &automations.RunJournal{RunId: "run", GoalId: "goal", OwnerUserId: "v1:identity:user:alice", Mode: "live"}
	ctx, err := workExecutionContext(parent, j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		t.Fatal("execution cannot forward model calls: no forwarded authority for the persisted owner")
	}
	// Exercise the wire conversion and the same verifier as the worker
	// model-call receiver, rather than trusting the producer's local actor.
	wire := node.ForwardedAuthorityToProto(assertion, "agent-origin", "agent")
	verified, err := auth.VerifyForwardedAuthority(node.ForwardedAuthorityFromProto(wire), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if verified.UserId != "v1:identity:user:alice" || verified.Role != auth.RoleWriter || verified.Synthetic || verified.Unranked || verified.IsClusterOwner() {
		t.Fatalf("execution forwarded authority beyond the persisted owner: %+v", verified)
	}
	if auth.OriginFromContext(ctx) != auth.OriginClient {
		t.Fatal("borrowed owner inherited internal call origin")
	}
	claims := assertion.Principal().Claims
	if claims["sub"] != verified.UserId || claims["role"] != string(verified.Role) {
		t.Fatalf("forwarded attribution differs from authorization: %+v / %+v", claims, verified)
	}
}

func TestExecutionHopRefusesAMissingPersistedOwner(t *testing.T) {
	for _, owner := range []string{"", "  "} {
		_, err := workExecutionContext(context.Background(), &automations.RunJournal{RunId: "run", GoalId: "goal", OwnerUserId: owner}, nil, nil)
		if err == nil {
			t.Fatalf("execution accepted missing persisted owner %q", owner)
		}
	}
}

// The receiver starts with no browser session; the journal and current identity
// read are the only authority allowed to survive this hop.
func TestExecutionHopCarriesIntakeCeilingToModelReceiver(t *testing.T) {
	for _, ceiling := range []auth.Role{auth.RoleOwner, auth.RoleReader} {
		resolver := auth.NewIdentityResolver(auth.QueryRunnerFunc(func(context.Context, string) (any, error) {
			return map[string]any{"role": "owner"}, nil
		}), nil)
		journal := &automations.RunJournal{RunId: "r", GoalId: "g", OwnerUserId: "v1:identity:user:alice", ExecutionAuthority: map[string]any{"roleCeiling": string(ceiling), "credentialClass": auth.ForwardedClassUser}}
		ctx, err := workExecutionContext(context.Background(), journal, nil, resolver)
		if err != nil {
			t.Fatal(err)
		}
		assertion, _ := auth.ForwardedAuthorityFromContext(ctx)
		wire := node.ForwardedAuthorityToProto(assertion, "receiving-agent", "agent")
		received, err := auth.VerifyForwardedAuthority(node.ForwardedAuthorityFromProto(wire), time.Now())
		if err != nil || received.Role != ceiling || received.UserId != journal.OwnerUserId {
			t.Fatalf("lost or expanded authority: %+v %v", received, err)
		}
	}
}

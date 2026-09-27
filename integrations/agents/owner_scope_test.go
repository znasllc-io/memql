package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// owner_scope_test.go -- the owner an agents builtin acts for is its caller
// (owner_scope.go).
//
// Every handler that takes an ownerUserId is driven three ways: naming
// ANOTHER user (refused before anything is read or written), naming the
// caller (works), and from a trusted server-side context naming another user
// (works -- the way every shipped automation reaches these builtins). The
// last two are the positive controls: without them a handler that refused
// everything would pass the first.

const (
	userA = "v1:identity:user:owner-a"
	userB = "v1:identity:user:owner-b"
)

// asPerson is a signed-in person's context.
func asPerson(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

// asAutomation is a shipped automation's context: the cluster's own synthetic
// actor under internal origin.
func asAutomation() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:ownerScopeTest", Role: auth.RoleReader, Synthetic: true, Unranked: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

func isOwnerRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), ownerNotCallerCode)
}

func TestCallOwner(t *testing.T) {
	synthetic := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:x", Role: auth.RoleReader, Synthetic: true, Unranked: true,
	})
	clusterOwner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:operator", Role: auth.RoleOwner,
	})
	admin := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:admin", Role: auth.RoleAdmin,
	})
	cases := []struct {
		name      string
		ctx       context.Context
		requested string
		want      string
		refused   bool
	}{
		{"the caller's own id", asPerson(userA), userA, userA, false},
		{"absent resolves to the caller", asPerson(userA), "", userA, false},
		{"the caller's own id, bare", asPerson(userA), "owner-a", "owner-a", false},
		{"a bare caller naming their canonical id", asPerson("owner-a"), userA, userA, false},
		{"another user", asPerson(userA), userB, "", true},
		{"the caller's short id under another concept", asPerson(userA), "v1:agents:agent:owner-a", "", true},
		{"no actor at all", context.Background(), userA, "", true},
		{"no actor and no owner", context.Background(), "", "", false},
		{"an automation's actor at client origin", synthetic, userB, "", true},
		{"an automation's actor names nobody when absent", synthetic, "", "", false},
		{"internal origin", asAutomation(), userB, userB, false},
		{"internal origin under a person", auth.ContextWithInternalOrigin(asPerson(userA)), userB, userB, false},
		{"a cluster owner", clusterOwner, userB, userB, false},
		// admin is not in the write guard's escape set (memql#3174), so it is
		// not here either.
		{"an admin", admin, userB, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := callOwner(tc.ctx, "op", tc.requested)
			if tc.refused {
				if !isOwnerRefusal(err) {
					t.Fatalf("callOwner(%q) = %q, %v; want the %s refusal", tc.requested, got, err, ownerNotCallerCode)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("callOwner(%q) = %q, %v; want %q", tc.requested, got, err, tc.want)
			}
		})
	}
}

func TestInvokeActsForItsCaller(t *testing.T) {
	newInvoke := func() (*Integration, *fakeWorkGoals) {
		shared := specialistDef(memql.GlobalAgentOwner, "v1:agents:agent:shared", "assistant", "Shared")
		i := New(registryWith(t, shared), &recordingEngine{})
		goals := &fakeWorkGoals{}
		i.SetWorkGoals(goals)
		return i, goals
	}
	args := func(owner string) map[string]any {
		a := map[string]any{"name": "assistant", "prompt": "summarise the week", "partitionId": "s1"}
		if owner != "" {
			a["ownerUserId"] = owner
		}
		return a
	}

	i, goals := newInvoke()
	if _, err := i.handleInvoke(asPerson(userA), args(userB), 0); !isOwnerRefusal(err) {
		t.Fatalf("a caller naming another user's id was not refused: %v", err)
	}
	if len(goals.opened) != 0 {
		t.Fatalf("the refused call opened a goal: %+v", goals.opened)
	}

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		owner string
		want  string
	}{
		{"the caller's own id", asPerson(userA), userA, userA},
		{"no id: the caller", asPerson(userA), "", userA},
		{"internal origin naming another user", asAutomation(), userB, userB},
	} {
		i, goals := newInvoke()
		if _, err := i.handleInvoke(tc.ctx, args(tc.owner), 0); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(goals.opened) != 1 || goals.opened[0].OwnerUserId != tc.want {
			t.Fatalf("%s: opened %+v, want one goal owned by %s", tc.name, goals.opened, tc.want)
		}
	}
}

func TestAskSpecialistActsForItsCaller(t *testing.T) {
	i := New(registryWith(t,
		specialistDef(userA, "v1:agents:agent:a1", "human-resources", "A's HR"),
		specialistDef(userB, "v1:agents:agent:b1", "human-resources", "B's HR"),
	), stubEngine{})

	nodes, err := i.handleAskSpecialist(asPerson(userA), askArgs(userB, "human-resources"), 0)
	if !isOwnerRefusal(err) {
		t.Fatalf("a caller naming another user's id was not refused: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("the refused call returned a persona: %v", nodes)
	}

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"the caller's own id", asPerson(userB), "B's HR system prompt"},
		{"internal origin naming another user", asAutomation(), "B's HR system prompt"},
	} {
		nodes, err := i.handleAskSpecialist(tc.ctx, askArgs(userB, "human-resources"), 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), tc.want) {
			t.Fatalf("%s: got %v, want the envelope built from %q", tc.name, nodes, tc.want)
		}
	}
}

func TestRequestUserFeedbackActsForItsCaller(t *testing.T) {
	args := func(owner string) map[string]any {
		return map[string]any{"question": "Which quarter?", "kind": "text", "runId": "v1:work:run:r1", "ownerUserId": owner}
	}

	i := New(memql.NewAgentRegistry(), nil)
	goals := &fakeWorkGoals{}
	i.SetWorkGoals(goals)
	if _, err := i.handleRequestUserFeedback(asPerson(userA), args(userB), 0); !isOwnerRefusal(err) {
		t.Fatalf("a caller naming another user's id was not refused: %v", err)
	}
	if len(goals.asked) != 0 {
		t.Fatalf("the refused call raised a question: %+v", goals.asked)
	}

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		owner string
	}{
		{"the caller's own id", asPerson(userA), userA},
		{"internal origin naming another user", asAutomation(), userB},
	} {
		i := New(memql.NewAgentRegistry(), nil)
		goals := &fakeWorkGoals{}
		i.SetWorkGoals(goals)
		if _, err := i.handleRequestUserFeedback(tc.ctx, args(tc.owner), 0); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(goals.askedFor) != 1 || goals.askedFor[0] != tc.owner {
			t.Fatalf("%s: raised for %v, want %s", tc.name, goals.askedFor, tc.owner)
		}
	}
}

func TestProduceArtifactActsForItsCaller(t *testing.T) {
	args := func(owner string) map[string]any {
		return map[string]any{"goal": "A markdown file listing 10 birds", "ownerUserId": owner, "partitionId": "s1"}
	}

	i := New(memql.NewAgentRegistry(), &recordingEngine{})
	goals := &fakeWorkGoals{}
	i.SetWorkGoals(goals)
	if _, err := i.handleProduceArtifact(asPerson(userA), args(userB), 0); !isOwnerRefusal(err) {
		t.Fatalf("a caller naming another user's id was not refused: %v", err)
	}
	if len(goals.opened) != 0 {
		t.Fatalf("the refused call opened a goal: %+v", goals.opened)
	}

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		owner string
	}{
		{"the caller's own id", asPerson(userA), userA},
		{"internal origin naming another user", asAutomation(), userB},
	} {
		i := New(memql.NewAgentRegistry(), &recordingEngine{})
		goals := &fakeWorkGoals{}
		i.SetWorkGoals(goals)
		if _, err := i.handleProduceArtifact(tc.ctx, args(tc.owner), 0); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(goals.opened) != 1 || goals.opened[0].OwnerUserId != tc.owner {
			t.Fatalf("%s: opened %+v, want one goal owned by %s", tc.name, goals.opened, tc.owner)
		}
	}
}

// ensureForGoal's refusal must come before the catalog read: the read is the
// named user's agents. The accepted cases are driven until the analysis call,
// which the engine refuses, so each proves the handler read the catalog OF THE
// OWNER it was given and got no further than it needed to.
func TestEnsureForGoalActsForItsCaller(t *testing.T) {
	args := func(owner string) map[string]any {
		return map[string]any{"goal": "track my reading", "ownerUserId": owner}
	}

	eng := &ownerScopeFactoryEngine{}
	i := New(memql.NewAgentRegistry(), eng)
	if _, err := i.handleEnsureForGoal(asPerson(userA), args(userB), 0); !isOwnerRefusal(err) {
		t.Fatalf("a caller naming another user's id was not refused: %v", err)
	}
	if len(eng.queries) != 0 {
		t.Fatalf("the refused call read the catalog: %v", eng.queries)
	}

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		owner string
	}{
		{"the caller's own id", asPerson(userA), userA},
		{"internal origin naming another user", asAutomation(), userB},
	} {
		eng := &ownerScopeFactoryEngine{}
		i := New(memql.NewAgentRegistry(), eng)
		_, err := i.handleEnsureForGoal(tc.ctx, args(tc.owner), 0)
		if err == nil || isOwnerRefusal(err) || !strings.Contains(err.Error(), "analyze") {
			t.Fatalf("%s: got %v, want the call to reach the analysis step", tc.name, err)
		}
		if len(eng.queries) == 0 || !strings.Contains(eng.queries[0], tc.owner) {
			t.Fatalf("%s: read %v, want the catalog of %s read first", tc.name, eng.queries, tc.owner)
		}
	}
}

// ownerScopeFactoryEngine answers every read empty, records it, and refuses
// the analysis call.
type ownerScopeFactoryEngine struct {
	memql.IntegrationEngineAccess
	queries []string
}

func (e *ownerScopeFactoryEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	e.queries = append(e.queries, q)
	return memql.NewResultWithOutput(nil), nil
}

func (e *ownerScopeFactoryEngine) InvokeAIStructured(
	context.Context, string, map[string]any, string, json.RawMessage, bool,
) (string, error) {
	return "", errors.New("no provider in this test")
}

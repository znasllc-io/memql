package auth

import (
	"context"
	"testing"
)

// THE CLUSTER'S OWN WORK IS SAID, NEVER INFERRED (owner decision 2026-11-23).
// A call is the cluster's own only when its id is one the engine minted for
// itself, or when it names nobody and its context carries the cluster's own
// actor or a named job. A context nobody stamped, and the anonymous reader,
// say nothing of the kind.
func TestActsForTheClusterOnlyWhenTheCallSaysSo(t *testing.T) {
	bg := context.Background()
	safety := ContextForClusterWork(bg, "safety-classifier")
	alice := ContextWithUserActor(bg, "v1:identity:user:alice")
	tomorrow := ContextWithAccess(bg, &AccessContext{UserId: "bench:tomorrow", Role: RoleOwner, Synthetic: true})

	for _, tc := range []struct {
		name string
		ctx  context.Context
		id   string
		want bool
	}{
		// Says so by its id: the engine mints these for itself and nothing else.
		{"a system actor's id", bg, SystemActor("edge").UserId, true},
		{"an automation's id", bg, "system:automation:nightly", true},
		{"a maintenance sweep's id", bg, MaintenanceActor(AutomationOriginEngine, "auditEventRetentionSweep").UserId, true},
		{"the operator's stream", bg, "cluster:operator", true},
		{"the work journal", bg, "cluster:work-journal", true},
		{"a connector's id", bg, ConnectorActor("shopify").UserId, true},

		// Says so by its context, while the call names nobody.
		{"a job wired as the cluster's own", safety, "", true},
		{"the cluster's own actor", ContextWithSystemActor(bg, "edge"), "", true},
		{"a connector acting", ContextWithConnectorActor(bg, "shopify"), "", true},
		{"a synthetic actor under a prefix nobody listed", tomorrow, "bench:tomorrow", true},
		// The job is the cluster's even when it runs for a person, as the safety
		// check screens a person's command.
		{"a job wired as the cluster's own, run for a person", ContextForClusterWork(alice, "safety-classifier"), "", true},

		// Says nothing of the kind.
		{"nobody stamped anything", bg, "", false},
		{"no context at all", nil, "", false},
		{"a job with no name", ContextForClusterWork(bg, "  "), "", false},
		{"the anonymous reader", ContextWithAnonymousActor(bg), AnonymousUserId, false},
		{"the anonymous reader, unnamed", ContextWithAnonymousActor(bg), "", false},
		{"a person's own context", alice, "", false},
		{"a person, named", alice, "v1:identity:user:alice", false},
		// A call that names a person is that person's, whatever its context.
		{"a person named under a job's context", safety, "v1:identity:user:alice", false},
		{"a person named under the cluster's actor", tomorrow, "v1:identity:user:alice", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActsForTheCluster(tc.ctx, tc.id); got != tc.want {
				t.Fatalf("ActsForTheCluster(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// The job's name rides the context, and a blank name stamps nothing: a caller
// that cannot say which job it is gets no claim at all.
func TestContextForClusterWorkNamesTheJob(t *testing.T) {
	ctx := ContextForClusterWork(context.Background(), " safety-classifier ")
	if job, ok := ClusterWorkFromContext(ctx); !ok || job != "safety-classifier" {
		t.Fatalf("ClusterWorkFromContext = %q, %v; want the job's name", job, ok)
	}
	if _, ok := ClusterWorkFromContext(ContextForClusterWork(context.Background(), "")); ok {
		t.Fatal("a blank job name was stamped")
	}
	if _, ok := ClusterWorkFromContext(nil); ok {
		t.Fatal("a nil context carried a job")
	}
}

// A named job is the cluster acting, so its call is recorded as the system's
// rather than as nobody's. A person's access context still answers first: the
// kind is what the context says about the caller, and a person is a person.
func TestCallerKindOfANamedJob(t *testing.T) {
	if got := CallerKindFromContext(ContextForClusterWork(context.Background(), "safety-classifier")); got != CallerKindSystem {
		t.Fatalf("CallerKindFromContext = %q for a named job, want %q", got, CallerKindSystem)
	}
	person := ContextWithUserActor(context.Background(), "v1:identity:user:alice")
	if got := CallerKindFromContext(ContextForClusterWork(person, "safety-classifier")); got != CallerKindUser {
		t.Fatalf("CallerKindFromContext = %q for a job run for a person, want %q", got, CallerKindUser)
	}
}

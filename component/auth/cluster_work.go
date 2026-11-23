package auth

import (
	"context"
	"strings"
)

// cluster_work.go -- THE CLUSTER'S OWN WORK IS SAID, NEVER INFERRED (owner
// decision 2026-11-23; design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// "Decided by the owner after landing").
//
// # Why this exists
//
// The cluster's own vendor account pays for the cluster's own work without
// asking anybody, and for a person only when that person may use it
// (component/memql vendor_account_cluster.go). "The cluster's own work" was
// first defined by the ABSENCE of a person: a blank acting id counted. A call
// site that lost track of who started it then looked exactly like a schedule
// or a sweep, so a person's work could be paid by the cluster's account with no
// permission asked, and the only trace was `callerKind: unattributed` on its
// decision row. The owner chose a positive definition: a call is the cluster's
// own only when it says so, and a call that says neither whom it is for nor
// that it is the cluster's is refused the account.
//
// # What says so
//
//   - The call's id is one minted for the cluster's own actors, never for a
//     person: a system actor (`system:`, which covers an automation's
//     `system:automation:<name>` and the maintenance principal), one of the
//     cluster's named principals (`cluster:`, among them the operator
//     credential's stream subject), or a connector. A service-account token
//     an operator mints for a machine (the deploy gate's, say) may carry a
//     `system:` subject too, and its calls are the cluster's for the same
//     reason: an operator holds it, and no person does.
//   - Or the call names nobody, and its context carries the cluster's own
//     actor (a Synthetic one, or a connector), or a JOB a Go caller wired as
//     the cluster's own with ContextForClusterWork.
//
// A call that names a PERSON is that person's, whatever its context carries,
// and is judged by that person's permission. A context nobody stamped and the
// anonymous reader say nothing about the cluster.
//
// # Why a named job and not the system actor
//
// Some of the cluster's work is wired once and run for many callers: the
// command-safety check resolves its model when the node starts and screens
// every later command, whoever sent it. ContextWithSystemActor would claim the
// work, but it also puts a `system:` id on the call, and the fleet reads an
// acting id to choose machines: a blank one reaches the machines their owners
// lent to cluster work, a named one only that name's own (component/worker
// fleetcatalog). The job claims the work and leaves the id, and so the
// machines a call may use, as they were.
//
// Regression tests: cluster_work_test.go; component/memql
// vendor_account_cluster_test.go holds the account rule built on it.

// clusterWorkKey carries the name of the cluster job a context runs.
type clusterWorkKey struct{}

// ContextForClusterWork stamps the name of one of the cluster's own jobs onto
// ctx, so a model call made under it is the cluster's own work: the cluster's
// vendor account pays for it without asking, and its decision row records the
// system as the caller.
//
// FOR GO WIRING ONLY, at the place a job of the cluster's is composed (the
// safety check, the healer). It is not a way to make a person's call free: a
// call that names a person stays that person's whatever this stamp says
// (ActsForTheCluster).
//
// STAMP IT ON A CONTEXT THAT NAMES NOBODY. A resolution fills the call's
// acting id from the person on its context before the account is asked (the
// router's ResolveFor, the engine's applyCallAttribution), and from then on
// the call names that person. So a job resolves under a bare context, as
// app's resolveForClusterJob does, and it is a CALL made later on that
// resolution, whose request names nobody, that the stamp on its own context
// claims for the job. A job resolved under a person's context is that
// person's call (router TestAJobsStampDoesNotPayForAPersonItsContextNames).
//
// A blank job returns ctx unchanged, matching ContextWithSystemActor: a caller
// that cannot say which job it is claims nothing.
func ContextForClusterWork(ctx context.Context, job string) context.Context {
	job = strings.TrimSpace(job)
	if job == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clusterWorkKey{}, job)
}

// ClusterWorkFromContext returns the cluster job ctx runs, if one was stamped.
func ClusterWorkFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	job, ok := ctx.Value(clusterWorkKey{}).(string)
	return job, ok && job != ""
}

// ActsForTheCluster reports whether a call SAYS it is the cluster's own work.
//
// id is who the call names (a router request's UserId, a vendor account
// query's ActingUserId). The order is the definition:
//
//  1. An id the engine minted for itself is the cluster's, whatever the
//     context: the id is the claim.
//  2. An id that names anybody else is not the cluster's -- a person is a
//     person, and the anonymous reader is nobody -- unless it is the id of
//     the context's own actor and that actor is the cluster's, a principal
//     minted under a prefix the string test does not know.
//  3. A call that names nobody is the cluster's when its context carries a
//     job of the cluster's, or the cluster's own actor.
//
// Nothing else is. This is narrower than ActsForNoPerson on purpose, and the
// two answer different questions: ActsForNoPerson decides who CANNOT be
// served as a person (no key, no app session, no pinned machine), and
// ActsForTheCluster decides who the cluster pays for unasked.
func ActsForTheCluster(ctx context.Context, id string) bool {
	id = strings.TrimSpace(id)
	if id != "" && (IsSystemActorId(id) ||
		strings.HasPrefix(id, clusterPrincipalPrefix) ||
		strings.HasPrefix(id, connectorUserIdPrefix)) {
		return true
	}
	ac, hasActor := AccessFromContext(ctx)
	clusterActor := hasActor && ac != nil && !ac.IsAnonymous && (ac.Synthetic || ac.ConnectorName != "")
	if id != "" {
		return clusterActor && strings.TrimSpace(ac.UserId) == id
	}
	if _, ok := ClusterWorkFromContext(ctx); ok {
		return true
	}
	return clusterActor
}

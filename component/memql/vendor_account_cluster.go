package memql

// THE CLUSTER'S OWN VENDOR ACCOUNT IS NOT A POOL FOR EVERYBODY (owner decision
// 2026-11-22, recorded at the end of
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md).
//
// The rule on a cluster is bring-your-own for everything a person does with a
// model: a machine, an app, a key. The cluster's own account at a vendor, when
// its owner connected one, bills that owner's organization, and the first
// build offered it to every person as the last account of every vendor call
// (K8). The owner had not understood it that way and did not want it.
//
// THE RULE. The cluster's account pays for:
//
//   - the cluster's own work: a call no person is behind (auth.ActsForNoPerson
//     -- a schedule, a sweep, a seed, the operator's stream);
//   - a person's call, only when that person holds
//     `execute app:settings/clusteraccount`. The cluster owner holds it from
//     the start (it is seeded on the owner role and no other), and gives it to
//     a person or a group in Users > Access.
//
// Everybody else reaches a vendor with an API key, their own or one lent to
// them, or not at all.
//
// WHO IS JUDGED. The person the call is FOR, by the rows: their stored role,
// their groups and their grants. Never the role a context happens to carry.
// Work continued on a person's behalf runs under borrowed authority, whose
// role is a stand-in (auth.AccessContext.RoleStandIn); judged as it stood, a
// cluster owner's own run would lose the account and a reader's would not
// matter either way. A visitor's call (a shopper form, an access code, a
// deployed workflow) is judged by the person whose authority it borrows. A
// person the principal table does not resolve holds nothing.
//
// WHERE IT IS ASKED. Wherever the cluster's account would pay: in the
// composition of a record's accounts (VendorAccountsWith), again when a
// wrapper resolves "record@cluster" at the call (VendorAccountUsable), in the
// vendor door a page reads (VendorDoorFor), and by the router for the vendor
// surfaces that take no account of a person's -- vision, speech,
// transcription and images (component/router). EMBEDDINGS ARE NOT ASKED: the
// memory space is the cluster's, one model for everybody, and a person whose
// writes were not embedded would be missing from everybody's search.
//
// THE SEAM. The answer needs the principal table and the grants, which the
// provider registry cannot read. The engine installs its own gate when it
// starts (engine_bootstrap.go). A registry nobody installed a gate on gives
// the account to no person: the narrowing direction, as a node with no key
// seam has no keys.
//
// COST. The gate reads rows, so it is asked only where there is an account to
// open (a record the cluster can pay for), and once per answer: one
// resolution asks at most once per vendor however many records a route names.
// A cluster with no vendor account never asks.
//
// Regression tests: vendor_account_cluster_test.go,
// cluster_account_gate_db_test.go; the router's half is
// component/router/accounts_test.go.

import (
	"context"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
)

// ClusterAccountPart is the capability a person holds to have the cluster's
// own vendor account pay for their calls. `execute` on it; seeded on the owner
// role only (dsl/rbac/seeds.memql).
const ClusterAccountPart = "app:settings/clusteraccount"

// ClusterAccountClosedReason is what a decision record says when the cluster's
// account was passed over because it does not pay for this person. The record
// is readable by every signed-in person, so it names nobody.
const ClusterAccountClosedReason = "the cluster's account does not pay for this person's calls"

// ClusterAccountGate answers whether the cluster's own vendor account may pay
// for a call made for one person. It is never asked about the cluster's own
// work.
type ClusterAccountGate func(ctx context.Context, actingUserId string) bool

// SetClusterAccountGate installs the gate. Nil removes it, and the cluster's
// account then pays for no person's call.
func (r *ProviderRegistry) SetClusterAccountGate(g ClusterAccountGate) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clusterGate = g
}

func (r *ProviderRegistry) installedClusterAccountGate() ClusterAccountGate {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clusterGate
}

// ClusterAccountClosedTo is why the cluster's own account may not pay for a
// call made for actingUserId, or "" when it may. The cluster's own work is
// always paid and the gate is not asked about it.
func (r *ProviderRegistry) ClusterAccountClosedTo(ctx context.Context, actingUserId string) string {
	actingUserId = strings.TrimSpace(actingUserId)
	if auth.ActsForNoPerson(ctx, actingUserId) {
		return ""
	}
	gate := r.installedClusterAccountGate()
	if gate == nil || !gate(ctx, actingUserId) {
		return ClusterAccountClosedReason
	}
	return ""
}

// clusterAccountAnswer is ClusterAccountClosedTo for one person, asked at most
// once: the first time a record that the cluster can pay for needs it.
type clusterAccountAnswer struct {
	once   sync.Once
	ask    func() string
	reason string
}

// clusterAccountFor prepares the answer for one person without asking yet.
func (r *ProviderRegistry) clusterAccountFor(ctx context.Context, actingUserId string) *clusterAccountAnswer {
	return &clusterAccountAnswer{ask: func() string { return r.ClusterAccountClosedTo(ctx, actingUserId) }}
}

// closed is why the cluster's account may not pay, or "" when it may. An
// answer nobody prepared is closed: a composition handed no answer for a
// person must not hand them the cluster's account.
func (a *clusterAccountAnswer) closed() string {
	if a == nil || a.ask == nil {
		return ClusterAccountClosedReason
	}
	a.once.Do(func() { a.reason = a.ask() })
	return a.reason
}

package vendorkeys

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// hop_db_test.go -- a key's rows decide on EVERY replica (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// mesh rules M1, M3 and M6).
//
// The cluster runs two replicas of every node, and a vendor call is paid on
// whichever replica makes it: an agent or a planner as often as the bff its
// owner is connected to. So "which account pays" is asked on a node that never
// saw the key being added, changed or removed, with nothing in between to tell
// it. These tests stand up two replicas over one database, each with its own
// engine, its own provider registry, its own resolver and its own router, and
// change a key through one while the other serves the call.
//
// They fail against a resolver that remembers rows, which is the single-node
// assumption being ruled out: replace Store.ActiveKeysAtVendor's read with a
// remembered slice and both replica tests go red.

// hopVendor stands in for one vendor text record on a replica. It serves a
// chat turn, and bound to an account it says who paid.
type hopVendor struct {
	paid   *hopCalls
	paidBy memql.VendorAccount
}

type hopCalls struct {
	mu   sync.Mutex
	made []string
}

func (c *hopCalls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.made...)
}

func (v *hopVendor) WithVendorAccount(account memql.VendorAccount, _ memql.VendorKeys) any {
	bound := *v
	bound.paidBy = account
	return &bound
}
func (v *hopVendor) VendorOrigin() string { return "" }
func (v *hopVendor) Call(context.Context, string) (any, error) {
	return v.CallChat(context.Background(), nil)
}
func (v *hopVendor) CallChat(context.Context, []common.ChatMessage) (string, error) {
	v.paid.mu.Lock()
	defer v.paid.mu.Unlock()
	v.paid.made = append(v.paid.made, v.paidBy.Ref())
	return "answer", nil
}

// replica is one node: its own resolver over its own engine, the registry that
// asks it, and the router that walks the registry.
type replica struct {
	store    *Store
	resolver *Resolver
	router   *router.Router
	paid     *hopCalls
}

const hopRoute = "chosenRoute"

// newReplica builds a node over an engine. clusterAccount says whether the
// cluster has an account at the vendor on this node.
func newReplica(t *testing.T, w *world, eng *memql.MemQLEngine, clusterAccount bool) *replica {
	t.Helper()
	r := &replica{paid: &hopCalls{}}
	r.store, r.resolver = w.over(eng)

	providers := memql.NewProviderRegistryForTest()
	providers.RegisterWithParamsForTest("claude", "anthropic", "claude-sonnet",
		map[string]any{"contextWindow": 200000},
		&hopVendor{paid: r.paid, paidBy: memql.VendorAccount{Kind: memql.VendorAccountCluster, Vendor: memql.VendorAnthropic}})
	if !clusterAccount {
		entry, _ := providers.Entry("claude")
		entry.Available = false
	}
	providers.SetVendorKeys(r.resolver)

	rules := memql.NewRuleRegistry()
	require.NoError(t, rules.Register(&memql.RuleConfig{
		Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}},
		Policy: hopRoute, OnUnavailable: memql.OnUnavailableDegrade, Locked: true, SourceFile: "dsl/rules/rules.memql",
	}))
	require.NoError(t, rules.Finalize())
	policies := memql.NewPolicyRegistryForTest(map[string][]string{hopRoute: {"claude"}})
	r.router = router.New(providers, policies, rules, nil, nil)
	return r
}

// resolve asks the replica's router for a chat turn under a route the person
// chose, the way a model call reaches it.
func (r *replica) resolve(t *testing.T, user string) (memql.ResolvedProvider, error) {
	t.Helper()
	return r.router.ResolveFor(actorContext(user), airoute.ResolveRequest{
		Level: airoute.LevelStrong, Modality: airoute.ModalityChat, Route: hopRoute, RouteBy: user,
	})
}

func (r *replica) call(t *testing.T, user string, resolved memql.ResolvedProvider) {
	t.Helper()
	_, err := resolved.Client.(common.ChatAIProvider).CallChat(actorContext(user), []common.ChatMessage{{Role: "user", Content: "hello"}})
	require.NoError(t, err)
}

func twoReplicas(t *testing.T, w *world, clusterAccount bool) (a, b *replica) {
	t.Helper()
	other, otherDB, err := newEngine(sharedBoot.dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = otherDB.Close() })
	require.NotSame(t, w.eng, other)
	return newReplica(t, w, w.eng, clusterAccount), newReplica(t, w, other, clusterAccount)
}

// M6: a key removed through one replica is refused on the other, at once, and
// that includes a call the other replica had already resolved to the key.
func TestAKeyRemovedOnOneReplicaIsRefusedOnTheOther(t *testing.T) {
	w := newWorld(t, "hop-remove")
	alice := w.person("alice")
	key := w.key(alice, "mine")
	a, b := twoReplicas(t, w, true)

	// A call on B is paid by the key.
	resolved, err := b.resolve(t, alice)
	require.NoError(t, err)
	require.Equal(t, memql.VendorAccountKey, resolved.Resolution.Decision.AccountKind)
	require.Equal(t, key, resolved.Resolution.Decision.AccountKeyId)
	b.call(t, alice, resolved)
	require.Equal(t, []string{"key:" + key}, b.paid.list())

	// B resolves the next call to the key as well, and before it is made the
	// key is removed through A.
	pending, err := b.resolve(t, alice)
	require.NoError(t, err)
	require.Equal(t, key, pending.Resolution.Decision.AccountKeyId)
	require.NoError(t, a.store.Remove(context.Background(), key, alice))

	// The call B had already resolved does not spend it: the account is judged
	// again when the call is made, and the cluster's account pays.
	b.call(t, alice, pending)
	require.Equal(t, []string{"key:" + key, memql.VendorAccountCluster}, b.paid.list(),
		"a key removed through another replica paid for a call that was already resolved here")

	// And a new resolution on B does not offer it.
	after, err := b.resolve(t, alice)
	require.NoError(t, err)
	require.Equal(t, memql.VendorAccountCluster, after.Resolution.Decision.AccountKind)
	require.Empty(t, after.Resolution.Decision.AccountKeyId)
}

// On a cluster with no account at the vendor, the key is all there is: once it
// is removed through one replica, the other refuses the call and says why.
func TestARemovedKeyLeavesNothingToPayWith(t *testing.T) {
	w := newWorld(t, "hop-alone")
	alice := w.person("alice")
	key := w.key(alice, "mine")
	a, b := twoReplicas(t, w, false)

	resolved, err := b.resolve(t, alice)
	require.NoError(t, err, "a person's key did not serve a model the cluster has no account for")
	require.Equal(t, key, resolved.Resolution.Decision.AccountKeyId)

	require.NoError(t, a.store.Remove(context.Background(), key, alice))
	_, err = b.resolve(t, alice)
	require.Error(t, err, "a removed key still served on the other replica")
	require.Contains(t, err.Error(), "no account can pay for this call")
	require.Contains(t, err.Error(), "the cluster's account is not set up for Anthropic")
}

// M1 and M3: what one replica learned about a key is read by the other on its
// next call, and the record there says why the key did not pay.
func TestAStateWrittenOnOneReplicaIsReadOnTheOther(t *testing.T) {
	w := newWorld(t, "hop-state")
	alice, bob := w.person("alice"), w.person("bob")
	lent := w.key(bob, "lent")
	w.lend(bob, lent, people(alice))
	a, b := twoReplicas(t, w, true)

	resolved, err := b.resolve(t, alice)
	require.NoError(t, err)
	require.Equal(t, lent, resolved.Resolution.Decision.AccountKeyId)
	require.Equal(t, bob, resolved.Resolution.Decision.AccountKeyOwnerUserId)

	// A's call was refused by the vendor.
	w.clock = w.clock.Add(time.Minute)
	a.resolver.Observe(context.Background(), keyAccount(lent, bob), memql.VendorKeyRefused, w.clock, "invalid x-api-key")

	after, err := b.resolve(t, alice)
	require.NoError(t, err)
	decision := after.Resolution.Decision
	require.Equal(t, memql.VendorAccountCluster, decision.AccountKind, "a key one replica saw refused paid on the other")
	var reason string
	for _, line := range decision.Considered {
		if line.Entry == "claude" && line.Account == "key:"+lent {
			reason = line.Reason
		}
	}
	require.Equal(t, "an API key was refused by Anthropic", reason, "the record does not say why the key did not pay: %+v", decision.Considered)

	// The owner pausing the borrower through A reads the same way on B.
	w.clock = w.clock.Add(time.Minute)
	a.resolver.Observe(context.Background(), keyAccount(lent, bob), memql.VendorKeyGood, w.clock, "")
	back, err := b.resolve(t, alice)
	require.NoError(t, err)
	require.Equal(t, lent, back.Resolution.Decision.AccountKeyId, "a key that recovered on one replica stayed refused on the other")
}

// The cluster's own work on either replica never reaches a key, whatever the
// rows say.
func TestNoReplicaSpendsAKeyForTheClustersOwnWork(t *testing.T) {
	w := newWorld(t, "hop-system")
	alice := w.person("alice")
	w.key(alice, "mine", autoOn())
	_, b := twoReplicas(t, w, true)

	sweep := auth.ContextWithAccess(context.Background(), auth.SystemActor("nightly-sweep"))
	resolved, err := b.router.ResolveFor(sweep, airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat})
	require.NoError(t, err)
	require.Equal(t, memql.VendorAccountCluster, resolved.Resolution.Decision.AccountKind)
	for _, line := range resolved.Resolution.Decision.Considered {
		require.False(t, strings.HasPrefix(line.Account, "key:"), "the cluster's own work considered a key: %+v", line)
	}
}

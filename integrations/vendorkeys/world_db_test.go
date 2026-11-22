package vendorkeys

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/component/worker"
)

// world_db_test.go -- the people and keys a test acts on, against a real engine
// and a real database.
//
// WHY A REAL ENGINE. Every property in this package is a claim about what a
// row holds and about who the engine took a read or a write to be: a key read
// as the cluster acting although it belongs to somebody else, a verdict that
// must not replace a newer one, a pause written by another replica that the
// next call has to see. A fake engine has none of that.
//
// Postgres-gated: a skip is what an unreachable database produces, and
// MEMQL_REQUIRE_DB=1 turns it into a failure.

// testMasterKey seals and unseals in these tests. It protects nothing.
var testMasterKey = strings.Repeat("3c", 32)

// world is one test's people, their keys and the resolver that judges them.
type world struct {
	t        *testing.T
	eng      *memql.MemQLEngine
	db       *bun.DB
	gate     *sql.DB
	store    *Store
	resolver *Resolver

	suffix string
	// groups is who is in which group, for the resolver's membership read.
	groups map[string][]string
	// clock is the resolver's and the store's time. A test moves it.
	clock time.Time

	users []string
	keys  []string
}

func newWorld(t *testing.T, name string) *world {
	t.Helper()
	eng, db, gate := bootEngine(t)
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	now := time.Now()
	w := &world{
		t: t, eng: eng, db: db, gate: gate,
		suffix: fmt.Sprintf("%s-%d-%d", name, now.Unix(), now.Nanosecond()),
		groups: map[string][]string{},
		clock:  now.UTC().Truncate(time.Second),
	}
	w.store, w.resolver = w.over(eng)
	t.Cleanup(w.sweep)
	return w
}

// over builds a store and a resolver on an engine, sharing this world's clock
// and groups. A second engine gets its own pair, as a second replica would.
func (w *world) over(eng *memql.MemQLEngine) (*Store, *Resolver) {
	store := NewStore(liveEngine{eng}, func() *sql.DB { return w.gate }, discardLogger())
	store.now = func() time.Time { return w.clock }
	resolver := NewResolver(store, func(_ context.Context, user string) []string { return w.groups[memql.BareShortId(user)] })
	return store, resolver
}

func seedContext() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "vendor-keys-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "vendor-keys-seeder"})
}

func (w *world) sweep() {
	ctx := context.Background()
	for _, user := range w.users {
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", "v1:identity:user:"+user).Exec(ctx)
	}
	for _, key := range w.keys {
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("id IN (?, ?)", keyConcept+":"+key, stateConcept+":"+key).Exec(ctx)
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept = ?", "v1:identity:auditEvent").Where("payload->>'targetId' LIKE ?", "%"+key).Exec(ctx)
	}
}

// person writes a user and returns their bare id.
func (w *world) person(name string) string {
	w.t.Helper()
	user := "vk-" + name + "-" + w.suffix
	w.writeUser(user, true)
	w.users = append(w.users, user)
	return user
}

func (w *world) writeUser(user string, active bool) {
	w.t.Helper()
	payload, err := json.Marshal(map[string]any{
		"displayName": user, "primaryEmail": user + "@example.test", "role": string(auth.RoleWriter), "active": active,
	})
	require.NoError(w.t, err)
	_, err = w.eng.Execute(seedContext(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString("v1:identity:user"), langparser.QuoteString(user), payload))
	require.NoError(w.t, err, "writing the user %s", user)
}

// keyOptions shape one key a test adds.
type keyOptions struct {
	vendor    string
	notDef    bool
	auto      bool
	addedAt   time.Duration // moves the clock for this add only, so an order can be staged
	plain     string
	checkedBy string
}

type keyOption func(*keyOptions)

func atVendor(v string) keyOption       { return func(o *keyOptions) { o.vendor = v } }
func notTheDefault() keyOption          { return func(o *keyOptions) { o.notDef = true } }
func autoOn() keyOption                 { return func(o *keyOptions) { o.auto = true } }
func added(ago time.Duration) keyOption { return func(o *keyOptions) { o.addedAt = -ago } }
func holding(plain string) keyOption    { return func(o *keyOptions) { o.plain = plain } }

// key adds a key for an owner the way the add does: both rows, as the owner,
// good as of the check that let it in. It returns the key's bare id.
func (w *world) key(owner, label string, opts ...keyOption) string {
	w.t.Helper()
	o := keyOptions{vendor: memql.VendorAnthropic, plain: "plain-value-of-" + label, checkedBy: "list"}
	for _, opt := range opts {
		opt(&o)
	}
	id := "vk-key-" + label + "-" + w.suffix
	sealed, fingerprint, err := secret.Encrypt(o.plain)
	require.NoError(w.t, err)

	saved := w.clock
	w.clock = w.clock.Add(o.addedAt)
	err = w.store.Create(context.Background(), owner, NewKey{
		Id: id, Vendor: o.vendor, Label: label + " " + w.suffix, Fingerprint: fingerprint,
		CheckedBy: o.checkedBy, IsDefault: !o.notDef, Sealed: sealedValue(sealed),
	})
	w.clock = saved
	require.NoError(w.t, err, "adding the key %s", label)
	w.keys = append(w.keys, id)
	if o.auto {
		on := true
		require.NoError(w.t, w.store.UpdateSettings(context.Background(), id, Settings{AutoUse: &on}))
	}
	return id
}

// lend sets who a key is lent to, as its owner.
func (w *world) lend(owner, key string, sharing worker.Sharing) {
	w.t.Helper()
	require.NoError(w.t, w.store.SetSharing(context.Background(), owner, key, sharing))
}

func people(users ...string) worker.Sharing {
	return worker.Sharing{Mode: worker.SharingModePeople, UserIds: users}
}

// asked is what KeysFor answered, in the words a test compares.
type asked struct {
	usable []string
	// passed is each passed-over key's reason, by key id.
	passed map[string]string
}

func (w *world) keysFor(r *Resolver, user string, chosen bool, vendor ...string) asked {
	w.t.Helper()
	q := memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: user, Chosen: chosen}
	if len(vendor) > 0 {
		q.Vendor = vendor[0]
	}
	usable, passedOver, err := r.KeysFor(actorContext(user), q)
	require.NoError(w.t, err)
	out := asked{passed: map[string]string{}}
	for _, account := range usable {
		require.Equal(w.t, memql.VendorAccountKey, account.Kind)
		require.Equal(w.t, q.Vendor, account.Vendor)
		out.usable = append(out.usable, account.KeyId)
	}
	for _, p := range passedOver {
		out.passed[p.Account.KeyId] = p.Reason
	}
	return out
}

func actorContext(user string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: user})
}

// state reads a key's state row from the database, past every cache.
func (w *world) state(key string) map[string]any {
	w.t.Helper()
	return w.latest(stateConcept, key)
}

func (w *world) latest(concept, short string) map[string]any {
	w.t.Helper()
	var node memorynodes.MemoryNode
	err := w.db.NewSelect().Model(&node).
		Where("concept = ?", concept).Where("id = ?", concept+":"+short).
		Order("createdAt DESC").Limit(1).Scan(context.Background())
	require.NoError(w.t, err, "reading %s %s", concept, short)
	var payload map[string]any
	require.NoError(w.t, json.Unmarshal(node.Payload, &payload))
	return payload
}

// auditActions are the audit events written about a key, oldest first.
func (w *world) auditActions(key string) []string {
	w.t.Helper()
	var nodes []memorynodes.MemoryNode
	err := w.db.NewSelect().Model(&nodes).
		Where("concept = ?", "v1:identity:auditEvent").
		Where("payload->>'targetType' = ?", "vendorKey").
		Where("payload->>'targetId' LIKE ?", "%"+key).
		Order("createdAt ASC").Scan(context.Background())
	require.NoError(w.t, err)
	type event struct {
		at     string
		action string
	}
	var events []event
	for _, node := range nodes {
		var payload map[string]any
		require.NoError(w.t, json.Unmarshal(node.Payload, &payload))
		at, _ := payload["occurredAt"].(string)
		action, _ := payload["action"].(string)
		events = append(events, event{at, action})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.action)
	}
	return out
}

// lastAudit is the newest audit event about a key, as stored.
func (w *world) lastAudit(key string) string {
	w.t.Helper()
	var node memorynodes.MemoryNode
	err := w.db.NewSelect().Model(&node).
		Where("concept = ?", "v1:identity:auditEvent").
		Where("payload->>'targetType' = ?", "vendorKey").
		Where("payload->>'targetId' LIKE ?", "%"+key).
		Order("createdAt DESC").Limit(1).Scan(context.Background())
	require.NoError(w.t, err)
	return string(node.Payload)
}

package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/adminops"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Use the same always-on factory as every serving node. These tests exercise
// the real DSL catalog, integration adapter, audited admin gate and PostgreSQL
// write, with independent engine caches and no model or transport test double.
func registerDisplayNameIdentity(t *testing.T, engine *memql.MemQLEngine, access memql.IntegrationEngineAccess) {
	t.Helper()
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name == "identity" {
			provider, err := plugin.Factory(memql.PluginContext{Engine: access, Logger: engine.Logger})
			require.NoError(t, err)
			require.NoError(t, engine.RegisterIntegration(provider))
			return
		}
	}
	t.Fatal("the identity integration is not registered on this node build")
}

func displayNameActor(user string, role auth.Role) context.Context {
	return auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), user), &auth.AccessContext{
		UserId: user, Role: role, PrimaryEmail: "name-admin@example.test",
	})
}

func displayNameCall(engine *memql.MemQLEngine, ctx context.Context, kind, name string, args map[string]any) (*memql.ExecuteResult, error) {
	call, err := parser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	return engine.Execute(ctx, kind+" "+call)
}

func displayNameFixture(t *testing.T, engine *memql.MemQLEngine, db *bun.DB) (context.Context, string) {
	t.Helper()
	owner, target := "name-admin-"+id.NewShortId(), "name-target-"+id.NewShortId()
	ctx := displayNameActor(owner, auth.RoleOwner)
	canonical := "v1:identity:user:" + target
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM "MemoryNodes" WHERE id = ? OR (concept = 'v1:identity:auditEvent' AND payload->>'actorUserId' IN (?, ?))`, canonical, owner, "v1:identity:user:"+owner)
		require.NoError(t, err)
	})
	templateMutation(t, engine, auth.ContextWithInternalOrigin(ctx), "identity.createUserOnFirstLogin", map[string]any{
		"userId": target, "displayName": "Original display", "primaryEmail": target + "@example.test",
		"firstName": "Keep given", "lastName": "Keep family", "phone": "+15550000001",
		"primaryRole": "Engineer", "gender": "unspecified", "birthdate": "1990-01-01",
		"role": "developer", "internal": true, "sharedMailbox": true,
		"preferences": map[string]any{"theme": "dark", "computerUseEnabled": false},
	})
	templateMutation(t, engine, auth.ContextWithInternalOrigin(ctx), "identity.updateUser", map[string]any{
		"userId": target, "payload": map[string]any{"signInPolicy": "passkey_only", "revocationEpoch": 7},
	})
	return ctx, target
}

func displayNameStoredPayload(t *testing.T, db *bun.DB, target string) map[string]any {
	t.Helper()
	var raw []byte
	err := db.NewSelect().Table("MemoryNodes").Column("payload").Where("id = ?", "v1:identity:user:"+target).
		OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background(), &raw)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	return payload
}

func TestIdentityDisplayName(t *testing.T) {
	first, db := workTemplateDBEngineAndDB(t)
	second := workTemplateDBEngine(t)
	access := &displayNameInterleavingEngine{IntegrationEngineAccess: first}
	registerDisplayNameIdentity(t, first, access)
	registerDisplayNameIdentity(t, second, second)
	// Each case owns fresh user ids and actors; the adapter constructs the
	// admin service per invocation. Reuse the loaded DSL rather than booting
	// eight engines, retaining two distinct replica caches. The parent keeps
	// their pools alive until every subtest has cleaned its rows. The read hook
	// is case-scoped, and the commit-window hook is installed last.
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *memql.MemQLEngine, *memql.MemQLEngine, *bun.DB)
	}{
		{"CapabilityDiscoveryWriteAndCrossEngineReadback", testIdentityDisplayNameCapabilityDiscoveryWriteAndCrossEngineReadback},
		{"RefusesConcurrentReplicaChangeThenUsesFreshVersion", func(t *testing.T, first, second *memql.MemQLEngine, db *bun.DB) {
			testIdentityDisplayNameRefusesConcurrentReplicaChangeThenUsesFreshVersion(t, first, second, db, access)
		}},
		{"SurvivesLegacyServiceReadBeforeRename", testIdentityDisplayNameSurvivesLegacyServiceReadBeforeRename},
		{"SerializesLegacyWriterAcrossCommitWindow", testIdentityDisplayNameSerializesLegacyWriterAcrossCommitWindow},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, first, second, db) })
	}
}

func testIdentityDisplayNameCapabilityDiscoveryWriteAndCrossEngineReadback(t *testing.T, first, second *memql.MemQLEngine, db *bun.DB) {
	t.Helper()
	ctx, target := displayNameFixture(t, first, db)
	before := displayNameStoredPayload(t, db, target)

	// A bounded identifier read is enough to prepare the write; the agent
	// never needs a full account inventory or the server-only payload writer.
	lookup, err := displayNameCall(first, ctx, "query", "identity.userById", map[string]any{"userId": target})
	require.NoError(t, err)
	require.Len(t, memql.MaterializeRows(lookup), 1)
	discovered, err := displayNameCall(first, ctx, "builtin", "work.workCapabilities", map[string]any{"search": "identity.setUserDisplayName"})
	require.NoError(t, err)
	discoveryRows := memql.MaterializeRows(discovered)
	require.Len(t, discoveryRows, 1)
	contracts, ok := discoveryRows[0]["capabilities"].([]any)
	require.True(t, ok)
	require.Len(t, contracts, 1)
	contract := contracts[0].(map[string]any)
	require.Equal(t, "identity.setUserDisplayName", contract["name"])
	require.Equal(t, "builtin", contract["kind"])
	arguments := contract["arguments"].([]any)
	require.Len(t, arguments, 2, "the catalog must not expose an arbitrary profile payload")
	argumentNames := []string{}
	for _, value := range arguments {
		argument := value.(map[string]any)
		argumentNames = append(argumentNames, argument["name"].(string))
		require.Equal(t, "string", argument["type"])
		require.Equal(t, false, argument["optional"])
	}
	require.ElementsMatch(t, []string{"userId", "displayName"}, argumentNames)
	for _, search := range []string{"rename user", "update user display name"} {
		t.Run("discovers_"+strings.ReplaceAll(search, " ", "_"), func(t *testing.T) {
			result, err := displayNameCall(first, ctx, "builtin", "work.workCapabilities", map[string]any{"search": search})
			require.NoError(t, err)
			rows := memql.MaterializeRows(result)
			require.Len(t, rows, 1)
			matches, ok := rows[0]["capabilities"].([]any)
			require.True(t, ok)
			require.LessOrEqual(t, len(matches), 6, "natural discovery must remain a bounded shortlist")
			names := make([]string, 0, len(matches))
			for _, value := range matches {
				match := value.(map[string]any)
				names = append(names, match["name"].(string))
				if match["name"] == "identity.setUserDisplayName" {
					require.Equal(t, "builtin", match["kind"])
					require.Equal(t, arguments, match["arguments"], "the natural search must return the complete executable contract")
				}
			}
			require.Contains(t, names, "identity.setUserDisplayName", "a rename request must discover the write without knowing its exact name")
		})
	}

	_, err = displayNameCall(first, ctx, "builtin", "work.workExecute", map[string]any{
		"name": "identity.setUserDisplayName", "arguments": map[string]any{"userId": target, "displayName": "Updated display name"},
	})
	require.NoError(t, err)
	after := displayNameStoredPayload(t, db, target)
	require.Equal(t, "Updated display name", after["displayName"])
	after["displayName"] = before["displayName"]
	require.Equal(t, before, after, "a name change must preserve every other stored field")
	readback, err := displayNameCall(second, memql.ContextWithFreshRead(ctx), "query", "identity.userById", map[string]any{"userId": target})
	require.NoError(t, err)
	rows := memql.MaterializeRows(readback)
	require.Len(t, rows, 1)
	require.Equal(t, "Updated display name", rows[0]["displayName"])
	var audits int
	err = db.NewSelect().Table("MemoryNodes").ColumnExpr("count(*)").
		Where("concept = ?", "v1:identity:auditEvent").Where("payload->>'action' = ?", "user_display_name_changed").
		Where("payload->>'targetId' IN (?, ?)", target, "v1:identity:user:"+target).Scan(context.Background(), &audits)
	require.NoError(t, err)
	require.Equal(t, 1, audits)

	for _, role := range []auth.Role{"", auth.RoleReader, auth.RoleWriter, auth.RoleDeveloper} {
		t.Run("refuses_"+string(role), func(t *testing.T) {
			denied := context.Background()
			if role != "" {
				ac, _ := auth.AccessFromContext(ctx)
				denied = displayNameActor(ac.UserId, role)
			}
			_, err := displayNameCall(second, denied, "builtin", "work.workExecute", map[string]any{
				"name": "identity.setUserDisplayName", "arguments": map[string]any{"userId": target, "displayName": "Refused"},
			})
			require.Error(t, err)
			_, err = displayNameCall(second, denied, "builtin", "identity.setUserDisplayName", map[string]any{"userId": target, "displayName": "Refused"})
			require.Error(t, err)
		})
	}
	_, err = displayNameCall(first, ctx, "builtin", "identity.setUserDisplayName", map[string]any{
		"userId": target, "displayName": "Bad payload", "payload": map[string]any{"role": "owner"},
	})
	require.Error(t, err)
	require.Equal(t, "Updated display name", displayNameStoredPayload(t, db, target)["displayName"])
}

type displayNameInterleavingEngine struct {
	memql.IntegrationEngineAccess
	afterRead func()
}

func (e *displayNameInterleavingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	result, err := e.IntegrationEngineAccess.Execute(ctx, query)
	if err == nil && strings.HasPrefix(query, "query userByIdSystem(") && e.afterRead != nil {
		change := e.afterRead
		e.afterRead = nil
		change()
	}
	return result, err
}

func testIdentityDisplayNameRefusesConcurrentReplicaChangeThenUsesFreshVersion(t *testing.T, first, second *memql.MemQLEngine, db *bun.DB, access *displayNameInterleavingEngine) {
	t.Helper()
	t.Cleanup(func() { access.afterRead = nil })
	ctx, target := displayNameFixture(t, second, db)
	access.afterRead = func() {
		templateMutation(t, second, auth.ContextWithInternalOrigin(ctx), "identity.updateUser", map[string]any{
			"userId": target, "payload": map[string]any{"phone": "+15550000002", "preferences": map[string]any{"theme": "light"}},
		})
	}
	_, err := displayNameCall(first, ctx, "builtin", "identity.setUserDisplayName", map[string]any{"userId": target, "displayName": "New display"})
	require.ErrorContains(t, err, "the user changed")
	current := displayNameStoredPayload(t, db, target)
	require.Equal(t, "Original display", current["displayName"])
	require.Equal(t, "+15550000002", current["phone"])
	require.Equal(t, map[string]any{"theme": "light"}, current["preferences"])

	// The next call uses a fresh version on the first replica, even though its
	// earlier read was cached before the second replica changed the row.
	_, err = displayNameCall(first, ctx, "builtin", "identity.setUserDisplayName", map[string]any{"userId": target, "displayName": "New display"})
	require.NoError(t, err)
	after := displayNameStoredPayload(t, db, target)
	require.Equal(t, "New display", after["displayName"])
	after["displayName"] = current["displayName"]
	require.Equal(t, current, after)
}

func testIdentityDisplayNameSurvivesLegacyServiceReadBeforeRename(t *testing.T, first, second *memql.MemQLEngine, db *bun.DB) {
	t.Helper()
	ctx, target := displayNameFixture(t, first, db)
	before := displayNameStoredPayload(t, db, target)
	access := &displayNameInterleavingEngine{IntegrationEngineAccess: first}
	service, err := adminops.New(&adminops.Service{
		Engine: access, Logger: first.Logger,
		Audit: &identity.SlogAuditLogger{Logger: first.Logger, DB: &identity.EngineAuditSink{Engine: first, Logger: first.Logger}},
	})
	require.NoError(t, err)
	access.afterRead = func() {
		_, err := displayNameCall(second, ctx, "builtin", "identity.setUserDisplayName", map[string]any{
			"userId": target, "displayName": "Name saved by another replica",
		})
		require.NoError(t, err)
	}
	// The existing admin service reads a full profile first. Its payload must
	// retain that witness even when the name changes before updateUser starts.
	result := service.SetUserRole(ctx, target, "writer")
	require.False(t, result.OK)
	require.Contains(t, result.ErrorMessage, memql.ErrRowVersionChanged.Error())
	require.NotEmpty(t, result.AuditEventId)
	current := displayNameStoredPayload(t, db, target)
	require.Equal(t, "Name saved by another replica", current["displayName"])
	require.Equal(t, "developer", current["role"])
	current["displayName"] = before["displayName"]
	require.Equal(t, before, current, "the refused stale profile must not change any stored field")

	// A new decision reads the committed name through the first engine's
	// previously primed cache and can then change the role without reverting it.
	result = service.SetUserRole(ctx, target, "writer")
	require.True(t, result.OK, result.ErrorMessage)
	after := displayNameStoredPayload(t, db, target)
	require.Equal(t, "writer", after["role"])
	require.Equal(t, "Name saved by another replica", after["displayName"])
	for _, field := range []string{"primaryEmail", "phone", "preferences", "signInPolicy", "revocationEpoch"} {
		require.Equal(t, before[field], after[field], "preserve %s", field)
	}
}

// Hold the actual INSERT after its transaction has acquired the version fence
// and checked the witness. This exposes the commit window that a read-then-write
// interleaving alone cannot cover: every older profile writer must participate
// in the same PostgreSQL lock, even without an explicit caller-supplied fence.
type displayNameCommitHold struct {
	target  string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (h *displayNameCommitHold) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if event.Operation() == "INSERT" && strings.Contains(event.Query, `"MemoryNodes"`) &&
		strings.Contains(event.Query, h.target) && h.armed.CompareAndSwap(true, false) {
		close(h.entered)
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*displayNameCommitHold) AfterQuery(context.Context, *bun.QueryEvent) {}

func testIdentityDisplayNameSerializesLegacyWriterAcrossCommitWindow(t *testing.T, first, second *memql.MemQLEngine, db *bun.DB) {
	t.Helper()
	ctx, target := displayNameFixture(t, first, db)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	hold := &displayNameCommitHold{
		target: "v1:identity:user:" + target, entered: make(chan struct{}), release: make(chan struct{}),
	}
	db.AddQueryHook(hold)
	hold.armed.Store(true)
	var release sync.Once
	var writers sync.WaitGroup
	defer func() {
		release.Do(func() { close(hold.release) })
		cancel()
		writers.Wait()
	}()
	nameDone := make(chan error, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		_, err := displayNameCall(first, ctx, "builtin", "identity.setUserDisplayName", map[string]any{
			"userId": target, "displayName": "Committed display",
		})
		nameDone <- err
	}()
	select {
	case <-hold.entered:
	case <-ctx.Done():
		t.Fatal("display-name write did not reach its PostgreSQL commit window")
	}

	legacyArgs := map[string]any{
		"userId": target, "payload": map[string]any{"phone": "+15550000003", "preferences": map[string]any{"theme": "light"}},
	}
	legacyDone := make(chan error, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		// Deliberately no ContextWithRowVersionFence here: this is the existing
		// server-only writer used by profile and authentication handlers.
		_, err := displayNameCall(second, auth.ContextWithInternalOrigin(ctx), "mutation", "identity.updateUser", legacyArgs)
		legacyDone <- err
	}()

	// Assert the second database session really waits for this exact row lock;
	// timing or a still-running goroutine alone would not prove serialization.
	var blocked bool
	require.Eventually(t, func() bool {
		err := db.NewRaw(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory'
			AND classid = 1296387415::oid AND objid::bigint = (hashtext(?)::bigint & 4294967295)
			AND NOT granted)`, hold.target).Scan(ctx, &blocked)
		return err == nil && blocked
	}, 3*time.Second, 10*time.Millisecond, "legacy writer bypassed the identity row fence")
	release.Do(func() { close(hold.release) })
	select {
	case err := <-nameDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("display-name write did not finish")
	}
	select {
	case err := <-legacyDone:
		require.ErrorIs(t, err, memql.ErrRowVersionChanged)
	case <-ctx.Done():
		t.Fatal("legacy writer did not finish")
	}
	current := displayNameStoredPayload(t, db, target)
	require.Equal(t, "Committed display", current["displayName"])
	require.Equal(t, "+15550000001", current["phone"])

	// Once the caller re-reads/retries its narrow delta, the second engine
	// merges against the committed version and preserves both independent edits.
	_, err := displayNameCall(second, auth.ContextWithInternalOrigin(ctx), "mutation", "identity.updateUser", legacyArgs)
	require.NoError(t, err)
	after := displayNameStoredPayload(t, db, target)
	require.Equal(t, "Committed display", after["displayName"])
	require.Equal(t, "+15550000003", after["phone"])
	require.Equal(t, map[string]any{"theme": "light"}, after["preferences"])
	after["phone"], after["preferences"] = current["phone"], current["preferences"]
	require.Equal(t, current, after)
}

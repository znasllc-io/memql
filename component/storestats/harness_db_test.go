package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// harness_db_test.go -- the store statistics against a real engine and a real
// database, the harness every database test of this package uses.
//
// The stores are written by the store mutation the connector and Connect
// Shopify write with, and read back through the engine the way the scheduled
// run reads them: through the @serverOnly store statistics reads, under the
// automation's maintenance principal. A fake engine would assert its own rows;
// what is worth proving is that the engine's own paging, its id spellings and
// its locks give what the walk says.
//
// THE DATABASE IS SHARED, so every assertion is about the stores its own test
// made (each newStore is a store of its own, named by a fresh suffix). A run
// lists every store in the database, other tests' included, and gives each a
// row; nothing here asserts about those.
//
// WHAT THE READS GUARANTEE, which every consumer of them relies on: a walk's
// page is ordered by write time, ties broken by id; a row rewritten while a
// walk is under way comes back on a later page, so a consumer dedupes by id;
// `since` is inclusive; a version that commits late can land behind a
// watermark already passed, so a consumer reads with an overlap; and the
// pack's reads refuse an empty list of store ids while a blank member also
// matches rows with no store id, so neither is ever passed.
//
// Postgres-gated. CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1,
// so a skip there is a failure rather than a green.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// statsEngine builds one engine over the real database per process, with this
// package's provider registered on it, or skips.
//
// ONE BOOT PER PROCESS, the email statistics harness's rule: an engine boot
// loads the whole DSL tree. The Once records its own failure so every caller
// reports it rather than the first one swallowing it.
var statsEngineState struct {
	once    sync.Once
	eng     *memql.MemQLEngine
	db      *bun.DB
	pingErr error
	bootErr error
	// integ is the provider registered on eng, the one `builtin
	// storeStatistics` dispatches to. A test that needs the read's clock sets
	// its `now` through atNow.
	integ *Integration
}

func statsEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	s := &statsEngineState
	s.once.Do(func() {
		if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
			s.pingErr = err
			return
		}
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		if err := db.PingContext(context.Background()); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		// The one hook storeSalesQueries records through: bun has no way to
		// take a hook off a handle, so it is added once and a test registers
		// a recorder with it for as long as it runs.
		db.AddQueryHook(statementRecorders)
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			_ = db.Close()
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("New: %w", err)
			_ = db.Close()
			return
		}
		eng.Logger = quietLogger()
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("Init: %w", err)
			_ = db.Close()
			return
		}
		// GRANTS ARE READ ONLY THROUGH AN INSTALLED SOURCE, which an engine
		// that never Starts must install itself, as the reviews pack's and
		// the engine's own database tests do: without it no group grant
		// reaches the sales part, and every member reads as holding nothing.
		eng.InstallGrantResolution()
		// The provider as a node registers it, so `builtin
		// storeStatisticsRollup()` dispatches here exactly as the scheduled
		// automation's run does.
		s.integ = NewIntegration(eng, func() *bun.DB { return db }, quietLogger())
		if err := eng.RegisterIntegration(s.integ); err != nil {
			s.bootErr = fmt.Errorf("RegisterIntegration: %w", err)
			_ = db.Close()
			return
		}
		s.eng, s.db = eng, db
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "the store statistics", dbtest.DSN(), s.pingErr)
		return nil, nil
	}
	if s.bootErr != nil {
		t.Fatalf("the statistics engine failed to boot: %v", s.bootErr)
	}
	return s.eng, s.db
}

// testIntegration is an Integration over the shared engine and database whose
// seams a test sets itself. The registered provider keeps its own.
func testIntegration(t *testing.T) *Integration {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	return NewIntegration(eng, func() *bun.DB { return db }, quietLogger())
}

// principalCtx is the context a scheduled automation runs its builtin under:
// the cluster's maintenance principal for the automation named, with the
// claims and token component/automations stamps beside it. Internal origin is
// the caller's to add, as the automation runner adds it.
func principalCtx(t *testing.T, automation string) context.Context {
	t.Helper()
	actor := auth.MaintenanceActor(automation)
	if actor == nil {
		t.Fatalf("%s is not on the maintenance list, so its automation runs as a reader and every run is refused", automation)
	}
	claims := map[string]any{"sub": actor.UserId, "email": actor.UserId, "role": string(actor.Role)}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, actor)
}

// ownerCtx is a person who is a cluster owner, signed in.
func ownerCtx() context.Context {
	claims := map[string]any{"sub": "dbtest-storestats-owner", "role": "owner"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "dbtest-storestats-owner", Role: auth.RoleOwner})
}

// newStore writes a store of the test's own through createStore, as its owner,
// and answers its bare id. The store's graph rows and its store_sales_store
// row are deleted when the test ends.
func newStore(t *testing.T) string {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	storeID := fmt.Sprintf("dbtest-storestats-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM store_sales_store WHERE store_id = ?`, storeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = ?`, "v1:shopify:store:"+storeID)
	})
	call := fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s, name: %s, adminTokenRef: "test-admin", apiVersion: "2026-07")`,
		langparser.QuoteString(storeID), langparser.QuoteString(storeID+".myshopify.com"), langparser.QuoteString("Store "+storeID))
	if _, err := eng.Execute(ownerCtx(), call); err != nil {
		t.Fatalf("write the store %s: %v", storeID, err)
	}
	return storeID
}

// storeRow is the store's store_sales_store row, or nil when it has none.
func storeRow(t *testing.T, storeID string) map[string]any {
	t.Helper()
	_, db := statsEngine(t)
	row := map[string]any{}
	err := db.NewSelect().Table("store_sales_store").Where("store_id = ?", storeID).Scan(context.Background(), &row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the store_sales_store row of %s: %v", storeID, err)
	}
	return row
}

// storeSalesQueries records, from now until the test ends, the statements the
// shared handle runs that name a store_sales_ table, and answers a function
// that hands back the ones recorded since it was last called.
func storeSalesQueries(t *testing.T) func() []string {
	t.Helper()
	statsEngine(t)
	r := &statementRecorder{}
	statementRecorders.add(r)
	t.Cleanup(func() { statementRecorders.remove(r) })
	return r.take
}

// statementRecorders is the one hook on the shared handle; it hands each
// statement to the recorders registered at the time.
var statementRecorders = &recorderHook{}

type recorderHook struct {
	mu        sync.Mutex
	recorders []*statementRecorder
}

func (h *recorderHook) add(r *statementRecorder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recorders = append(h.recorders, r)
}

func (h *recorderHook) remove(r *statementRecorder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, x := range h.recorders {
		if x == r {
			h.recorders = append(h.recorders[:i], h.recorders[i+1:]...)
			return
		}
	}
}

func (h *recorderHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *recorderHook) AfterQuery(_ context.Context, ev *bun.QueryEvent) {
	if !strings.Contains(ev.Query, "store_sales_") {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recorders {
		r.record(ev.Query)
	}
}

type statementRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *statementRecorder) record(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, q)
}

func (r *statementRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.seen
	r.seen = nil
	return out
}

// ---- the read's callers, sites and frames (Task 17) ----

// seedCtx is the engine's own seeding identity, as the reviews pack's and the
// account tests use it: internal origin and a synthetic owner, so a fixture
// row is written without standing in for anyone.
func seedCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "dbtest-storestats-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "dbtest-storestats-seeder"})
}

// seedRow writes one row through the engine's raw insert as the seeder, and
// deletes it when the test ends.
func seedRow(t *testing.T, concept, id string, payload map[string]any) {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode %s %s: %v", concept, id, err)
	}
	call := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(id), string(raw))
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("seed %s %s: %v", concept, id, err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = ? AND id = ?`, concept, concept+":"+id)
	})
}

// seedCall runs one named call as the seeder.
func seedCall(t *testing.T, call string) {
	t.Helper()
	eng, _ := statsEngine(t)
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("%s: %v", call, err)
	}
}

func q(s string) string { return langparser.QuoteString(s) }

// uniq is a suffix no other test of this run shares.
func uniq() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

// siteAccounts is the organization siteFor put each site in.
var siteAccounts sync.Map

// siteFor writes a live shopify_storefront site bound to the store, in an
// organization of its own (its account row and its account group), and
// answers the site's bare id. over replaces fields of the site's row; a key
// whose value is nil is left out of it, so {"accountId": nil} is a site no
// organization owns. The store's row is written first, as a storefront may
// be bound only to a store that exists.
func siteFor(t *testing.T, storeID string, over map[string]any) string {
	t.Helper()
	n := uniq()
	siteID := "dbtest-storestats-site-" + n
	account := "dbtest-storestats-org-" + n
	if storeID != "" {
		bare := memql.BareShortId(storeID)
		seedRow(t, "v1:shopify:store", bare, map[string]any{"domain": bare + ".myshopify.com"})
	}
	seedOrganization(t, account)
	site := map[string]any{
		"ownerUserId": "dbtest-storestats-site-owner-" + n, "title": "A city bike shop", "hostname": siteID + ".example.test",
		"status": "live", "kind": "shopify_storefront", "bundleRef": "blob://sites/" + siteID + "/v1/",
		"binding": map[string]any{"storeId": storeID}, "accountId": account,
	}
	for k, v := range over {
		if v == nil {
			delete(site, k)
			continue
		}
		site[k] = v
	}
	seedRow(t, "v1:platform:site", siteID, site)
	acct, _ := site["accountId"].(string)
	siteAccounts.Store(siteID, acct)
	return siteID
}

// seedOrganization writes an active organization and its account group, at
// the id integrations/groups derives (acct-<account>), and answers the
// group's id.
func seedOrganization(t *testing.T, account string) string {
	t.Helper()
	seedRow(t, "v1:accounts:account", account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
	group := "acct-" + account
	seedGroup(t, group, "account", account)
	return group
}

func seedGroup(t *testing.T, groupID, kind, account string) {
	t.Helper()
	seedCall(t, `mutation writeGroup(groupId: `+q(groupID)+`, name: `+q(groupID)+`, kind: `+q(kind)+
		`, accountId: `+q(account)+`, status: "active")`)
}

// grantGroup gives a group one allow grant, at the id the grant writer derives.
func grantGroup(t *testing.T, groupID, verb, resource string) {
	t.Helper()
	seedCall(t, `writeGrant(grantId: `+q(auth.GrantRowID(auth.SubjectKindGroup, groupID, verb, resource))+
		`, subjectKind: "group", subjectId: `+q(groupID)+`, verb: `+q(verb)+`, resourceType: `+q(resource)+
		`, effect: "allow", grantedBy: "dbtest-storestats-seeder")`)
}

// personCtx is a signed-in person as a deployed cluster spells them: the
// canonical id their token carries, and the role named.
func personCtx(userID string, role auth.Role) context.Context {
	canonical := "v1:identity:user:" + userID
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: canonical, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: canonical})
}

// memberCtx is a person of the site's organization who is not a cluster
// owner: a user row on the member rung (writer, its compiled name, since this
// engine installs no role catalog), in a group of the organization of their
// own that opens Deployables (read app:deployables) and, when withPart, holds
// the sales part (execute app:deployables/sales). Answered signed in.
func memberCtx(t *testing.T, siteID string, withPart bool) context.Context {
	t.Helper()
	v, ok := siteAccounts.Load(siteID)
	account, _ := v.(string)
	if !ok || account == "" {
		t.Fatalf("memberCtx: site %s was not made by siteFor in an organization", siteID)
	}
	n := uniq()
	userID := "dbtest-storestats-member-" + n
	group := "dbtest-storestats-group-" + n
	seedGroup(t, group, "custom", account)
	grantGroup(t, group, auth.VerbRead, "app:deployables")
	if withPart {
		grantGroup(t, group, auth.VerbExecute, "app:deployables/sales")
	}
	seedRow(t, "v1:identity:user", userID, map[string]any{"displayName": "A member", "primaryEmail": userID + "@example.test",
		"role": string(auth.RoleWriter), "active": true})
	seedCall(t, `mutation writeGroupMembership(membershipId: `+q(group+"-"+userID)+`, groupId: `+q(group)+
		`, userId: `+q(userID)+`, origin: "added", status: "active", accountId: `+q(account)+`)`)
	return personCtx(userID, auth.RoleWriter)
}

// storeFrame writes the store's store_sales_store row with the columns given
// (the migration's names), replacing any row it had, and deletes it when the
// test ends. verdict_reasons may be given as a Go value; it is stored as JSON.
func storeFrame(t *testing.T, storeID string, columns map[string]any) {
	t.Helper()
	_, db := statsEngine(t)
	if db == nil {
		t.FailNow()
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM store_sales_store WHERE store_id = ?`, storeID); err != nil {
		t.Fatalf("clear the frame of %s: %v", storeID, err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM store_sales_store WHERE store_id = ?`, storeID)
	})
	names := []string{"store_id"}
	marks := []string{"?"}
	values := []any{storeID}
	keys := make([]string, 0, len(columns))
	for k := range columns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := columns[k]
		mark := "?"
		if k == "verdict_reasons" {
			if _, isText := v.(string); !isText {
				raw, err := json.Marshal(v)
				if err != nil {
					t.Fatalf("encode verdict_reasons: %v", err)
				}
				v = string(raw)
			}
			mark = "?::jsonb"
		}
		names = append(names, k)
		marks = append(marks, mark)
		values = append(values, v)
	}
	stmt := "INSERT INTO store_sales_store (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	if _, err := db.ExecContext(ctx, stmt, values...); err != nil {
		t.Fatalf("write the frame of %s: %v", storeID, err)
	}
}

// atNow sets the registered read's clock for the rest of the test.
func atNow(t *testing.T, now time.Time) {
	t.Helper()
	statsEngine(t)
	integ := statsEngineState.integ
	integ.now = func() time.Time { return now }
	t.Cleanup(func() { integ.now = nil })
}

// askStatistics calls the builtin through the engine and answers the one
// row's payload, or the engine's error.
func askStatistics(t *testing.T, ctx context.Context, siteID, storeID, period string) (map[string]any, error) {
	t.Helper()
	eng, _ := statsEngine(t)
	return askOn(t, eng, ctx, `builtin storeStatistics(siteId: `+q(siteID)+`, storeId: `+q(storeID)+`, period: `+q(period)+`)`)
}

func askOn(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, call string) (map[string]any, error) {
	t.Helper()
	res, err := eng.Execute(ctx, call)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		raw, _ := json.Marshal(rows)
		t.Fatalf("%s answered %d rows, want 1: %s", call, len(rows), raw)
	}
	if p, ok := rows[0]["payload"].(map[string]any); ok {
		return p, nil
	}
	return rows[0], nil
}

// mustAsk is askStatistics for a call that must be answered.
func mustAsk(t *testing.T, ctx context.Context, siteID, storeID, period string) map[string]any {
	t.Helper()
	answer, err := askStatistics(t, ctx, siteID, storeID, period)
	if err != nil {
		t.Fatalf("storeStatistics(%s, %s, %s): %v", siteID, storeID, period, err)
	}
	return answer
}

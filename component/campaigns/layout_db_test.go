package campaigns

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
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

// layout_db_test.go -- organization layouts against real engines: the parts
// layout_test.go's fake cannot say. Two engines share one database, as two
// replicas do, and each has its own worker, so the exact-revision check and
// the write gate are exercised across them.

// layoutWorld is two replicas of the engine over one database, an owner who can
// create organizations, and the identity that seeds scaffolding.
type layoutWorld struct {
	db      *bun.DB
	engines [2]*memql.MemQLEngine
	workers [2]*Worker
	suffix  string
	system  context.Context
	owner   context.Context
	ownerID string
}

func layoutActor(user string, role auth.Role) context.Context {
	return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role}), &auth.TokenInfo{Subject: user})
}

func newLayoutWorld(t *testing.T) *layoutWorld {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "organization layouts", dbtest.DSN(), err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	w := &layoutWorld{db: db, suffix: fmt.Sprintf("layout-%d", time.Now().UnixNano())}
	// Registered after the connection's own cleanup, so it runs while the
	// connection is still open.
	t.Cleanup(func() { removeLayoutRows(t, db, w.suffix) })
	for n := range w.engines {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = quietLogger()
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		w.engines[n] = e
		w.workers[n] = NewWorker(deliveryDBEngine{e}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
		// Registered as the integration, so a builtin call runs the whole way:
		// the DSL declaration, the executor name and the handler.
		if err := e.RegisterIntegration(w.workers[n]); err != nil {
			t.Fatal(err)
		}
	}
	// A replica whose clock lags must still write a revision behind nothing.
	w.workers[0].now = func() time.Time { return time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC) }
	w.workers[1].now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	w.system = auth.ContextWithInternalOrigin(auth.ContextWithToken(
		auth.ContextWithAccess(context.Background(), auth.SystemActor("layout-tests")), &auth.TokenInfo{Subject: "layout-tests"}))
	w.ownerID = w.suffix + "-owner"
	w.owner = layoutActor(w.ownerID, auth.RoleOwner)
	w.seedUser(t, w.ownerID, "owner")
	return w
}

// layoutSuffixPattern is the shape of a run's suffix: "layout-" and the clock's
// nanoseconds, nineteen digits until the year 2286. removeLayoutRows refuses
// anything else.
var layoutSuffixPattern = regexp.MustCompile(`^layout-[0-9]{19}$`)

// removeLayoutRows deletes what one run of these tests left in the database:
// the users, organizations, groups, memberships and layouts it made, and the
// rows the engine and other processes made about them (a seeded agent, a
// notification addressed to one of its users). All of them carry the run's
// suffix, in their id or in their payload, and nothing else does: the suffix is
// the clock's nanoseconds, so two runs never share one.
//
// The database is shared with other test binaries, and nothing here may touch
// their rows. So the statement is a plain substring match on the suffix, and a
// suffix that is not exactly the shape a run makes is refused: an empty one
// would be contained in every id there is.
func removeLayoutRows(t testing.TB, db *bun.DB, suffix string) {
	t.Helper()
	if !layoutSuffixPattern.MatchString(suffix) {
		t.Errorf("refusing to clean up after %q: it is not the suffix of a layout test run", suffix)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE strpos(id, ?) > 0 OR strpos(payload::text, ?) > 0`, suffix, suffix)
	if err != nil {
		t.Errorf("removing the rows of %s: %v", suffix, err)
		return
	}
	removed, _ := res.RowsAffected()
	t.Logf("removed the %d rows of %s", removed, suffix)
}

// quietTB is a test that records what is reported to it instead of failing.
type quietTB struct {
	testing.TB
	errors []string
}

func (q *quietTB) Errorf(format string, args ...any) {
	q.errors = append(q.errors, fmt.Sprintf(format, args...))
}
func (q *quietTB) Logf(string, ...any) {}
func (q *quietTB) Helper()             {}

// The statement that cleans up is a substring match on a shared database, so
// it must refuse anything that is not a run's suffix: an empty one is inside
// every id. It is refused before the database is touched (the database here is
// nil), so this needs no Postgres.
func TestRemoveLayoutRowsRefusesAnythingButARunsSuffix(t *testing.T) {
	for _, suffix := range []string{
		"", "layout-", "layout", "layout-1", "layout-123456789", "x-1791791261136112000",
		"layout-1791791261136112000-owner", "layout-1791791261136112000%", "layout-1791791261136112000' OR 'a'='a",
		" layout-1791791261136112000", "LAYOUT-1791791261136112000",
		// Not the nineteen digits of the clock: a shorter or a longer number is
		// not a run's.
		"layout-1234567890", "layout-179179126113611200", "layout-17917912611361120000",
	} {
		q := &quietTB{TB: t}
		removeLayoutRows(q, nil, suffix)
		if len(q.errors) != 1 || !strings.Contains(q.errors[0], "refusing to clean up") {
			t.Errorf("suffix %q: reported %v, want one refusal", suffix, q.errors)
		}
	}
	for _, suffix := range []string{"layout-1791791261136112000", "layout-1000000000000000000", "layout-9223372036854775807"} {
		if !layoutSuffixPattern.MatchString(suffix) {
			t.Errorf("%q is the shape of a run's suffix and was refused", suffix)
		}
	}
}

func (w *layoutWorld) run(t *testing.T, ctx context.Context, q string) any {
	t.Helper()
	res, err := w.engines[0].Execute(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", firstWords(q), err)
	}
	return res
}

func (w *layoutWorld) seedUser(t *testing.T, user, role string) {
	t.Helper()
	w.run(t, w.system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Layout test","role":%s,"active":true})`,
		langparser.QuoteString(user), langparser.QuoteString(user+"@example.test"), langparser.QuoteString(role)))
}

func (w *layoutWorld) createOrganization(t *testing.T, account, name string) {
	t.Helper()
	w.run(t, w.owner, call("mutation", "createClientAccount", arg{"accountId", account}, arg{"name", name}))
}

// seedMember makes user a member of the organization through the group the
// engine reads membership from. The two writers are @serverOnly, so the
// scaffolding identity is a synthetic cluster owner with internal origin.
func (w *layoutWorld) seedMember(t *testing.T, user, account string) {
	t.Helper()
	seeder := auth.ContextWithToken(auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "layout-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true})), &auth.TokenInfo{Subject: "layout-seeder"})
	group := "g-" + account
	if _, err := w.engines[0].Execute(seeder, call("mutation", "writeGroup", arg{"groupId", group}, arg{"name", account}, arg{"kind", "account"}, arg{"accountId", account}, arg{"status", "active"})); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := w.engines[0].Execute(seeder, call("mutation", "writeGroupMembership", arg{"membershipId", group + "-" + user}, arg{"groupId", group},
		arg{"userId", user}, arg{"origin", "added"}, arg{"status", "active"}, arg{"accountId", account})); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// layoutArgs is a save call for the world's layout.
func layoutArgs(t *testing.T, layout, account, revision, action string, mutate ...func(map[string]any)) map[string]any {
	t.Helper()
	args := map[string]any{
		"layoutId": layout, "accountId": account, "name": "Acme letter", "description": "A personal note.",
		"document": layoutDocument(t, "builtin:letter"), "expectedRevision": revision, "action": action,
	}
	for _, m := range mutate {
		m(args)
	}
	return args
}

// save runs the handler on one replica and returns the receipt's revision and status.
func saveOn(t *testing.T, w *Worker, ctx context.Context, args map[string]any) (revision, status string) {
	t.Helper()
	nodes, err := w.handleSaveLayout(ctx, args, 0)
	if err != nil {
		t.Fatalf("save %v: %v", args["action"], err)
	}
	if len(nodes) != 1 {
		t.Fatalf("receipt: %v", nodes)
	}
	var receipt map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &receipt); err != nil {
		t.Fatal(err)
	}
	return str(receipt, "revision"), str(receipt, "status")
}

// layoutIDs lists what a caller is offered, reading from the database: each
// engine here keeps its own result cache and nothing carries one's evictions to
// the other, so a cached list would make these assertions about the cache.
func layoutIDs(t *testing.T, w *Worker, ctx context.Context, account string, includeArchived bool) []string {
	t.Helper()
	nodes, err := w.handleEmailLayouts(memql.ContextWithFreshRead(ctx), map[string]any{"accountId": account, "includeArchived": includeArchived}, 0)
	if err != nil {
		t.Fatalf("emailLayouts: %v", err)
	}
	var ids []string
	for _, row := range nodePayloads(t, nodes) {
		ids = append(ids, str(row, "id"))
	}
	return ids
}

func TestLayoutRevisionGateArchiveAndRestore(t *testing.T) {
	w := newLayoutWorld(t)
	account, other, layout := w.suffix+"-org", w.suffix+"-other-org", w.suffix+"-layout"
	w.createOrganization(t, account, "Layout client")
	w.createOrganization(t, other, "Another client")
	fresh := func(ctx context.Context) context.Context { return memql.ContextWithFreshRead(ctx) }
	read := func(r int) map[string]any {
		t.Helper()
		row, found, err := w.workers[r].store.EmailLayoutByID(fresh(w.owner), layout)
		if err != nil || !found {
			t.Fatalf("read on replica %d: %v %v", r, found, err)
		}
		return row
	}

	// Create on one replica; the same call retried on the other (an uncertain
	// response is retried) is the same revision, not a second version.
	first, status := saveOn(t, w.workers[0], w.owner, layoutArgs(t, layout, account, "", "save"))
	if first == "" || status != "active" {
		t.Fatalf("created at %q %q", first, status)
	}
	if retry, _ := saveOn(t, w.workers[1], w.owner, layoutArgs(t, layout, account, "", "save")); retry != first {
		t.Fatalf("a creation retry on the other replica wrote another version: %s then %s", first, retry)
	}

	// A client cannot write the row any other way: a raw insert meets the
	// internal-origin guard, and the mutation is server-only.
	_, err := w.engines[0].Execute(w.owner, fmt.Sprintf(`insert("v1:campaigns:emailLayout", id=%s, payload={"name":"Bypass","accountId":%s,"status":"active"})`, langparser.QuoteString(layout), langparser.QuoteString(account)))
	if err == nil || !strings.Contains(err.Error(), "layout writes require campaignSaveLayout") {
		t.Fatalf("a raw insert was not refused by the layout guard: %v", err)
	}
	_, err = w.engines[0].Execute(w.owner, fmt.Sprintf(`mutation updateEmailLayout(layoutId: %s, name: "Bypass", status: "active", versionTime: "2031-01-01T00:00:00Z")`, langparser.QuoteString(layout)))
	if err == nil || !strings.Contains(err.Error(), "server-only") {
		t.Fatalf("a client called the server-only mutation: %v", err)
	}
	if got := read(0); str(got, "name") != "Acme letter" {
		t.Fatalf("a refused write changed the row: %v", got)
	}

	// Two replicas saving from the same revision: exactly one wins.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n, worker := range w.workers {
		args := layoutArgs(t, layout, account, first, "save", func(a map[string]any) { a["name"] = fmt.Sprintf("Replica %d", n) })
		wg.Add(1)
		go func(worker *Worker, args map[string]any) {
			defer wg.Done()
			_, err := worker.handleSaveLayout(w.owner, args, 0)
			results <- err
		}(worker, args)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case strings.Contains(err.Error(), "changed"):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("got %d successes and %d conflicts", successes, conflicts)
	}
	row := read(1)
	current := templateTime(row["createdAt"]).Format(time.RFC3339Nano)
	if !templateTime(row["createdAt"]).After(templateTime(first)) {
		t.Fatal("a lagging replica wrote behind its own read")
	}

	// A stale revision is refused with "changed".
	if _, err := w.workers[0].handleSaveLayout(w.owner, layoutArgs(t, layout, account, first, "save", func(a map[string]any) { a["name"] = "Stale" }), 0); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a stale revision: %v", err)
	}
	// A layout already saved on one replica is listed on the other, built-ins first.
	wantList := []string{"builtin:confirmation", "builtin:letter", "builtin:article", "builtin:spotlight", "org:" + layout}
	if ids := layoutIDs(t, w.workers[1], w.owner, account, false); strings.Join(ids, ",") != strings.Join(wantList, ",") {
		t.Fatalf("replica 1 lists %v", ids)
	}

	// Another organization of the same owner is a different place: the layout
	// cannot be saved over there.
	if _, err := w.workers[0].handleSaveLayout(w.owner, layoutArgs(t, layout, other, current, "save"), 0); err == nil || !strings.Contains(err.Error(), "a layout cannot move between organizations") {
		t.Fatalf("a layout moved between organizations: %v", err)
	}
	if _, err := w.workers[0].handleSaveLayout(w.owner, layoutArgs(t, layout, "missing-organization", current, "save"), 0); err == nil {
		t.Fatal("a layout was saved for an organization that does not exist")
	}

	// Archive with the current revision: the picker stops offering it, but
	// the archived layout is still there for those who ask.
	archivedAt, status := saveOn(t, w.workers[1], w.owner, map[string]any{"layoutId": layout, "accountId": account, "expectedRevision": current, "action": "archive"})
	if status != "archived" || archivedAt == current {
		t.Fatalf("archive gave %q at %q", status, archivedAt)
	}
	if ids := layoutIDs(t, w.workers[0], w.owner, account, false); strings.Join(ids, ",") != strings.Join(wantList[:4], ",") {
		t.Fatalf("an archived layout is still offered: %v", ids)
	}
	if ids := layoutIDs(t, w.workers[0], w.owner, account, true); len(ids) != 5 || ids[4] != "org:"+layout {
		t.Fatalf("includeArchived lacks the archived layout: %v", ids)
	}
	if got := read(0); str(got, "status") != "archived" || str(got, "name") == "" || got["document"] == nil {
		t.Fatalf("archiving lost the layout's content: %v", got)
	}
	// An archive that already landed is not an error to repeat.
	if again, status := saveOn(t, w.workers[0], w.owner, map[string]any{"layoutId": layout, "accountId": account, "expectedRevision": current, "action": "archive"}); again != archivedAt || status != "archived" {
		t.Fatalf("a repeated archive gave %q at %q, not the receipt it had", status, again)
	}

	// An archived layout is not edited; it is restored first.
	if _, err := w.workers[0].handleSaveLayout(w.owner, layoutArgs(t, layout, account, archivedAt, "save", func(a map[string]any) { a["name"] = "Edited while archived" }), 0); err == nil || !strings.Contains(err.Error(), "restore this layout before editing it") {
		t.Fatalf("an archived layout was edited: %v", err)
	}
	restoredAt, status := saveOn(t, w.workers[0], w.owner, map[string]any{"layoutId": layout, "accountId": account, "expectedRevision": archivedAt, "action": "restore"})
	if status != "active" || restoredAt == archivedAt {
		t.Fatalf("restore gave %q at %q", status, restoredAt)
	}
	if ids := layoutIDs(t, w.workers[1], w.owner, account, false); len(ids) != 5 {
		t.Fatalf("a restored layout is not offered again: %v", ids)
	}
	editedAt, _ := saveOn(t, w.workers[1], w.owner, layoutArgs(t, layout, account, restoredAt, "save", func(a map[string]any) { a["name"] = "Acme letter, restored" }))
	if editedAt == restoredAt || str(read(0), "name") != "Acme letter, restored" {
		t.Fatalf("a restored layout cannot be edited: %v", read(0))
	}
	// Nothing saved under this id is archived.
	if _, err := w.workers[0].handleSaveLayout(w.owner, map[string]any{"layoutId": layout + "-never", "accountId": account, "expectedRevision": editedAt, "action": "archive"}, 0); err == nil || !strings.Contains(err.Error(), "layout not found") {
		t.Fatalf("archiving what was never saved: %v", err)
	}

	// The builtins run the whole way through the engine, and the list keeps
	// the order a node map would lose: built-ins first, then the organization's.
	w.checkBuiltinsThroughTheEngine(t, account, layout+"-through-the-engine", wantList[:4])
}

// checkBuiltinsThroughTheEngine calls campaignSaveLayout and emailLayouts as a
// client does, as DSL builtin calls, so a mismatch between the declaration and
// the executor, or a capability that forgot PreserveOrder, fails here.
func (w *layoutWorld) checkBuiltinsThroughTheEngine(t *testing.T, account, layout string, builtins []string) {
	t.Helper()
	save, err := langparser.RenderCall("campaignSaveLayout", map[string]any{
		"layoutId": layout, "accountId": account, "name": "Through the engine", "description": "Saved by a builtin call.",
		"document": layoutDocument(t, "builtin:spotlight"), "expectedRevision": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(w.run(t, w.owner, "builtin "+save))
	if len(rows) != 1 || rows[0]["layoutId"] != layout || rows[0]["status"] != "active" || rows[0]["saved"] != true {
		t.Fatalf("campaignSaveLayout answered %v", rows)
	}

	list, err := langparser.RenderCall("emailLayouts", map[string]any{"accountId": account})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := w.run(t, w.owner, "builtin "+list).(*memql.ExecuteResult)
	if !ok {
		t.Fatal("emailLayouts did not answer an engine result")
	}
	nodes, ok := result.OutputPayload().(map[string]memorynodes.MemoryNode)
	if !ok {
		t.Fatalf("emailLayouts answered %T", result.OutputPayload())
	}
	// A client reads a builtin's nodes newest first: that is how the order a
	// handler gave survives the id-keyed map.
	ordered := make([]memorynodes.MemoryNode, 0, len(nodes))
	for _, n := range nodes {
		ordered = append(ordered, n)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].CreatedAt.After(ordered[j].CreatedAt) })
	var ids []string
	for _, n := range ordered {
		ids = append(ids, n.ID)
	}
	if len(ids) < 6 || strings.Join(ids[:4], ",") != strings.Join(builtins, ",") {
		t.Fatalf("the engine delivered %v; the built-ins must come first, in catalogue order", ids)
	}
	if !strings.Contains(strings.Join(ids[4:], ","), "org:"+layout) {
		t.Fatalf("the layout saved by the builtin is not listed: %v", ids)
	}
}

// A person outside an organization cannot read, list, save over or archive its
// layouts, even a person who belongs to a different organization.
func TestLayoutStaysInItsOrganization(t *testing.T) {
	w := newLayoutWorld(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.engines[0].InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	victim, rivalOrg, layout := w.suffix+"-victim", w.suffix+"-rival", w.suffix+"-layout"
	w.createOrganization(t, victim, "Victim client")
	w.createOrganization(t, rivalOrg, "Rival client")
	people := map[string]string{"colleague": "writer", "viewer": "reader", "rival": "writer", "stranger": "writer"}
	for name, role := range people {
		w.seedUser(t, w.suffix+"-"+name, role)
	}
	w.seedMember(t, w.suffix+"-colleague", victim)
	w.seedMember(t, w.suffix+"-viewer", victim)
	w.seedMember(t, w.suffix+"-rival", rivalOrg)
	colleague := layoutActor(w.suffix+"-colleague", auth.RoleWriter)
	viewer := layoutActor(w.suffix+"-viewer", auth.RoleReader)
	rival := layoutActor(w.suffix+"-rival", auth.RoleWriter)
	stranger := layoutActor(w.suffix+"-stranger", auth.RoleWriter)

	saved, _ := saveOn(t, w.workers[0], w.owner, layoutArgs(t, layout, victim, "", "save"))
	fresh := func(ctx context.Context) context.Context { return memql.ContextWithFreshRead(ctx) }
	intact := func(step string) {
		t.Helper()
		row, found, err := w.workers[0].store.EmailLayoutByID(fresh(w.owner), layout)
		if err != nil || !found {
			t.Fatalf("%s: the layout cannot be read back: %v %v", step, found, err)
		}
		if templateTime(row["createdAt"]).Format(time.RFC3339Nano) != saved || str(row, "name") != "Acme letter" ||
			bare(str(row, "accountId")) != victim || str(row, "status") != "active" {
			t.Fatalf("%s changed the layout: %v", step, row)
		}
	}

	// The people who belong see it. A reader lists but does not write.
	for name, ctx := range map[string]context.Context{"colleague": colleague, "viewer": viewer} {
		ids := layoutIDs(t, w.workers[1], ctx, victim, false)
		if len(ids) != 5 || ids[4] != "org:"+layout {
			t.Fatalf("%s lists %v", name, ids)
		}
		row, found, err := w.workers[1].store.EmailLayoutByID(fresh(ctx), layout)
		if err != nil || !found || bare(str(row, "accountId")) != victim {
			t.Fatalf("%s cannot read the layout: %v %v %v", name, row, found, err)
		}
	}
	if _, err := w.workers[1].handleSaveLayout(viewer, layoutArgs(t, layout, victim, saved, "save", func(a map[string]any) { a["name"] = "Reader edit" }), 0); err == nil || !strings.Contains(err.Error(), "layout write permission is required") {
		t.Fatalf("a reader edited a layout: %v", err)
	}
	intact("a reader's edit")

	// A colleague edits it; the person who first saved it stays its owner.
	editedAt, _ := saveOn(t, w.workers[1], colleague, layoutArgs(t, layout, victim, saved, "save", func(a map[string]any) { a["description"] = "Edited by a colleague." }))
	row, _, err := w.workers[0].store.EmailLayoutByID(fresh(w.owner), layout)
	if err != nil || bare(str(row, "ownerUserId")) != w.ownerID || str(row, "description") != "Edited by a colleague." {
		t.Fatalf("a colleague's edit changed the owner or was lost: %v %v", row, err)
	}
	saved = editedAt
	intact("a colleague's edit")

	// People outside see nothing, and can do nothing.
	for name, ctx := range map[string]context.Context{"a member of another organization": rival, "a stranger": stranger} {
		if _, err := w.workers[1].handleEmailLayouts(fresh(ctx), map[string]any{"accountId": victim}, 0); err == nil {
			t.Errorf("%s listed the organization's layouts", name)
		}
		if row, found, err := w.workers[1].store.EmailLayoutByID(fresh(ctx), layout); err == nil && found {
			t.Errorf("%s read the layout by id: %v", name, row)
		}
		if rows, err := w.workers[1].store.EmailLayoutsForAccount(fresh(ctx), victim, true); err == nil && len(rows) != 0 {
			t.Errorf("%s read the organization's layouts: %v", name, rows)
		}
		// Saving over it, naming its organization or any other, and archiving it.
		attempts := []map[string]any{
			layoutArgs(t, layout, victim, saved, "save", func(a map[string]any) { a["name"] = "Stolen" }),
			layoutArgs(t, layout, victim, "", "save", func(a map[string]any) { a["name"] = "Stolen" }),
			layoutArgs(t, layout, rivalOrg, "", "save", func(a map[string]any) { a["name"] = "Hijacked" }),
			layoutArgs(t, layout, rivalOrg, saved, "save", func(a map[string]any) { a["name"] = "Hijacked" }),
			{"layoutId": layout, "accountId": victim, "expectedRevision": saved, "action": "archive"},
			{"layoutId": layout, "accountId": rivalOrg, "expectedRevision": saved, "action": "archive"},
		}
		for i, args := range attempts {
			if _, err := w.workers[1].handleSaveLayout(ctx, args, 0); err == nil {
				t.Errorf("%s: attempt %d to write another organization's layout succeeded", name, i)
			}
			intact(fmt.Sprintf("%s, attempt %d", name, i))
		}
	}

	// What the engine itself answers, apart from the handler: the rival's own
	// organization cannot be used to overwrite the layout by id, even by a write
	// that carries internal origin (the handler's own) and every argument.
	overwrite, err := langparser.RenderCall("createEmailLayout", map[string]any{
		"layoutId": layout, "accountId": rivalOrg, "name": "Hijacked", "document": layoutDocument(t, "builtin:spotlight"),
		"versionTime": "2031-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.engines[1].Execute(auth.ContextWithInternalOrigin(rival), "mutation "+overwrite); err == nil {
		t.Error("the engine let a member of another organization overwrite a layout by id")
	} else {
		t.Logf("the engine refuses the overwrite: %v", err)
	}
	intact("an overwrite by id")
}

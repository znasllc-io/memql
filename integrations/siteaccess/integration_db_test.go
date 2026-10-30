package siteaccess

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/identity"
)

// integration_db_test.go -- an operator's acts on an access code, against a
// real engine and a real database (design record 2026-10-29, sections 3 to 5).
//
// WHY A REAL ENGINE, for this package above most. Every property below is a
// claim about what a row holds after an act and about who the engine took each
// read and write to be: a code's digest and its generation, the fence that
// refuses a write decided from a stale read, the tier that admits one person
// and not another, the @serverOnly gate that refuses a read with no origin
// stamped. A fake engine has none of those, so a fake could not fail any of
// these tests for the reason they exist.
//
// A second engine wraps the real one in the tests that need something to go
// wrong at a chosen instant (spyEngine): it moves a row between a handler's read
// and its write, and records what each call was made as. Everything still lands
// on the real engine, so a fence loss below is the real fence refusing a real
// write and not a fake's idea of one.
//
// Postgres-gated: a skip is what an unreachable database produces, and
// MEMQL_REQUIRE_DB=1 turns it into a failure.

// ---------------------------------------------------------------------------
// the shared engine
// ---------------------------------------------------------------------------

// sharedBoot is one engine per test process, for sharedReadMergeEngine's reason
// (component/memql): a boot costs seconds, and these tests run dozens of acts.
var sharedBoot struct {
	once    sync.Once
	dsn     string
	eng     *memql.MemQLEngine
	db      *bun.DB
	pingErr error
	bootErr error
}

// liveEngine is the narrow Engine this package asks for, over the real one.
type liveEngine struct{ e *memql.MemQLEngine }

func (l liveEngine) Execute(ctx context.Context, q string) (any, error) { return l.e.Execute(ctx, q) }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func bootEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	s := &sharedBoot
	s.once.Do(func() {
		s.dsn = dbtest.DSN()
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(s.dsn))), pgdialect.New())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("memql.New: %w", err)
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("engine Init: %w", err)
			return
		}
		// THE REAL RUNNER, as app bootstrap wires it, so a logic's statements run on the
		// executor that stamps a run and journals what each one returned.
		eng.SetLogicRunner(automations.NewLogicRunner(eng, steps.NewRegistry(), discardLogger()))
		// THE REAL HANDLERS BEHIND THE REAL DISPATCH, so a test that goes through
		// `builtin siteAccessCode...` meets the capability gate and the shared
		// organization gate on the way to them, as a person's call does.
		if err := eng.RegisterIntegration(NewIntegration(liveEngine{eng}, discardLogger())); err != nil {
			s.bootErr = fmt.Errorf("RegisterIntegration: %w", err)
			return
		}
		s.eng, s.db = eng, db
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "access code integration", s.dsn, s.pingErr)
		return nil, nil
	}
	require.NoError(t, s.bootErr, "the shared engine failed to boot")
	return s.eng, s.db
}

// ---------------------------------------------------------------------------
// the world a test acts in
// ---------------------------------------------------------------------------

// world is one test's deployable and the people around it, unique per test so a
// rerun against the same database collides with nothing a previous one left.
//
//	owner     a writer, a member of the account, and the site's owner
//	operator  a cluster owner, who owns nothing here
//	member    a writer in the account's group, who does not own the site
//	stranger  a writer in no group at all
type world struct {
	t   *testing.T
	eng *memql.MemQLEngine
	db  *bun.DB
	in  *Integration

	suffix     string
	owner      string
	operator   string
	member     string
	stranger   string
	account    string
	group      string
	pkg        string
	site       string
	deployable string
	domain     string

	rows  []string // canonical ids this test wrote and has to take away
	sites []string // canonical site ids whose codes and sessions go with them
}

func actorContext(userID string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userID})
}

// seedContext is the scaffolding actor, the cluster acting on its own: internal
// origin because the writers are @serverOnly, Synthetic because the rows it
// writes (organizations, memberships, a site with no owner) belong to nobody.
func seedContext() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "site-access-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "site-access-seeder"})
}

func newWorld(t *testing.T, name string) *world {
	t.Helper()
	eng, db := bootEngine(t)
	// Two short runs of digits rather than one long one: TestNoFailureCarriesASecret
	// looks for a digest by its long run of hex, and an id must not look like one.
	now := time.Now()
	s := fmt.Sprintf("%s-%d-%d", name, now.Unix(), now.Nanosecond())
	w := &world{
		t: t, eng: eng, db: db, in: NewIntegration(liveEngine{eng}, discardLogger()), suffix: s,
		owner: "ac-owner-" + s, operator: "ac-operator-" + s, member: "ac-member-" + s, stranger: "ac-stranger-" + s,
		account: "ac-acct-" + s, group: "ac-group-" + s, pkg: "ac-pkg-" + s, site: "ac-site-" + s,
		deployable: "desk-" + s, domain: "desk" + strings.ReplaceAll(s, "-", ""),
	}
	t.Cleanup(w.sweep)
	w.principal(w.owner, auth.RoleWriter)
	w.principal(w.operator, auth.RoleOwner)
	w.principal(w.member, auth.RoleWriter)
	w.principal(w.stranger, auth.RoleWriter)
	w.organization(w.account, w.group)
	w.join(w.group, w.owner)
	w.join(w.group, w.member)
	w.packageRow(w.pkg, w.account, []string{w.domain})
	w.siteRow(w.site, nil)
	w.declare(
		memql.AccessGateEntry{Deployable: w.deployable, Gate: "dashboard", Construct: "deskDigest", Kind: memql.AccessKindQuery, Domain: w.domain, Namespace: w.domain, Stamped: []string{memql.AccessArgCodeID}},
		memql.AccessGateEntry{Deployable: w.deployable, Gate: "clients", Construct: "deskClient", Kind: memql.AccessKindQuery, Domain: w.domain, Namespace: w.domain, Stamped: []string{memql.AccessArgSubject}},
	)
	return w
}

func (w *world) declare(entries ...memql.AccessGateEntry) {
	memql.SetAccessGatesForTest(entries...)
	w.t.Cleanup(func() { memql.SetAccessGatesForTest() })
}

// sweep takes away what this test wrote. It is not what isolates one test from
// another -- the ids do that -- but the shared lane's table is not a place to
// leave rows behind, and a code is found by its site rather than listed because
// the handlers mint its id.
func (w *world) sweep() {
	ctx := context.Background()
	for _, id := range w.rows {
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", id).Exec(ctx)
	}
	for _, site := range w.sites {
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept IN (?, ?)", codeConcept, sessionConcept).Where("payload->>'siteId' = ?", site).Exec(ctx)
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept = ?", "v1:identity:auditEvent").Where("payload#>>'{detail,siteId}' = ?", memql.BareShortId(site)).Exec(ctx)
	}
}

func (w *world) insert(concept, id string, payload map[string]any) {
	w.t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(w.t, err)
	_, err = w.eng.Execute(seedContext(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(concept), langparser.QuoteString(id), encoded))
	require.NoError(w.t, err, "seeding %s %s", concept, id)
	w.rows = append(w.rows, concept+":"+memql.BareShortId(id))
}

func (w *world) principal(id string, role auth.Role) {
	w.t.Helper()
	w.insert("v1:identity:user", id, map[string]any{
		"displayName": id, "primaryEmail": id + "@example.test", "role": string(role), "active": true,
	})
}

func (w *world) organization(account, group string) {
	w.t.Helper()
	w.insert("v1:accounts:account", account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
	w.mutate(seedContext(), "writeGroup", map[string]any{
		"groupId": group, "name": group, "kind": "account", "accountId": account, "status": "active",
	})
	w.rows = append(w.rows, "v1:identity:group:"+group)
}

func (w *world) join(group, user string) {
	w.t.Helper()
	parent, err := w.eng.Execute(seedContext(), `query groupById(groupId: `+langparser.QuoteString(group)+`)`)
	require.NoError(w.t, err)
	rows := memql.MaterializeRows(parent)
	require.Len(w.t, rows, 1, "joining a group that is not there")
	membership := group + "-" + user
	w.mutate(seedContext(), "writeGroupMembership", map[string]any{
		"membershipId": membership, "groupId": group, "userId": user, "origin": "added", "status": "active",
		"accountId": rows[0]["accountId"],
	})
	w.rows = append(w.rows, "v1:identity:groupMembership:"+membership)
}

func (w *world) packageRow(id, account string, domains []string) {
	w.t.Helper()
	payload := map[string]any{
		"name": id, "sourceKind": "repo", "status": "active", "ownerUserId": w.owner, "accountId": account,
	}
	if domains != nil {
		payload["dslDomains"] = domains
	}
	w.insert("v1:platform:package", id, payload)
}

// siteRow writes a site. A RAW INSERT rather than createSite, because the
// fixtures are sites the flow would not make (no owner, system-owned, no package
// deployable) and they are what the refusals are about. A nil value in `over`
// takes the field away.
func (w *world) siteRow(id string, over map[string]any) {
	w.t.Helper()
	payload := map[string]any{
		"hostname": id + ".example.test", "kind": "spa", "status": "draft", "bundleRef": "blob://site-access/v1",
		"ownerUserId": "v1:identity:user:" + w.owner, "accountId": w.account,
		"packageId": "v1:platform:package:" + w.pkg, "packageDeployableName": w.deployable,
	}
	for key, value := range over {
		if value == nil {
			delete(payload, key)
		} else {
			payload[key] = value
		}
	}
	w.insert(siteConcept, id, payload)
	w.sites = append(w.sites, siteConcept+":"+id)
}

func (w *world) mutate(ctx context.Context, name string, args map[string]any) {
	w.t.Helper()
	call, err := langparser.RenderCall(name, args)
	require.NoError(w.t, err)
	_, err = w.eng.Execute(ctx, "mutation "+call)
	require.NoError(w.t, err, "mutation %s", name)
}

// asOwner is how a write that is not under test reaches a row: as the site's owner,
// with internal origin. It is a fixture's shortcut, and it is what the handlers do for
// the two creates and nothing else: their updates run as the cluster acting.
func (w *world) asOwner() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), w.owner))
}

// forgeCode writes a code straight through the mutation, for the rows no handler
// would mint: one whose copied organization is out of date, or whose issuedAt is
// in the past.
func (w *world) forgeCode(site, codeID, account string, issued, expires time.Time) string {
	w.t.Helper()
	code, digest, hint, err := memql.MintAccessCode()
	require.NoError(w.t, err)
	args := map[string]any{
		"codeId": codeID, "siteId": site, "label": "Forged", "gate": "dashboard",
		"codeHash": digest, "codeHint": hint, "issuedAt": stamp(issued), "expiresAt": stamp(expires),
		"lastChangedBy": w.owner,
	}
	if account != "" {
		args["accountId"] = account
	}
	w.mutate(w.asOwner(), "createSiteAccessCode", args)
	return code
}

// moveExpiry re-times a code past every bound the handlers enforce, which is how
// a test gets a code that is already expired: time, not an act, is what expires
// a code, and these tests do not wait.
func (w *world) moveExpiry(codeID string, to time.Time) {
	w.t.Helper()
	w.mutate(w.asOwner(), "setSiteAccessCodeExpiry", map[string]any{
		"codeId": codeID, "expiresAt": stamp(to), "lastChangedBy": w.owner,
	})
}

func (w *world) removeCode(codeID string) {
	w.t.Helper()
	w.mutate(w.asOwner(), "removeSiteAccessCode", map[string]any{
		"codeId": codeID, "revokedAt": stamp(time.Now()), "lastChangedBy": w.owner,
	})
}

func stamp(at time.Time) string { return at.UTC().Truncate(time.Second).Format(time.RFC3339) }

// after is an expiry `d` from now, as a person's browser would send one.
func after(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }

// createArgs is a valid mint. A nil value in `over` leaves the argument out.
func (w *world) createArgs(over map[string]any) map[string]any {
	args := map[string]any{
		"siteId": w.site, "label": "Client reviewers", "gate": "dashboard", "expiresAt": after(time.Hour),
	}
	for key, value := range over {
		if value == nil {
			delete(args, key)
		} else {
			args[key] = value
		}
	}
	return args
}

// mint goes through the handler, so every row a test starts from is one a person
// could have.
func (w *world) mint(ctx context.Context, over map[string]any) (codeID, code string) {
	w.t.Helper()
	nodes, err := w.in.handleCreate(ctx, w.createArgs(over), 0)
	require.NoError(w.t, err)
	reply := onlyPayload(w.t, nodes)
	codeID, _ = reply["codeId"].(string)
	code, _ = reply["code"].(string)
	require.NotEmpty(w.t, codeID)
	require.NotEmpty(w.t, code)
	return codeID, code
}

func onlyPayload(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	require.Len(t, nodes, 1)
	return payloadOf(t, nodes[0])
}

func payloadOf(t *testing.T, node memorynodes.MemoryNode) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(node.Payload, &out))
	return out
}

// row reads the stored row itself, digest included, straight off the table: the
// question here is what was stored, which no shape would answer.
func (w *world) row(codeID string) map[string]any {
	w.t.Helper()
	var node memorynodes.MemoryNode
	err := w.db.NewSelect().Model(&node).
		Where("concept = ?", codeConcept).Where("id = ?", codeConcept+":"+codeID).
		Order("createdAt DESC").Limit(1).Scan(context.Background())
	require.NoError(w.t, err, "reading back code %s", codeID)
	var payload map[string]any
	require.NoError(w.t, json.Unmarshal(node.Payload, &payload))
	return payload
}

func bare(v any) string { return memql.BareShortId(fmt.Sprint(v)) }

// codesOn counts every version of every code row on a site.
func (w *world) codesOn(site string) int {
	w.t.Helper()
	n, err := w.db.NewSelect().TableExpr(`"MemoryNodes"`).
		Where("concept = ?", codeConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Count(context.Background())
	require.NoError(w.t, err)
	return int(n)
}

// codesOnSiteDistinct counts the codes on a site, however many versions each has.
func (w *world) codesOnSiteDistinct(site string) int {
	w.t.Helper()
	var ids []string
	require.NoError(w.t, w.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("DISTINCT id").
		Where("concept = ?", codeConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Scan(context.Background(), &ids))
	return len(ids)
}

// versions counts the versions of one code, optionally only those carrying one
// generation: a regenerate that lost must have left none behind.
func (w *world) versions(codeID string, generation int) int {
	w.t.Helper()
	q := w.db.NewSelect().TableExpr(`"MemoryNodes"`).Where("id = ?", codeConcept+":"+codeID)
	if generation > 0 {
		q = q.Where("payload->>'generation' = ?", fmt.Sprint(generation))
	}
	n, err := q.Count(context.Background())
	require.NoError(w.t, err)
	return int(n)
}

// events reads the audit rows naming one code, oldest first.
func (w *world) events(codeID string) []map[string]any {
	w.t.Helper()
	var nodes []memorynodes.MemoryNode
	err := w.db.NewSelect().Model(&nodes).
		Where("concept = ?", "v1:identity:auditEvent").Where("payload->>'targetId' = ?", codeID).
		Order("createdAt ASC").Scan(context.Background())
	require.NoError(w.t, err)
	out := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		var payload map[string]any
		require.NoError(w.t, json.Unmarshal(node.Payload, &payload))
		out = append(out, payload)
	}
	return out
}

// eventsOnSite counts the audit rows a site's acts wrote, for a refusal that must
// have left none, whatever code id it might have minted.
func (w *world) eventsOnSite(site string) int {
	w.t.Helper()
	n, err := w.db.NewSelect().TableExpr(`"MemoryNodes"`).
		Where("concept = ?", "v1:identity:auditEvent").Where("payload#>>'{detail,siteId}' = ?", site).Count(context.Background())
	require.NoError(w.t, err)
	return int(n)
}

// builtin is a call as a person's stream makes it: through the engine's dispatch,
// with whatever gates that crosses.
func (w *world) builtin(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	w.t.Helper()
	call, err := langparser.RenderCall(name, args)
	require.NoError(w.t, err)
	res, err := w.eng.Execute(ctx, "builtin "+call)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

func (w *world) installGrants() {
	w.t.Helper()
	grants, members := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.eng.InstallGrantResolution()
	w.t.Cleanup(func() { auth.SetGrantSource(grants); auth.SetMembershipSource(members) })
}

func (w *world) grantPart(group string) {
	w.t.Helper()
	id := auth.GrantRowID(auth.SubjectKindGroup, group, auth.VerbExecute, "app:deployables/access")
	w.mutate(seedContext(), "writeGrant", map[string]any{
		"grantId": id, "subjectKind": auth.SubjectKindGroup, "subjectId": group,
		"verb": auth.VerbExecute, "resourceType": "app:deployables/access", "effect": auth.GrantAllow, "grantedBy": "site-access-seeder",
	})
	w.rows = append(w.rows, "v1:rbac:grant:"+id)
}

// requireSentence holds a refusal to what the OS shows it as: verbatim, so it
// opens with a capital, closes like a sentence, and carries none of the frames a
// Go error usually wears. It answers the text, for the caller's next assertion.
func requireSentence(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	msg := err.Error()
	first, _ := utf8.DecodeRuneInString(msg)
	require.True(t, unicode.IsUpper(first), "a refusal the OS shows verbatim opens in lower case: %q", msg)
	require.True(t, strings.HasSuffix(msg, "."), "a refusal the OS shows verbatim does not read as a sentence: %q", msg)
	for _, frame := range []string{"siteAccess", "siteaccess", "memql:", "row version", "not implemented"} {
		require.NotContains(t, msg, frame, "a refusal carries a frame a person cannot act on: %q", msg)
	}
	require.False(t, anyDigest.MatchString(msg), "a refusal carries a digest: %q", msg)
	return msg
}

// anyDigest is what a stored digest, or the front of one, looks like in text.
var anyDigest = regexp.MustCompile(`[0-9a-f]{16,}`)

// ---------------------------------------------------------------------------
// minting
// ---------------------------------------------------------------------------

var codeFormat = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){4}$`)

func TestCreateReturnsTheCodeOnceAndStoresOnlyItsDigest(t *testing.T) {
	w := newWorld(t, "once")
	operator := actorContext(w.operator, auth.RoleOwner)

	// THROUGH THE DISPATCH, so what is asserted is the reply a person's call gets.
	rows, err := w.builtin(operator, "siteAccessCodeCreate", w.createArgs(map[string]any{"subject": "acme"}))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	reply := rows[0]
	codeID, code := fmt.Sprint(reply["codeId"]), fmt.Sprint(reply["code"])
	require.Regexp(t, codeFormat, code, "the reply carries the plain code, in its five groups")
	require.Equal(t, codeID, memql.BareShortId(codeID), "the reply's id is bare, as every client id is")
	_, parseErr := time.Parse(time.RFC3339, fmt.Sprint(reply["expiresAt"]))
	require.NoError(t, parseErr, "the reply carries the expiry")

	stored := w.row(codeID)
	require.Equal(t, memql.AccessCodeDigest(code), stored["codeHash"], "the row holds the digest of what the operator was handed")
	require.Equal(t, code[len(code)-4:], stored["codeHint"], "the hint is the code's last four symbols")
	require.EqualValues(t, 1, stored["generation"])

	blob, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NotContains(t, string(blob), code, "the stored row holds the plain code")
	require.NotContains(t, string(blob), memql.NormalizeAccessCode(code), "the stored row holds the plain code, unhyphenated")

	// THE LIST NEVER SHOWS IT AGAIN, nor the digest it was checked against.
	listed, err := w.builtin(operator, "siteAccessCodesForSite", map[string]any{"siteId": w.site})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	listBlob, err := json.Marshal(listed)
	require.NoError(t, err)
	require.NotContains(t, string(listBlob), code, "the list shows the code")
	require.NotContains(t, string(listBlob), memql.NormalizeAccessCode(code), "the list shows the code, unhyphenated")
	require.NotContains(t, string(listBlob), memql.AccessCodeDigest(code), "the list shows the digest")
	require.NotContains(t, string(listBlob), "codeHash", "the list has a digest field")

	row := listed[0]
	require.Equal(t, codeID, row["id"])
	require.Equal(t, w.site, row["siteId"], "the list's ids are bare")
	require.Equal(t, w.account, row["accountId"], "the list's ids are bare")
	require.Equal(t, "Client reviewers", row["label"])
	require.Equal(t, "dashboard", row["gate"])
	require.Equal(t, "acme", row["subject"])
	require.Equal(t, code[len(code)-4:], row["codeHint"])
	require.EqualValues(t, 1, row["generation"])
	require.NotEmpty(t, row["issuedAt"])
	require.NotEmpty(t, row["expiresAt"])
	require.NotEmpty(t, row["createdAt"])
	for _, absent := range []string{"revokedAt", "lastOpenedAt", "ownerUserId", "lastChangedBy"} {
		_, present := row[absent]
		require.False(t, present, "a fresh code's list row carries %q: %v", absent, row)
	}
}

func TestCreateRefusals(t *testing.T) {
	w := newWorld(t, "refusals")
	operator := actorContext(w.operator, auth.RoleOwner)
	valid := func(site string, over map[string]any) map[string]any {
		args := w.createArgs(over)
		args["siteId"] = site
		return args
	}

	ownerless, systemOwned, unpackaged := "ac-ownerless-"+w.suffix, "ac-system-"+w.suffix, "ac-unpackaged-"+w.suffix
	nodomains, unreadablePackage := "ac-nodomains-"+w.suffix, "ac-foreignpkg-"+w.suffix
	emptyPackage, foreignPackage := "ac-pkg-empty-"+w.suffix, "ac-pkg-foreign-"+w.suffix
	otherAccount, otherGroup := "ac-acct-other-"+w.suffix, "ac-group-other-"+w.suffix
	w.organization(otherAccount, otherGroup)
	w.packageRow(emptyPackage, w.account, []string{})
	w.packageRow(foreignPackage, otherAccount, []string{w.domain})
	w.siteRow(ownerless, map[string]any{"ownerUserId": nil})
	w.siteRow(systemOwned, map[string]any{"systemOwned": true})
	w.siteRow(unpackaged, map[string]any{"packageDeployableName": nil, "packageId": nil})
	w.siteRow(nodomains, map[string]any{"packageId": "v1:platform:package:" + emptyPackage})
	w.siteRow(unreadablePackage, map[string]any{"packageId": "v1:platform:package:" + foreignPackage})

	cases := []struct {
		name   string
		caller context.Context
		args   map[string]any
		// says is what the sentence must name, so a refusal for some other reason
		// fails this case rather than passing it by luck.
		says string
	}{
		{"a site the caller cannot read", actorContext(w.stranger, auth.RoleWriter), valid(w.site, nil), "read"},
		{"a site that does not exist", operator, valid("ac-nowhere-"+w.suffix, nil), "read"},
		{"no site at all", operator, valid("", nil), "deployable"},
		{"a site with no owner", operator, valid(ownerless, nil), "owner"},
		{"a system-owned site", operator, valid(systemOwned, nil), "managed through"},
		{"a site with no package deployable", operator, valid(unpackaged, nil), "package"},
		{"a package with no list of domains", operator, valid(nodomains, nil), "no gates"},
		{"a package the caller cannot read", actorContext(w.member, auth.RoleWriter), valid(unreadablePackage, nil), "no gates"},
		{"a gate the deployable does not declare", operator, valid(w.site, map[string]any{"gate": "secrets"}), "dashboard"},
		{"no gate", operator, valid(w.site, map[string]any{"gate": nil}), "clients"},
		{"an empty label", operator, valid(w.site, map[string]any{"label": ""}), "name"},
		{"a label of only spaces", operator, valid(w.site, map[string]any{"label": "   "}), "name"},
		{"no label", operator, valid(w.site, map[string]any{"label": nil}), "name"},
		{"a label of 81 characters", operator, valid(w.site, map[string]any{"label": strings.Repeat("x", 81)}), "80"},
		{"a subject of 121 characters", operator, valid(w.site, map[string]any{"subject": strings.Repeat("x", 121)}), "120"},
		{"an expiry 30 seconds away", operator, valid(w.site, map[string]any{"expiresAt": after(30 * time.Second)}), "1 minute"},
		{"an expiry in the past", operator, valid(w.site, map[string]any{"expiresAt": after(-time.Hour)}), "already passed"},
		{"an expiry 400 days away", operator, valid(w.site, map[string]any{"expiresAt": after(400 * 24 * time.Hour)}), "366 days"},
		{"an expiry that is not a date", operator, valid(w.site, map[string]any{"expiresAt": "next tuesday"}), "date"},
		{"no expiry", operator, valid(w.site, map[string]any{"expiresAt": nil}), "expire"},
	}
	before := w.codesOn(w.site)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := w.in.handleCreate(tc.caller, tc.args, 0)
			require.Nil(t, nodes, "a refused mint answered with a reply")
			require.Contains(t, requireSentence(t, err), tc.says, "refused, but not for this reason")
		})
	}
	// NOTHING WAS MINTED BY ANY OF THEM, on any of the sites, and nothing audited.
	require.Equal(t, before, w.codesOn(w.site), "a refused mint wrote a code")
	for _, site := range []string{w.site, ownerless, systemOwned, unpackaged, nodomains, unreadablePackage} {
		require.Zero(t, w.codesOn(site), "a refused mint wrote a code on %s", site)
		require.Zero(t, w.eventsOnSite(site), "a refused mint wrote an audit event on %s", site)
	}

	// THE REACHABLE POSITIVE. A table of refusals passes for the wrong reason if
	// the fixture itself refuses everything.
	for name, args := range map[string]map[string]any{
		"the plain mint":                 nil,
		"a label of exactly 80":          {"label": strings.Repeat("x", 80)},
		"a subject of exactly 120":       {"subject": strings.Repeat("x", 120)},
		"multi-byte label of 80 symbols": {"label": strings.Repeat("é", 80)},
		"the other gate":                 {"gate": "clients", "subject": "acme"},
		"an expiry of 366 days":          {"expiresAt": after(366*24*time.Hour - time.Minute)},
	} {
		_, err := w.in.handleCreate(operator, w.createArgs(args), 0)
		require.NoError(t, err, "%s was refused", name)
	}
}

func TestCreateCopiesOwnerAndAccountFromTheSite(t *testing.T) {
	w := newWorld(t, "copies")
	developer := "ac-developer-" + w.suffix
	w.principal(developer, auth.RoleDeveloper)

	rows, err := w.builtin(actorContext(developer, auth.RoleDeveloper), "siteAccessCodeCreate", w.createArgs(nil))
	require.NoError(t, err, "a developer, who holds the part, was refused")
	require.Len(t, rows, 1)
	stored := w.row(fmt.Sprint(rows[0]["codeId"]))

	require.Equal(t, w.owner, bare(stored["ownerUserId"]), "the row's owner is the site's, whoever asked")
	require.Equal(t, w.account, bare(stored["accountId"]), "the row's organization is the site's")
	require.Equal(t, developer, bare(stored["lastChangedBy"]), "the person who asked is recorded, and not as the owner")
	require.Equal(t, w.site, bare(stored["siteId"]))
}

// ---------------------------------------------------------------------------
// regenerating
// ---------------------------------------------------------------------------

func TestRegenerateReplacesTheSecretAndKeepsEverythingElse(t *testing.T) {
	w := newWorld(t, "regen")
	operator := actorContext(w.operator, auth.RoleOwner)
	developer := "ac-developer-" + w.suffix
	w.principal(developer, auth.RoleDeveloper)

	codeID, code := w.mint(operator, map[string]any{"subject": "acme", "expiresAt": after(2 * time.Hour)})
	before := w.row(codeID)

	nodes, err := w.in.handleRegenerate(actorContext(developer, auth.RoleDeveloper), map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
	reply := onlyPayload(t, nodes)
	fresh := fmt.Sprint(reply["code"])
	require.Equal(t, codeID, reply["codeId"], "regenerating gave the code a new id")
	require.Regexp(t, codeFormat, fresh)
	require.NotEqual(t, code, fresh, "regenerating handed back the same code")
	require.Equal(t, before["expiresAt"], reply["expiresAt"], "the reply's expiry is not the one kept")

	row := w.row(codeID)
	for _, kept := range []string{"siteId", "label", "gate", "subject", "ownerUserId", "accountId", "expiresAt"} {
		require.Equal(t, before[kept], row[kept], "regenerating changed %s", kept)
	}
	require.EqualValues(t, 2, row["generation"], "the generation is what ends the old secret's sessions")
	require.Equal(t, memql.AccessCodeDigest(fresh), row["codeHash"])
	require.Equal(t, fresh[len(fresh)-4:], row["codeHint"])
	require.Equal(t, developer, bare(row["lastChangedBy"]))

	// THE OLD CODE IS GONE, by the same read the bff uses to look one up.
	store := NewGraphStore(liveEngine{w.eng})
	gone, err := store.CodeByHash(context.Background(), w.site, memql.AccessCodeDigest(code))
	require.NoError(t, err)
	require.Nil(t, gone, "the code that was regenerated away still resolves")
	found, err := store.CodeByHash(context.Background(), w.site, memql.AccessCodeDigest(fresh))
	require.NoError(t, err)
	require.NotNil(t, found, "the regenerated code does not resolve")
	require.Equal(t, 2, found.Generation)

	t.Run("a new expiry is taken when one is given", func(t *testing.T) {
		to := after(3 * time.Hour)
		nodes, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID, "expiresAt": to}, 0)
		require.NoError(t, err)
		require.Equal(t, to, onlyPayload(t, nodes)["expiresAt"])
		row := w.row(codeID)
		require.Equal(t, to, row["expiresAt"])
		require.EqualValues(t, 3, row["generation"])
	})
	t.Run("a new expiry is held to the bounds", func(t *testing.T) {
		for name, to := range map[string]string{"too soon": after(30 * time.Second), "too far": after(400 * 24 * time.Hour), "not a date": "soon"} {
			hash := w.row(codeID)["codeHash"]
			_, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID, "expiresAt": to}, 0)
			requireSentence(t, err)
			require.Equal(t, hash, w.row(codeID)["codeHash"], "a refused regenerate (%s) replaced the secret", name)
		}
	})
	t.Run("a blank expiry is no expiry", func(t *testing.T) {
		// The generated Go builder writes this optional argument whether it was given
		// or not, so a blank has to keep the expiry rather than be refused as a bad date.
		kept := w.row(codeID)["expiresAt"]
		nodes, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID, "expiresAt": ""}, 0)
		require.NoError(t, err)
		require.Equal(t, kept, onlyPayload(t, nodes)["expiresAt"])
		require.Equal(t, kept, w.row(codeID)["expiresAt"])
	})
	t.Run("what is written is what was checked: UTC at whole seconds", func(t *testing.T) {
		// A browser sends milliseconds, and a person's offset is a fine thing to send.
		zone := time.FixedZone("somewhere", -5*60*60)
		given := time.Now().In(zone).Add(4 * time.Hour).Truncate(time.Second).Add(250 * time.Millisecond)
		nodes, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID, "expiresAt": given.Format(time.RFC3339Nano)}, 0)
		require.NoError(t, err)
		want := given.UTC().Truncate(time.Second).Format(time.RFC3339)
		require.Equal(t, want, onlyPayload(t, nodes)["expiresAt"])
		require.Equal(t, want, w.row(codeID)["expiresAt"])
	})
	t.Run("issuedAt moves to the new generation", func(t *testing.T) {
		forged := "ac-forged-" + w.suffix
		w.forgeCode(w.site, forged, w.account, time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour))
		old := w.row(forged)["issuedAt"]
		_, err := w.in.handleRegenerate(operator, map[string]any{"codeId": forged}, 0)
		require.NoError(t, err)
		require.NotEqual(t, old, w.row(forged)["issuedAt"], "issuedAt says when THIS generation was minted")
	})
}

func TestRegenerateOfAnExpiredCodeRequiresANewExpiry(t *testing.T) {
	w := newWorld(t, "regenexpired")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	w.moveExpiry(codeID, time.Now().Add(-time.Minute))
	before := w.row(codeID)

	_, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
	require.Contains(t, requireSentence(t, err), "expir")
	require.Equal(t, before, w.row(codeID), "the refused regenerate changed the row")
	require.Len(t, w.events(codeID), 1, "the refused regenerate was audited as though it happened")

	// With an expiry it is the code a person asked for: new secret, new life.
	to := after(time.Hour)
	nodes, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID, "expiresAt": to}, 0)
	require.NoError(t, err)
	require.Equal(t, to, onlyPayload(t, nodes)["expiresAt"])
	row := w.row(codeID)
	require.Equal(t, to, row["expiresAt"])
	require.EqualValues(t, 2, row["generation"])
}

func TestRegenerateOfARemovedCodeIsRefused(t *testing.T) {
	w := newWorld(t, "regenremoved")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	_, err := w.in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
	before := w.row(codeID)

	for name, args := range map[string]map[string]any{
		"keeping the expiry":  {"codeId": codeID},
		"with a fresh expiry": {"codeId": codeID, "expiresAt": after(time.Hour)},
	} {
		nodes, err := w.in.handleRegenerate(operator, args, 0)
		require.Nil(t, nodes, "%s: a removed code handed back a new one", name)
		require.Contains(t, requireSentence(t, err), "add a new code", "%s: the refusal does not say what to do", name)
	}
	require.Equal(t, before, w.row(codeID), "removal is final, and a refused regenerate changed the row")
}

// ---------------------------------------------------------------------------
// changing the expiry
// ---------------------------------------------------------------------------

func TestSetExpiryKeepsTheCode(t *testing.T) {
	w := newWorld(t, "setexpiry")
	operator := actorContext(w.operator, auth.RoleOwner)
	developer := "ac-developer-" + w.suffix
	w.principal(developer, auth.RoleDeveloper)
	codeID, code := w.mint(operator, nil)
	before := w.row(codeID)

	to := after(5 * time.Hour)
	nodes, err := w.in.handleSetExpiry(actorContext(developer, auth.RoleDeveloper), map[string]any{"codeId": codeID, "expiresAt": to}, 0)
	require.NoError(t, err)
	reply := onlyPayload(t, nodes)
	require.Equal(t, codeID, reply["codeId"])
	require.Equal(t, to, reply["expiresAt"])
	_, handed := reply["code"]
	require.False(t, handed, "changing the expiry handed back a code")

	row := w.row(codeID)
	require.Equal(t, to, row["expiresAt"])
	require.Equal(t, before["codeHash"], row["codeHash"], "changing the expiry replaced the secret")
	require.Equal(t, memql.AccessCodeDigest(code), row["codeHash"])
	require.EqualValues(t, 1, row["generation"], "changing the expiry ended every open session")
	require.Equal(t, developer, bare(row["lastChangedBy"]))

	t.Run("the bounds hold", func(t *testing.T) {
		for name, value := range map[string]any{
			"30 seconds":  after(30 * time.Second),
			"400 days":    after(400 * 24 * time.Hour),
			"not a date":  "tomorrow",
			"no expiry":   nil,
			"already due": after(-time.Hour),
		} {
			args := map[string]any{"codeId": codeID}
			if value != nil {
				args["expiresAt"] = value
			}
			held := w.row(codeID)["expiresAt"]
			_, err := w.in.handleSetExpiry(operator, args, 0)
			requireSentence(t, err)
			require.Equal(t, held, w.row(codeID)["expiresAt"], "%s moved the expiry", name)
		}
	})

	t.Run("extending an expired code revives it", func(t *testing.T) {
		w.moveExpiry(codeID, time.Now().Add(-time.Minute))
		revived := after(time.Hour)
		_, err := w.in.handleSetExpiry(operator, map[string]any{"codeId": codeID, "expiresAt": revived}, 0)
		require.NoError(t, err, "an expired code could not be revived, so somebody has to be handed a new one")
		row := w.row(codeID)
		require.Equal(t, revived, row["expiresAt"])
		require.Equal(t, memql.AccessCodeDigest(code), row["codeHash"], "reviving a code replaced it")
	})

	t.Run("a removed code stays removed", func(t *testing.T) {
		_, err := w.in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
		require.NoError(t, err)
		held := w.row(codeID)
		_, err = w.in.handleSetExpiry(operator, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
		require.Contains(t, requireSentence(t, err), "add a new code")
		require.Equal(t, held, w.row(codeID), "a removed code was re-timed")
	})
}

// ---------------------------------------------------------------------------
// removing
// ---------------------------------------------------------------------------

func TestRemoveIsSoft(t *testing.T) {
	w := newWorld(t, "remove")
	operator := actorContext(w.operator, auth.RoleOwner)
	developer := "ac-developer-" + w.suffix
	w.principal(developer, auth.RoleDeveloper)
	codeID, code := w.mint(operator, nil)
	before := w.row(codeID)

	nodes, err := w.in.handleRemove(actorContext(developer, auth.RoleDeveloper), map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
	require.Equal(t, codeID, onlyPayload(t, nodes)["codeId"])

	row := w.row(codeID)
	require.NotEmpty(t, row["revokedAt"], "a removed code does not say so")
	removedAt, parseErr := time.Parse(time.RFC3339, fmt.Sprint(row["revokedAt"]))
	require.NoError(t, parseErr)
	require.WithinDuration(t, time.Now(), removedAt, time.Minute)
	require.Equal(t, developer, bare(row["lastChangedBy"]))
	require.Equal(t, before["codeHash"], row["codeHash"], "removing a code changed its secret")
	require.Equal(t, before["expiresAt"], row["expiresAt"], "removing a code changed its expiry")

	// THE ROW STAYS, and both readers can still see it: the question it answers is
	// who could get in, and when.
	listed, err := w.in.handleList(operator, map[string]any{"siteId": w.site}, 0)
	require.NoError(t, err)
	require.Len(t, listed, 1, "a removed code vanished from the list; the OS hides it, the list does not")
	require.NotEmpty(t, payloadOf(t, listed[0])["revokedAt"])
	store := NewGraphStore(liveEngine{w.eng})
	found, err := store.CodeByHash(context.Background(), w.site, memql.AccessCodeDigest(code))
	require.NoError(t, err)
	require.NotNil(t, found, "a removed code stopped resolving, so the bff cannot give it the same answer as an unknown one")
	require.True(t, found.Revoked)

	// A SECOND REMOVE IS HARMLESS. It reports the same thing and moves nothing: the
	// row still says who removed the code, and when, and the log has one event.
	versions := w.versions(codeID, 0)
	nodes, err = w.in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err, "removing a removed code was refused")
	require.Equal(t, codeID, onlyPayload(t, nodes)["codeId"])
	require.Equal(t, versions, w.versions(codeID, 0), "the second remove wrote a version")
	require.Equal(t, row, w.row(codeID), "the second remove changed the record of the first")
	require.Len(t, w.events(codeID), 2, "one for the mint and one for the remove, and none for the second remove")
}

// ---------------------------------------------------------------------------
// the audit log
// ---------------------------------------------------------------------------

func TestEveryOperatorActIsAudited(t *testing.T) {
	w := newWorld(t, "audit")
	operatorID := "ac-audit-operator-" + w.suffix
	w.principal(operatorID, auth.RoleOwner)
	operator := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: operatorID, Role: auth.RoleOwner, PrimaryEmail: operatorID + "@example.test",
	})
	operator = auth.ContextWithToken(operator, &auth.TokenInfo{Subject: operatorID})

	codeID, first := w.mint(operator, nil)
	row := w.row(codeID)
	// NOT THE HINT. It is not a secret, four symbols of 25, and one that is all digits can
	// turn up in a timestamp, which would fail this test about one run in four thousand.
	secrets := []string{first, memql.NormalizeAccessCode(first), fmt.Sprint(row["codeHash"])}

	nodes, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
	second := fmt.Sprint(onlyPayload(t, nodes)["code"])
	row = w.row(codeID)
	secrets = append(secrets, second, memql.NormalizeAccessCode(second), fmt.Sprint(row["codeHash"]))

	_, err = w.in.handleSetExpiry(operator, map[string]any{"codeId": codeID, "expiresAt": after(3 * time.Hour)}, 0)
	require.NoError(t, err)
	_, err = w.in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)

	events := w.events(codeID)
	actions, acts := make([]string, 0, len(events)), make([]string, 0, len(events))
	for _, event := range events {
		actions = append(actions, fmt.Sprint(event["action"]))
		require.Equal(t, "siteAccessCode", event["targetType"])
		require.Equal(t, codeID, event["targetId"], "the event's target is the code's bare id")
		require.Equal(t, "success", event["outcome"])
		require.Equal(t, "authorization", event["category"])
		require.Equal(t, operatorID, bare(event["actorUserId"]), "the event's actor is the person, not the site's owner")
		require.Equal(t, operatorID+"@example.test", event["actorEmail"])
		require.Equal(t, string(auth.RoleOwner), event["actorRole"])
		detail, ok := event["detail"].(map[string]any)
		require.True(t, ok, "the event carries no detail: %v", event)
		require.Equal(t, w.site, detail["siteId"])
		require.Equal(t, "dashboard", detail["gate"])
		acts = append(acts, fmt.Sprint(detail["act"]))

		blob, err := json.Marshal(event)
		require.NoError(t, err)
		for _, secret := range secrets {
			require.NotContains(t, string(blob), secret, "an audit event carries a code or a digest")
		}
	}
	require.Equal(t, []string{auditActionCreated, auditActionRegenerated, auditActionExpiryChanged, auditActionRemoved}, actions,
		"one event per act, in the order they happened")
	require.Equal(t, []string{"create", "regenerate", "setExpiry", "remove"}, acts, "the detail does not say which act it was")
}

// ---------------------------------------------------------------------------
// the gates
// ---------------------------------------------------------------------------

func TestGatesAnswerForThisDeployableOnly(t *testing.T) {
	w := newWorld(t, "gates")
	operator := actorContext(w.operator, auth.RoleOwner)
	w.declare(
		memql.AccessGateEntry{Deployable: w.deployable, Gate: "dashboard", Construct: "deskDigest", Kind: memql.AccessKindQuery, Domain: w.domain, Stamped: []string{memql.AccessArgCodeID}},
		memql.AccessGateEntry{Deployable: w.deployable, Gate: "dashboard", Construct: "deskAlerts", Kind: memql.AccessKindQuery, Domain: w.domain},
		memql.AccessGateEntry{Deployable: w.deployable, Gate: "clients", Construct: "deskClient", Kind: memql.AccessKindQuery, Domain: w.domain, Stamped: []string{memql.AccessArgSubject}},
		// The same gate name on somebody else's deployable, in the same domain.
		memql.AccessGateEntry{Deployable: "someone-elses", Gate: "secrets", Construct: "theirBrief", Kind: memql.AccessKindQuery, Domain: w.domain},
		// This deployable's name, in a domain this package does not ship.
		memql.AccessGateEntry{Deployable: w.deployable, Gate: "foreign", Construct: "foreignBrief", Kind: memql.AccessKindQuery, Domain: "elsewhere"},
	)

	gates := func(caller context.Context, site string) (map[string]map[string]any, error) {
		t.Helper()
		nodes, err := w.in.handleGates(caller, map[string]any{"siteId": site}, 0)
		if err != nil {
			return nil, err
		}
		out := map[string]map[string]any{}
		for _, node := range nodes {
			row := payloadOf(t, node)
			out[fmt.Sprint(row["gate"])] = row
		}
		return out, nil
	}

	got, err := gates(operator, w.site)
	require.NoError(t, err)
	require.Len(t, got, 2, "the gates of another deployable, or of a domain this package does not ship, were offered: %v", got)
	require.Equal(t, false, got["dashboard"]["usesSubject"])
	require.Equal(t, []any{"deskAlerts", "deskDigest"}, got["dashboard"]["constructs"])
	require.Equal(t, true, got["clients"]["usesSubject"], "a gate that stamps accessSubject did not say so, so the form would not ask for one")
	require.Equal(t, []any{"deskClient"}, got["clients"]["constructs"])

	// EVERY WAY OF NOT KNOWING IS NO GATES, and none of them is an error: an empty
	// answer is the panel's cue to explain @accessGate.
	otherAccount, otherGroup := "ac-acct-other-"+w.suffix, "ac-group-other-"+w.suffix
	w.organization(otherAccount, otherGroup)
	emptyPackage, foreignPackage := "ac-pkg-empty-"+w.suffix, "ac-pkg-foreign-"+w.suffix
	w.packageRow(emptyPackage, w.account, []string{})
	w.packageRow(foreignPackage, otherAccount, []string{w.domain})
	unpackaged, noDomains, unreadable := "ac-unpackaged-"+w.suffix, "ac-nodomains-"+w.suffix, "ac-foreignpkg-"+w.suffix
	w.siteRow(unpackaged, map[string]any{"packageDeployableName": nil, "packageId": nil})
	w.siteRow(noDomains, map[string]any{"packageId": "v1:platform:package:" + emptyPackage})
	w.siteRow(unreadable, map[string]any{"packageId": "v1:platform:package:" + foreignPackage})
	for name, tc := range map[string]struct {
		caller context.Context
		site   string
	}{
		"no package deployable": {operator, unpackaged},
		"no list of domains":    {operator, noDomains},
		"an unreadable package": {actorContext(w.member, auth.RoleWriter), unreadable},
	} {
		got, err := gates(tc.caller, tc.site)
		require.NoError(t, err, name)
		require.Empty(t, got, name)
	}

	// A SITE THE CALLER CANNOT READ IS REFUSED, not answered empty: an empty answer
	// would tell a stranger the deployable declares nothing.
	_, err = gates(actorContext(w.stranger, auth.RoleWriter), w.site)
	requireSentence(t, err)
}

// ---------------------------------------------------------------------------
// the list, and when a code was last opened
// ---------------------------------------------------------------------------

func TestListCarriesLastOpenedForTheCurrentGeneration(t *testing.T) {
	w := newWorld(t, "lastopened")
	operator := actorContext(w.operator, auth.RoleOwner)
	store := NewGraphStore(liveEngine{w.eng})
	codeID, _ := w.mint(operator, nil)
	other, _ := w.mint(operator, map[string]any{"label": "Unopened"})

	lastOpened := func(id string) (any, bool) {
		t.Helper()
		nodes, err := w.in.handleList(operator, map[string]any{"siteId": w.site}, 0)
		require.NoError(t, err)
		for _, node := range nodes {
			row := payloadOf(t, node)
			if row["id"] == id {
				value, present := row["lastOpenedAt"]
				return value, present
			}
		}
		t.Fatalf("the list did not carry code %s", id)
		return nil, false
	}
	open := func(generation int, at time.Time, digestSeed string) string {
		t.Helper()
		session := "ac-session-" + digestSeed + "-" + w.suffix
		require.NoError(t, store.CreateSession(context.Background(), w.owner, Session{
			ID: session, SiteID: w.site, CodeID: codeID, Generation: generation, IssuedAt: at,
		}, memql.AccessSessionTokenDigest(digestSeed+w.suffix)))
		return session
	}

	_, present := lastOpened(codeID)
	require.False(t, present, "a code nobody has opened says it was opened")

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	open(1, base, "early")
	newest := open(1, base.Add(10*time.Minute), "late")
	open(1, base.Add(5*time.Minute), "middle")

	opened, present := lastOpened(codeID)
	require.True(t, present)
	require.Equal(t, stamp(base.Add(10*time.Minute)), opened, "the newest session of the generation is the answer, whatever order they were written in")

	_, present = lastOpened(other)
	require.False(t, present, "another code's sessions answered for this one")

	// An ended session was still opened.
	require.NoError(t, store.EndSession(context.Background(), newest, time.Now()))
	opened, present = lastOpened(codeID)
	require.True(t, present)
	require.Equal(t, stamp(base.Add(10*time.Minute)), opened, "ending a session moved when the code was last opened")

	// A REGENERATED CODE READS AS NOT OPENED, though older sessions exist: the new
	// secret has not been used, and no write to any session says so.
	_, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
	_, present = lastOpened(codeID)
	require.False(t, present, "a regenerated code shows the old secret's last use")

	open(2, base.Add(time.Minute), "regenerated")
	opened, present = lastOpened(codeID)
	require.True(t, present)
	require.Equal(t, stamp(base.Add(time.Minute)), opened)
}

func TestAMemberSeesLastOpened(t *testing.T) {
	w := newWorld(t, "memberopened")
	w.installGrants()
	w.grantPart(w.group)
	store := NewGraphStore(liveEngine{w.eng})
	codeID, _ := w.mint(actorContext(w.operator, auth.RoleOwner), nil)
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	require.NoError(t, store.CreateSession(context.Background(), w.owner, Session{
		SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: at,
	}, memql.AccessSessionTokenDigest("member-sees-"+w.suffix)))

	member := actorContext(w.member, auth.RoleWriter)

	// THE PREMISE: the member's own actor is NOT admitted to the session, whose tier
	// has no account arm. Reading it as the caller would show them "Not opened yet"
	// on every code of every site they manage and do not personally own.
	call, err := langparser.RenderCall("siteAccessSessionLatestForCode", map[string]any{"codeId": codeID, "generation": 1})
	require.NoError(t, err)
	res, err := w.eng.Execute(auth.ContextWithInternalOrigin(member), "query "+call)
	require.NoError(t, err)
	require.Empty(t, memql.MaterializeRows(res), "the premise failed: the session's tier admits a member, so this test proves nothing")

	// THROUGH THE DISPATCH, so the member's memoised scope is in the context the
	// handler borrows the owner from, as it is on a real call.
	rows, err := w.builtin(member, "siteAccessCodesForSite", map[string]any{"siteId": w.site})
	require.NoError(t, err)
	require.Len(t, rows, 1, "a member of the site's organization, holding the part, cannot see its codes")
	require.Equal(t, stamp(at), rows[0]["lastOpenedAt"], "a member sees an empty lastOpenedAt, because the read ran as them and not as the site's owner")
}

// ---------------------------------------------------------------------------
// the site, re-read under the caller
// ---------------------------------------------------------------------------

func TestActsReReadTheCodesSiteUnderTheCaller(t *testing.T) {
	w := newWorld(t, "reread")
	beta, betaGroup := "ac-acct-beta-"+w.suffix, "ac-group-beta-"+w.suffix
	w.organization(beta, betaGroup)

	// THE SITE IS BETA'S NOW, and the code's copy still says the account the member
	// belongs to. This is the one stale copy a move racing a mint can leave.
	movedSite := "ac-moved-" + w.suffix
	w.siteRow(movedSite, map[string]any{"accountId": beta})
	stale := "ac-stale-" + w.suffix
	w.forgeCode(movedSite, stale, w.account, time.Now(), time.Now().Add(time.Hour))

	member := actorContext(w.member, auth.RoleWriter)
	store := NewGraphStore(liveEngine{w.eng})

	// THE PREMISE: the tier admits the member to the code through its copied
	// account, and not to the site. Without the re-read the code's row is all a
	// handler would have seen, and the old organization would keep acting on a site
	// it can no longer read.
	read, ok, err := store.codeForCaller(member, stale)
	require.NoError(t, err)
	require.True(t, ok, "the premise failed: the member cannot read the code")
	require.Equal(t, w.account, read.AccountID, "the copy reads back bare")
	_, ok, err = store.siteForCaller(member, movedSite)
	require.NoError(t, err)
	require.False(t, ok, "the premise failed: the member can read the site")

	before := w.row(stale)
	for name, act := range map[string]func() ([]memorynodes.MemoryNode, error){
		"regenerate": func() ([]memorynodes.MemoryNode, error) {
			return w.in.handleRegenerate(member, map[string]any{"codeId": stale}, 0)
		},
		"set expiry": func() ([]memorynodes.MemoryNode, error) {
			return w.in.handleSetExpiry(member, map[string]any{"codeId": stale, "expiresAt": after(2 * time.Hour)}, 0)
		},
		"remove": func() ([]memorynodes.MemoryNode, error) {
			return w.in.handleRemove(member, map[string]any{"codeId": stale}, 0)
		},
	} {
		t.Run(name+" is refused for someone who cannot read the site", func(t *testing.T) {
			nodes, err := act()
			require.Nil(t, nodes)
			msg := requireSentence(t, err)
			require.Contains(t, msg, "cannot read the deployable", "the refusal does not say what is in the way")
			// THE SHARED GATE JUDGES THE COPY, so the people who manage the deployable now
			// are refused this code, and a person told to ask them would be sent nowhere.
			require.Contains(t, msg, "opens nothing", "the refusal does not say the code is harmless")
			require.Contains(t, msg, "cluster operator must remove it", "the refusal does not say who can do something about it")
			require.Equal(t, before, w.row(stale), "a refused act changed the row")
			require.Empty(t, w.events(stale), "a refused act was audited")
		})
	}

	// THE CONTROL: the same member, on a code and a site of their own organization.
	mine, _ := w.mint(actorContext(w.operator, auth.RoleOwner), nil)
	_, err = w.in.handleSetExpiry(member, map[string]any{"codeId": mine, "expiresAt": after(2 * time.Hour)}, 0)
	require.NoError(t, err, "a member of the site's organization was refused")

	// A CLUSTER OPERATOR can read both, and is the one person the shared gate lets
	// near a stale code, because it judges the copy. It opens nothing (the bff
	// refuses a code whose copy differs from its site's), so regenerating it would
	// hand back a code that does not work. Removing it is the one act that helps.
	operator := actorContext(w.operator, auth.RoleOwner)
	for name, act := range map[string]func() ([]memorynodes.MemoryNode, error){
		"regenerate": func() ([]memorynodes.MemoryNode, error) {
			return w.in.handleRegenerate(operator, map[string]any{"codeId": stale}, 0)
		},
		"set expiry": func() ([]memorynodes.MemoryNode, error) {
			return w.in.handleSetExpiry(operator, map[string]any{"codeId": stale, "expiresAt": after(2 * time.Hour)}, 0)
		},
	} {
		nodes, err := act()
		require.Nil(t, nodes, name)
		require.Contains(t, requireSentence(t, err), "Remove it", "%s of a stale code does not say what to do", name)
		require.Equal(t, before, w.row(stale), "%s changed a stale code", name)
	}
	_, err = w.in.handleRemove(operator, map[string]any{"codeId": stale}, 0)
	require.NoError(t, err, "a cluster operator could not remove a stale code, so nothing can")
	require.NotEmpty(t, w.row(stale)["revokedAt"])
}

// A SOFT-DELETED SITE READS AS NOTHING TO ANYBODY, a cluster operator included, and its
// codes have nothing left to open. So the refusal has to be true of that as well.
func TestACodeOfADeletedSiteIsRefusedWithoutSendingAnyoneAnywhere(t *testing.T) {
	w := newWorld(t, "deletedsite")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	w.insert(siteConcept, w.site, map[string]any{"deleted": true})
	before := w.row(codeID)

	_, err := w.in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
	msg := requireSentence(t, err)
	require.Contains(t, msg, "If it was deleted, this code opens nothing and can be left alone")
	require.Equal(t, before, w.row(codeID))
}

// ONLY REMOVAL IS LEFT TO A SITE THAT LOST ITS OWNER. Such a site cannot have a code
// minted, and the edge serves it nothing, so a code it still carries opens nothing: a
// regenerate would hand somebody a code that does not work, and a new date would say the
// same thing more quietly. But removing it needs nothing from the owner, and a code nobody
// can end blocks a move for good. All of this is asserted for the two people who reach it,
// a cluster operator and a member of the site's organization.
func TestACodeOfASiteThatLostItsOwnerCanStillBeEnded(t *testing.T) {
	w := newWorld(t, "ownerlost")
	site := "ac-ownerless-" + w.suffix
	w.siteRow(site, map[string]any{"ownerUserId": nil})
	for _, tc := range []struct {
		name, slug string
		caller     context.Context
	}{
		{"a cluster operator", "operator", actorContext(w.operator, auth.RoleOwner)},
		{"a member of the deployable's organization", "member", actorContext(w.member, auth.RoleWriter)},
	} {
		caller := tc.caller
		t.Run(tc.name, func(t *testing.T) {
			codeID := "ac-code-" + tc.slug + "-" + w.suffix
			w.forgeCode(site, codeID, w.account, time.Now(), time.Now().Add(time.Hour))
			before := w.row(codeID)

			nodes, err := w.in.handleRegenerate(caller, map[string]any{"codeId": codeID}, 0)
			require.Nil(t, nodes, "a regenerate handed back a code that opens nothing")
			require.Contains(t, requireSentence(t, err), "no owner", "refused, but not for this reason")
			nodes, err = w.in.handleRegenerate(caller, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
			require.Nil(t, nodes, "a regenerate with a new expiry handed back a code that opens nothing")
			require.Contains(t, requireSentence(t, err), "no owner")
			nodes, err = w.in.handleSetExpiry(caller, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
			require.Nil(t, nodes, "a code that opens nothing was given a new date")
			require.Contains(t, requireSentence(t, err), "no owner")
			require.Equal(t, before, w.row(codeID), "a refused act changed the row")
			require.Empty(t, w.events(codeID), "a refused act was audited")

			listed, err := w.in.handleList(caller, map[string]any{"siteId": site}, 0)
			require.NoError(t, err)
			require.NotEmpty(t, listed, "the code cannot be seen, so nobody can end it")

			_, err = w.in.handleRemove(caller, map[string]any{"codeId": codeID}, 0)
			require.NoError(t, err, "the one act left to this code was refused")
			row := w.row(codeID)
			require.NotEmpty(t, row["revokedAt"])
			require.Equal(t, w.owner, bare(row["ownerUserId"]), "the update moved the row's owner")
			require.Len(t, w.events(codeID), 1, "removing it was not audited")
		})
	}

	// WHICH REASON IS SAID MATTERS WHEN BOTH ARE TRUE. Sending a person to find the site an
	// owner, only to tell them next that the code was stale all along, is a wasted errand.
	t.Run("a code that is stale as well", func(t *testing.T) {
		moved := "ac-ownerless-moved-" + w.suffix
		w.siteRow(moved, map[string]any{"ownerUserId": nil, "accountId": nil})
		codeID := "ac-code-stale-ownerless-" + w.suffix
		w.forgeCode(moved, codeID, w.account, time.Now(), time.Now().Add(time.Hour))
		nodes, err := w.in.handleRegenerate(actorContext(w.operator, auth.RoleOwner), map[string]any{"codeId": codeID}, 0)
		require.Nil(t, nodes)
		require.Contains(t, requireSentence(t, err), "another organization", "told the reason that can change")
	})
}

// ---------------------------------------------------------------------------
// an act belongs to a person
// ---------------------------------------------------------------------------

// THE WRITE DOES NOT RUN AS THE PERSON WHO ASKED, so `lastChangedBy` is the only record of
// who did, and every act refuses a caller who is not somebody. The cluster acting is the
// one that would otherwise get through: it reads everything, and it would write a
// `system:` id into a field that promises a person.
func TestAnActBelongsToAPerson(t *testing.T) {
	w := newWorld(t, "person")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	before, versions, events := w.row(codeID), w.versions(codeID, 0), len(w.events(codeID))
	require.NotEmpty(t, auth.MaintenanceAutomationNames(), "there is no maintenance actor to try")

	// THE PREMISE of the operator credential's row: it is an owner, with no flag to say it
	// is not a person, so only its id gives it away.
	credential, ok := auth.AccessFromContext(operatorCredential())
	require.True(t, ok)
	require.False(t, credential.Synthetic, "the premise failed: this actor is flagged, so a flag check would catch it")
	require.Equal(t, auth.RoleOwner, credential.Role)

	callers := map[string]context.Context{
		"a caller with no id":      auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: auth.RoleOwner}),
		"no caller at all":         context.Background(),
		"a connector":              auth.ContextWithConnectorActor(context.Background(), "shopify"),
		"an anonymous caller":      auth.ContextWithAnonymousActor(context.Background()),
		"the cluster acting":       auth.ContextWithSystemActor(context.Background(), "some-part-of-the-cluster"),
		"the cluster, trusted":     auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "some-part-of-the-cluster")),
		"a maintenance automation": auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.MaintenanceAutomationNames()[0])),
		"the operator credential":  operatorCredential(),
	}
	acts := map[string]func(context.Context) ([]memorynodes.MemoryNode, error){
		"minting": func(ctx context.Context) ([]memorynodes.MemoryNode, error) {
			return w.in.handleCreate(ctx, w.createArgs(nil), 0)
		},
		"regenerating": func(ctx context.Context) ([]memorynodes.MemoryNode, error) {
			return w.in.handleRegenerate(ctx, map[string]any{"codeId": codeID}, 0)
		},
		"re-timing": func(ctx context.Context) ([]memorynodes.MemoryNode, error) {
			return w.in.handleSetExpiry(ctx, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
		},
		"removing": func(ctx context.Context) ([]memorynodes.MemoryNode, error) {
			return w.in.handleRemove(ctx, map[string]any{"codeId": codeID}, 0)
		},
	}
	for who, caller := range callers {
		for what, act := range acts {
			t.Run(what+" by "+who, func(t *testing.T) {
				nodes, err := act(caller)
				require.Nil(t, nodes, "a caller who is nobody was answered")
				require.Contains(t, requireSentence(t, err), "person", "refused, but not for this reason")
			})
		}
	}

	// THROUGH THE DISPATCH AT CLIENT ORIGIN, which is how that credential got through: it
	// holds every capability, the organization gate admits it, and it is the handler's
	// question alone. It may still read, since a read records nobody.
	for builtin, args := range map[string]map[string]any{
		"siteAccessCodeCreate":     w.createArgs(nil),
		"siteAccessCodeRegenerate": {"codeId": codeID},
		"siteAccessCodeSetExpiry":  {"codeId": codeID, "expiresAt": after(2 * time.Hour)},
		"siteAccessCodeRemove":     {"codeId": codeID},
	} {
		t.Run(builtin+" with the operator credential, through the dispatch", func(t *testing.T) {
			rows, err := w.builtin(operatorCredential(), builtin, args)
			require.Nil(t, rows, "the operator credential was answered")
			require.Contains(t, requireSentence(t, err), "person", "refused, but not for this reason")
		})
	}
	listed, err := w.builtin(operatorCredential(), "siteAccessCodesForSite", map[string]any{"siteId": w.site})
	require.NoError(t, err, "the operator credential can no longer read the list, and nothing asked for that")
	require.Len(t, listed, 1)

	require.Equal(t, before, w.row(codeID), "an act by nobody changed the row")
	require.Equal(t, versions, w.versions(codeID, 0), "an act by nobody wrote a version")
	require.Len(t, w.events(codeID), events, "an act by nobody was audited")
	require.Equal(t, versions, w.codesOn(w.site), "an act by nobody minted a code")

	// THE REACHABLE POSITIVE. Each of the four, by somebody, is the rest of this file; this
	// one is the shape a server path borrows: a real person's id, with no role of their own.
	borrowed := auth.ContextWithUserActor(context.Background(), w.owner)
	_, err = w.in.handleSetExpiry(borrowed, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
	require.NoError(t, err, "borrowed authority names a person, and it was refused as though it named nobody")
	require.Equal(t, w.owner, bare(w.row(codeID)["lastChangedBy"]))
}

// operatorCredential is the actor a stream admitted with MEMQL_OPERATOR_KEY resolves to,
// built the way ensureAccess builds it (component/grpc): the subject the interceptor
// stamps, through FallbackFromClaims, because no user row is behind it. That is why it
// carries no Synthetic flag, though the interceptor's own doc calls it synthetic.
func operatorCredential() context.Context {
	claims := map[string]any{
		"sub": "cluster:operator", "role": string(auth.RoleOwner), "roles": []string{string(auth.RoleOwner)},
		"name": "Cluster Operator", "identity.operator": map[string]any{"source": "operator_key"},
	}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, auth.FallbackFromClaims(claims))
}

// ---------------------------------------------------------------------------
// a test persona
// ---------------------------------------------------------------------------

// personaValidator answers the shared session check a persona is held to on every
// engine call, and says when one has ended.
type personaValidator struct {
	mu    sync.Mutex
	ended bool
}

func (v *personaValidator) ValidateRoleTest(context.Context, string, string, auth.Role) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ended {
		return errors.New("test ended")
	}
	return nil
}

func (v *personaValidator) end() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ended = true
}

// persona builds a role-test caller the way a stream does: the forwarded authority
// bindQueryAuthority stamps (component/grpc/work_authority.go) is on the context, which is
// what the cluster-acting calls below must not present as their own.
func persona(t *testing.T, id string, role auth.Role) context.Context {
	t.Helper()
	access := &auth.AccessContext{UserId: id, Role: role, RoleTestSessionID: "access-code-session"}
	assertion, err := auth.ForwardedAuthorityForUser(access, auth.RoleTestClass, "", time.Now().Add(time.Hour), time.Now())
	require.NoError(t, err)
	ctx := auth.BindForwardedContext(context.Background(), assertion.Principal().Claims, access, assertion)
	require.True(t, auth.IsRoleTestContext(ctx), "the premise failed: this context is not a persona's")
	return ctx
}

// A PERSONA IS JUDGED WHERE AN ACT IS AUTHORIZED, UNDER THE CALLER, AND NOWHERE ELSE. The
// store's own reads and updates run as the cluster acting, and a stream's forwarded
// authority names the persona: left on that context it is an assertion the service actor
// cannot honour, and every such call was refused, so a persona on their own site could not
// even list its codes. So it is blanked there, and that must not turn into a way around the
// persona's limits: each thing a persona could not otherwise do is refused here.
func TestATestPersonaIsJudgedWhereTheActIsAuthorized(t *testing.T) {
	w := newWorld(t, "persona")
	validator := &personaValidator{}
	auth.SetRoleTestValidator(validator)
	t.Cleanup(func() { auth.SetRoleTestValidator(nil) })

	developerID := auth.RoleTestUserPrefix + "developer-" + w.suffix
	writerID := auth.RoleTestUserPrefix + "writer-" + w.suffix
	w.principal(developerID, auth.RoleDeveloper)
	w.principal(writerID, auth.RoleWriter)
	developer := persona(t, developerID, auth.RoleDeveloper)
	writer := persona(t, writerID, auth.RoleWriter)

	own := "ac-own-" + w.suffix
	w.siteRow(own, map[string]any{"ownerUserId": "v1:identity:user:" + developerID})

	var codeID string
	t.Run("a developer persona does all of it on their own site, through the dispatch", func(t *testing.T) {
		rows, err := w.builtin(developer, "siteAccessCodeCreate", w.createArgs(map[string]any{"siteId": own}))
		require.NoError(t, err)
		require.Len(t, rows, 1)
		codeID = fmt.Sprint(rows[0]["codeId"])

		rows, err = w.builtin(developer, "siteAccessCodesForSite", map[string]any{"siteId": own})
		require.NoError(t, err, "a persona could not list a site with a code on it")
		require.Len(t, rows, 1)

		_, err = w.builtin(developer, "siteAccessCodeSetExpiry", map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)})
		require.NoError(t, err, "a persona could not re-time their own code")
		rows, err = w.builtin(developer, "siteAccessCodeRegenerate", map[string]any{"codeId": codeID})
		require.NoError(t, err, "a persona could not regenerate their own code")
		require.Equal(t, memql.AccessCodeDigest(fmt.Sprint(rows[0]["code"])), w.row(codeID)["codeHash"])

		row := w.row(codeID)
		require.Equal(t, developerID, bare(row["ownerUserId"]))
		require.Equal(t, developerID, bare(row["lastChangedBy"]), "the persona is who asked")

		_, err = w.builtin(developer, "siteAccessCodeRemove", map[string]any{"codeId": codeID})
		require.NoError(t, err, "a persona could not remove their own code")
		require.NotEmpty(t, w.row(codeID)["revokedAt"])
		require.Len(t, w.events(codeID), 4, "the persona's acts were not audited")
	})

	// Each row below is something a persona cannot do; the row above is what it can.
	theirs, _ := w.mint(actorContext(w.operator, auth.RoleOwner), nil)
	before := w.row(theirs)

	t.Run("a persona whose role lacks the part is refused at the gate", func(t *testing.T) {
		_, err := w.builtin(writer, "siteAccessCodeRegenerate", map[string]any{"codeId": theirs})
		require.Error(t, err)
		require.Contains(t, err.Error(), "capability_not_held")
	})

	t.Run("a persona the tier does not admit reads nothing, and so does nothing", func(t *testing.T) {
		// A stranger to the site's organization, with no gate in front of the handler: the
		// reads under the caller are all that stands here, and they still do.
		nodes, err := w.in.handleRegenerate(writer, map[string]any{"codeId": theirs}, 0)
		require.Nil(t, nodes, "a persona was handed a code on a site it cannot read")
		require.Contains(t, requireSentence(t, err), "No access code you can read")
		nodes, err = w.in.handleList(writer, map[string]any{"siteId": w.site}, 0)
		require.Nil(t, nodes)
		requireSentence(t, err)
		nodes, err = w.in.handleCreate(writer, w.createArgs(nil), 0)
		require.Nil(t, nodes, "a persona was handed a code on a site it cannot read")
		requireSentence(t, err)
	})

	t.Run("a persona cannot mint for an owner it is not, whatever the row tier admits", func(t *testing.T) {
		// The mint runs as the site's owner, and a persona may not borrow anybody else. It
		// is the one act a real developer can do here and this one cannot, so it is refused
		// in words that do not send them to try again.
		nodes, err := w.in.handleCreate(developer, w.createArgs(nil), 0)
		require.Nil(t, nodes, "a persona was handed a code for a site somebody else owns")
		require.Contains(t, requireSentence(t, err), "test persona")
	})
	require.Equal(t, before, w.row(theirs), "a refused persona changed the row")

	t.Run("a persona whose session has ended reaches no write", func(t *testing.T) {
		live := "ac-live-" + w.suffix
		w.forgeCode(own, live, w.account, time.Now(), time.Now().Add(time.Hour))
		beforeLive := w.row(live)
		writes := []string{"rotateSiteAccessCode", "setSiteAccessCodeExpiry", "removeSiteAccessCode"}
		acts := map[string]func(*Integration) ([]memorynodes.MemoryNode, error){
			"regenerate": func(in *Integration) ([]memorynodes.MemoryNode, error) {
				return in.handleRegenerate(developer, map[string]any{"codeId": live}, 0)
			},
			"set expiry": func(in *Integration) ([]memorynodes.MemoryNode, error) {
				return in.handleSetExpiry(developer, map[string]any{"codeId": live, "expiresAt": after(2 * time.Hour)}, 0)
			},
			"remove": func(in *Integration) ([]memorynodes.MemoryNode, error) {
				return in.handleRemove(developer, map[string]any{"codeId": live}, 0)
			},
		}

		// BETWEEN THE AUTHORIZING READ AND THE WRITE, the one window the engine's own check
		// on each call does not cover, since the write runs as somebody else.
		for name, act := range acts {
			t.Run(name+", ended after the read that authorized it", func(t *testing.T) {
				revocable := &personaValidator{}
				auth.SetRoleTestValidator(revocable)
				spy := &spyEngine{inner: &afterRead{inner: liveEngine{w.eng}, prefix: "query siteById(", onRead: revocable.end}}
				nodes, err := act(NewIntegration(spy, discardLogger()))
				require.Nil(t, nodes, "an ended persona was answered")
				requireSentence(t, err)
				for _, write := range writes {
					require.Empty(t, spy.named(write), "an ended persona reached %s", write)
				}
				require.Equal(t, beforeLive, w.row(live), "an ended persona changed the row")
			})
		}

		// AND WITH THE ANSWER IN HAND: a session that ended while the cluster's read ran was
		// the session it was for, and the row it read is not for anybody else.
		t.Run("the list, ended while it read when a code was last opened", func(t *testing.T) {
			// One code, so that this is the last read the list makes, and no later read
			// under the persona's own context can be what refuses it.
			single := "ac-own-one-code-" + w.suffix
			w.siteRow(single, map[string]any{"ownerUserId": "v1:identity:user:" + developerID})
			w.forgeCode(single, "ac-only-code-"+w.suffix, w.account, time.Now(), time.Now().Add(time.Hour))

			revocable := &personaValidator{}
			auth.SetRoleTestValidator(revocable)
			in := NewIntegration(&afterRead{inner: liveEngine{w.eng}, prefix: "query siteAccessSessionLatestForCode(", onRead: revocable.end}, discardLogger())
			nodes, err := in.handleList(developer, map[string]any{"siteId": single}, 0)
			require.Nil(t, nodes, "an ended persona was told when a code was last opened")
			requireSentence(t, err)
		})

		// AND ONCE IT HAS ENDED, every read under the caller refuses, so nothing starts.
		validator.end()
		auth.SetRoleTestValidator(validator)
		for name, act := range acts {
			t.Run(name+", ended before it began", func(t *testing.T) {
				spy := &spyEngine{inner: liveEngine{w.eng}}
				nodes, err := act(NewIntegration(spy, discardLogger()))
				require.Nil(t, nodes, "an ended persona was answered")
				requireSentence(t, err)
				for _, write := range writes {
					require.Empty(t, spy.named(write), "an ended persona reached %s", write)
				}
				require.Equal(t, beforeLive, w.row(live), "an ended persona changed the row")
			})
		}
	})
}

// ---------------------------------------------------------------------------
// a code is not minted inside a run
// ---------------------------------------------------------------------------

// codeInText is what a plain code looks like inside a row's text.
const codeInText = `[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){4}`

// THE ONE COPY OF A CODE IS THE REPLY, and inside a run the reply is not the end of it:
// the journal writes what a statement returned into rows that outlive the code. So a run
// is refused before anything is read or minted. Re-timing and removing carry no code, and
// a person's call is not in a run, which is the rest of this file.
func TestACodeIsNotMintedInsideARun(t *testing.T) {
	w := newWorld(t, "inrun")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	before := w.row(codeID)
	versions := w.versions(codeID, 0)

	inRun := common.ContextWithRun(operator, common.RunContext{RunId: "ac-run-" + w.suffix})
	_, stamped := common.RunFromContext(inRun)
	require.True(t, stamped, "the premise failed: the context carries no run")

	in, spy := w.spied()
	for name, act := range map[string]func() ([]memorynodes.MemoryNode, error){
		"minting": func() ([]memorynodes.MemoryNode, error) { return in.handleCreate(inRun, w.createArgs(nil), 0) },
		"regenerating": func() ([]memorynodes.MemoryNode, error) {
			return in.handleRegenerate(inRun, map[string]any{"codeId": codeID}, 0)
		},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			spy.reset()
			nodes, err := act()
			require.Nil(t, nodes, "a run was handed a code")
			msg := requireSentence(t, err)
			require.Contains(t, msg, "Deployables", "the refusal does not say where a code is minted")
			require.Contains(t, msg, "journal", "the refusal does not say why")
			require.Empty(t, spy.calls, "the refusal came after something was read or written")
		})
	}
	require.Equal(t, before, w.row(codeID), "a refused regenerate replaced the secret")
	require.Equal(t, versions, w.codesOn(w.site), "a refused mint wrote a code")

	t.Run("an act that carries no code may run in a run", func(t *testing.T) {
		_, err := in.handleSetExpiry(inRun, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
		require.NoError(t, err)
		_, err = in.handleList(inRun, map[string]any{"siteId": w.site}, 0)
		require.NoError(t, err)
		_, err = in.handleGates(inRun, map[string]any{"siteId": w.site}, 0)
		require.NoError(t, err)
		_, err = in.handleRemove(inRun, map[string]any{"codeId": codeID}, 0)
		require.NoError(t, err)
	})
}

// registerLogic compiles one logic and registers it as the loader would.
func (w *world) registerLogic(source string) string {
	w.t.Helper()
	normalised, err := langparser.NormaliseAll(source)
	require.NoError(w.t, err)
	file, err := langparser.ParseFile(normalised)
	require.NoError(w.t, err)
	fn, ok := file.Definitions[0].(*langparser.FunctionDef)
	require.True(w.t, ok, "the source is not a logic")
	body, problems := compiler.CompileBody("logic", fn.Name, nil, fn.Body.(*langparser.AutomationDef).Body)
	require.Empty(w.t, problems)
	require.NoError(w.t, w.eng.Functions().Upsert(&memql.Function{Name: fn.Name, FunctionKind: "logic", Enabled: true, LogicBody: body}))
	return fn.Name
}

// THROUGH THE REAL LOGIC RUNNER, because that is how the code reached the journal when
// review measured it: `minted := builtin siteAccessCodeCreate(...); return minted` wrote the
// plain code to v1:work:step and v1:work:run. A logic that regenerates a client's code and
// emails it is the natural thing for a pack to write, so the property is asserted of the
// rows themselves, under the journal this logic really writes to.
func TestALogicCannotPutACodeInItsJournal(t *testing.T) {
	w := newWorld(t, "logic")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	before := w.row(codeID)
	stamp := time.Now().UnixNano()
	started := time.Now().Add(-time.Second)

	journalRows := func(automation string) int {
		t.Helper()
		n, err := w.db.NewSelect().TableExpr(`"MemoryNodes"`).
			Where("concept = ?", "v1:work:run").Where("payload->>'automationName' = ?", automation).Count(context.Background())
		require.NoError(t, err)
		return int(n)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept IN (?, ?)", "v1:work:run", "v1:work:step").Where(`"createdAt" >= ?`, started).
			Where("payload::text LIKE ?", "%"+w.site+"%").Exec(ctx)
	})

	// THE CONTROL: this logic writes, so its run opens and journals what it returned. If
	// this finds nothing, the rows below prove nothing about a journal that is not on.
	retime := w.registerLogic(fmt.Sprintf(`logic retimeFromALogic%d {
  moved := builtin siteAccessCodeSetExpiry(codeId: %s, expiresAt: %s)
  return moved
}`, stamp, langparser.QuoteString(codeID), langparser.QuoteString(after(2*time.Hour))))
	_, err := w.eng.Execute(operator, retime+"()")
	require.NoError(t, err)
	require.NotZero(t, journalRows("logic:"+retime), "the logic's run was not journalled, so this test measures nothing")
	before = w.row(codeID)

	for name, source := range map[string]string{
		"minting": fmt.Sprintf(`logic mintFromALogic%d {
  minted := builtin siteAccessCodeCreate(siteId: %s, label: "From a logic", gate: "dashboard", expiresAt: %s)
  return minted
}`, stamp, langparser.QuoteString(w.site), langparser.QuoteString(after(time.Hour))),
		"regenerating": fmt.Sprintf(`logic regenerateFromALogic%d {
  minted := builtin siteAccessCodeRegenerate(codeId: %s)
  return minted
}`, stamp, langparser.QuoteString(codeID)),
	} {
		t.Run(name, func(t *testing.T) {
			logic := w.registerLogic(source)
			_, err := w.eng.Execute(operator, logic+"()")
			require.Error(t, err, "a logic was handed a code")
			require.Contains(t, err.Error(), "Deployables")
		})
	}

	// FIRST, because it is the point: with the refusal taken away this is what fails, on
	// the rows review found the code in.
	held, err := w.db.NewSelect().TableExpr(`"MemoryNodes"`).
		Where("concept LIKE ?", "v1:work:%").Where(`"createdAt" >= ?`, started).
		Where("payload::text ~ ?", codeInText).Count(context.Background())
	require.NoError(t, err)
	require.Zero(t, held, "a v1:work row holds a plain code")
	require.Equal(t, before, w.row(codeID), "a logic replaced a code's secret")
	require.Equal(t, 1, w.codesOnSiteDistinct(w.site), "a logic minted a code")
}

// ---------------------------------------------------------------------------
// a row's owner has no say
// ---------------------------------------------------------------------------

// transfer runs the real ownership transfer, as the caller given, and answers what it
// says it moved.
func (w *world) transfer(caller context.Context, args map[string]any) []map[string]any {
	w.t.Helper()
	integration := identity.NewIdentityIntegrationWithEngine(w.eng, func() *bun.DB { return w.db }, discardLogger())
	for _, capability := range integration.Capabilities() {
		if capability.Name != "transferRowOwnership" {
			continue
		}
		nodes, err := capability.Handler(caller, args, 0)
		require.NoError(w.t, err, "the transfer failed")
		require.Len(w.t, nodes, 1)
		reply := payloadOf(w.t, nodes[0])
		var moved []map[string]any
		if rows, ok := reply["rows"].([]any); ok {
			for _, row := range rows {
				moved = append(moved, row.(map[string]any))
			}
		}
		w.t.Cleanup(func() {
			_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).
				Where("concept = ?", "v1:identity:auditEvent").Where("payload->>'targetId' = ?", args["toUserId"]).Exec(context.Background())
		})
		return moved
	}
	w.t.Fatal("the identity integration has no transferRowOwnership")
	return nil
}

func (w *world) siteOwner(site string) string {
	w.t.Helper()
	var node memorynodes.MemoryNode
	require.NoError(w.t, w.db.NewSelect().Model(&node).
		Where("concept = ?", siteConcept).Where("id = ?", siteConcept+":"+site).
		Order("createdAt DESC").Limit(1).Scan(context.Background()))
	var payload map[string]any
	require.NoError(w.t, json.Unmarshal(node.Payload, &payload))
	return bare(payload["ownerUserId"])
}

func (w *world) sessionOwner(session string) string {
	w.t.Helper()
	var node memorynodes.MemoryNode
	require.NoError(w.t, w.db.NewSelect().Model(&node).
		Where("concept = ?", sessionConcept).Where("id = ?", sessionConcept+":"+session).
		Order("createdAt DESC").Limit(1).Scan(context.Background()))
	var payload map[string]any
	require.NoError(w.t, json.Unmarshal(node.Payload, &payload))
	return bare(payload["ownerUserId"])
}

// WHOLE-USER TRANSFER ONCE MOVED A CODE TO AN HEIR WHILE ITS TIED SITE KEPT ITS OWNER, and
// a lookup as the site's owner then missed it: a code an operator had handed out stopped
// opening, silently, because of an offboarding. Neither concept moves now, and a row's
// owner has no say in the lookups anyway, so this is asserted from both ends.
func TestAWholeUserTransferLeavesCodesAndSessionsWhereTheyWereStamped(t *testing.T) {
	w := newWorld(t, "wholeuser")
	operator := actorContext(w.operator, auth.RoleOwner)
	store := NewGraphStore(liveEngine{w.eng})
	background := context.Background()
	heir := "ac-heir-" + w.suffix
	w.principal(heir, auth.RoleWriter)

	codeID, code := w.mint(operator, nil)
	session := "ac-session-" + w.suffix
	token := memql.AccessSessionTokenDigest("whole-user-" + w.suffix)
	require.NoError(t, store.CreateSession(background, w.owner, Session{
		ID: session, SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now(),
	}, token))
	codeVersions := w.versions(codeID, 0)
	require.Equal(t, w.owner, bare(w.row(codeID)["ownerUserId"]))
	require.Equal(t, w.owner, w.sessionOwner(session))

	moved := w.transfer(operator, map[string]any{"fromUserId": w.owner, "toUserId": heir})

	// THE REACHABLE POSITIVE: it is a real transfer, and it found this person's rows.
	require.NotEmpty(t, moved, "the transfer moved nothing, so nothing here was at risk")
	for _, row := range moved {
		require.NotEqual(t, codeConcept, row["concept"], "the transfer moved a code")
		require.NotEqual(t, sessionConcept, row["concept"], "the transfer moved a session")
	}
	require.Equal(t, w.owner, bare(w.row(codeID)["ownerUserId"]), "the code moved")
	require.Equal(t, codeVersions, w.versions(codeID, 0), "the transfer wrote a version of the code")
	require.Equal(t, w.owner, w.sessionOwner(session), "the session moved")
	require.Equal(t, w.owner, w.siteOwner(w.site), "the premise failed: a tied site is what kept its owner")

	// AND IT STILL OPENS: the lookups the unlock and every later request make.
	found, err := store.CodeByHash(background, w.site, memql.AccessCodeDigest(code))
	require.NoError(t, err)
	require.NotNil(t, found, "the code stopped resolving after an offboarding")
	byID, err := store.CodeByID(background, codeID)
	require.NoError(t, err)
	require.NotNil(t, byID)
	open, err := store.SessionByTokenHash(background, token)
	require.NoError(t, err)
	require.NotNil(t, open, "an open session stopped resolving after an offboarding")
	require.NoError(t, store.CreateSession(background, w.siteOwner(w.site), Session{
		SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now(),
	}, memql.AccessSessionTokenDigest("after-"+w.suffix)))
}

// A TRANSFER OF THE SITE ALONE MOVES THE SITE AND NOT ITS CODES, on an untied site where
// nothing restores the owner. So the code's owner and the site's now differ for good, and
// a lookup as the site's owner would miss the code, and a session too. The codes have to
// keep working, and an operator has to be able to end them.
func TestASiteOnlyTransferLeavesItsCodesWorking(t *testing.T) {
	w := newWorld(t, "siteonly")
	operator := actorContext(w.operator, auth.RoleOwner)
	store := NewGraphStore(liveEngine{w.eng})
	background := context.Background()
	heir := "ac-heir-" + w.suffix
	w.principal(heir, auth.RoleWriter)
	untied := "ac-untied-" + w.suffix
	w.siteRow(untied, map[string]any{"accountId": nil})

	codeID, code := w.mint(operator, map[string]any{"siteId": untied})
	opened := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	session := "ac-session-" + w.suffix
	token := memql.AccessSessionTokenDigest("site-only-" + w.suffix)
	require.NoError(t, store.CreateSession(background, w.owner, Session{
		ID: session, SiteID: untied, CodeID: codeID, Generation: 1, IssuedAt: opened,
	}, token))

	// AS THE CLUSTER ACTING, because a person cannot move this row: the organization rule
	// refuses any change to an untied site until it is attributed (organization_required),
	// and a tied site keeps its owner whatever a transfer says. So this is how a site's
	// owner comes to differ from the one its codes were stamped with.
	moved := w.transfer(seedContext(), map[string]any{
		"fromUserId": w.owner, "toUserId": heir, "concept": siteConcept, "rowId": siteConcept + ":" + untied,
	})
	require.Len(t, moved, 1, "the transfer did not move the site")

	// THE PREMISE: the two now disagree.
	require.Equal(t, heir, w.siteOwner(untied), "the site did not move")
	require.Equal(t, w.owner, bare(w.row(codeID)["ownerUserId"]), "the code moved with the site")
	require.Equal(t, w.owner, w.sessionOwner(session), "the session moved with the site")

	// THE CODE STILL OPENS THE SITE, and a session opened before the transfer still resolves.
	found, err := store.CodeByHash(background, untied, memql.AccessCodeDigest(code))
	require.NoError(t, err)
	require.NotNil(t, found, "a code stopped opening its site because the site changed hands")
	open, err := store.SessionByTokenHash(background, token)
	require.NoError(t, err)
	require.NotNil(t, open, "an open session stopped resolving because its site changed hands")

	// THE NEW OWNER'S VISITORS OPEN SESSIONS STAMPED WITH THE NEW OWNER, and "last opened"
	// reads across both, because it reads as the cluster and not as either of them.
	later := opened.Add(30 * time.Second)
	require.NoError(t, store.CreateSession(background, heir, Session{
		SiteID: untied, CodeID: codeID, Generation: 1, IssuedAt: later,
	}, memql.AccessSessionTokenDigest("site-only-later-"+w.suffix)))
	listed, err := w.in.handleList(operator, map[string]any{"siteId": untied}, 0)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, stamp(later), payloadOf(t, listed[0])["lastOpenedAt"], "last opened missed a session stamped with the site's new owner")

	// AN OPERATOR CAN STILL REGENERATE IT AND REMOVE IT, and neither moves its owner.
	nodes, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err, "a code could not be regenerated because its site changed hands")
	fresh := fmt.Sprint(onlyPayload(t, nodes)["code"])
	require.Equal(t, memql.AccessCodeDigest(fresh), w.row(codeID)["codeHash"])
	require.Equal(t, w.owner, bare(w.row(codeID)["ownerUserId"]), "regenerating moved the row's owner")
	found, err = store.CodeByHash(background, untied, memql.AccessCodeDigest(fresh))
	require.NoError(t, err)
	require.NotNil(t, found)

	_, err = w.in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err, "a code could not be removed because its site changed hands")
	row := w.row(codeID)
	require.NotEmpty(t, row["revokedAt"])
	require.Equal(t, w.owner, bare(row["ownerUserId"]), "removing moved the row's owner")
}

// ---------------------------------------------------------------------------
// the audit event does not wait on the request
// ---------------------------------------------------------------------------

// A STREAM DROPPED IN THE MOMENT AFTER THE WRITE has cancelled the request's context, and
// the act happened regardless. The record of it has to land anyway: a regenerate with no
// event is a credential replaced with nobody's name on it.
func TestAStreamDroppedAfterTheWriteStillRecordsTheAct(t *testing.T) {
	w := newWorld(t, "dropped")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)

	// Each act answers the code it landed on, because a mint's is not known until it has.
	acts := []struct {
		name string
		act  func(*Integration, context.Context) (string, error)
	}{
		{"minting", func(in *Integration, ctx context.Context) (string, error) {
			nodes, err := in.handleCreate(ctx, w.createArgs(map[string]any{"label": "Minted as the stream dropped"}), 0)
			if err != nil {
				return "", err
			}
			return fmt.Sprint(onlyPayload(t, nodes)["codeId"]), nil
		}},
		{"regenerating", func(in *Integration, ctx context.Context) (string, error) {
			_, err := in.handleRegenerate(ctx, map[string]any{"codeId": codeID}, 0)
			return codeID, err
		}},
		{"re-timing", func(in *Integration, ctx context.Context) (string, error) {
			_, err := in.handleSetExpiry(ctx, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
			return codeID, err
		}},
		{"removing", func(in *Integration, ctx context.Context) (string, error) {
			_, err := in.handleRemove(ctx, map[string]any{"codeId": codeID}, 0)
			return codeID, err
		}},
	}
	for _, tc := range acts {
		t.Run(tc.name, func(t *testing.T) {
			in, spy := w.spied()
			ctx, cancel := context.WithCancel(operator)
			defer cancel()
			// THE MOMENT: the write has landed, and the audit event is about to be written.
			var (
				atTheWrite error
				bounded    bool
			)
			spy.before = func(call recordedCall) {
				if call.construct() == "createAuditEvent" {
					cancel()
					atTheWrite = call.ctx.Err()
					_, bounded = call.ctx.Deadline()
				}
			}
			before := len(w.events(codeID))
			landedOn, err := tc.act(in, ctx)
			require.NoError(t, err)
			require.Error(t, ctx.Err(), "the premise failed: the request was not cancelled")

			require.Len(t, spy.named("createAuditEvent"), 1)
			require.NoError(t, atTheWrite, "the audit write was left waiting on the request that was dropped")
			require.True(t, bounded, "a write that outlives its request has no bound of its own")
			if landedOn == codeID {
				require.Len(t, w.events(codeID), before+1, "the act happened and its audit event did not land")
			} else {
				require.Len(t, w.events(landedOn), 1, "the mint happened and its audit event did not land")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// a spy in front of the real engine
// ---------------------------------------------------------------------------

type recordedCall struct {
	query string
	ctx   context.Context
}

func (c recordedCall) construct() string {
	q := strings.TrimPrefix(strings.TrimPrefix(c.query, "query "), "mutation ")
	if i := strings.Index(q, "("); i > 0 {
		return q[:i]
	}
	return q
}

func (c recordedCall) actor() string {
	ac, _ := auth.AccessFromContext(c.ctx)
	if ac == nil {
		return ""
	}
	return memql.BareShortId(ac.UserId)
}

func (c recordedCall) internal() bool { return auth.OriginFromContext(c.ctx).IsInternal() }

// spyEngine records every call, and can do something to the world just before
// one reaches the real engine. The call itself always lands.
type spyEngine struct {
	inner  Engine
	mu     sync.Mutex
	calls  []recordedCall
	before func(call recordedCall)
}

func (s *spyEngine) Execute(ctx context.Context, query string) (any, error) {
	call := recordedCall{query: query, ctx: ctx}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	before := s.before
	s.mu.Unlock()
	if before != nil {
		before(call)
	}
	return s.inner.Execute(ctx, query)
}

func (s *spyEngine) named(construct string) []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedCall
	for _, call := range s.calls {
		if call.construct() == construct {
			out = append(out, call)
		}
	}
	return out
}

func (s *spyEngine) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

func (w *world) spied() (*Integration, *spyEngine) {
	spy := &spyEngine{inner: liveEngine{w.eng}}
	return NewIntegration(spy, discardLogger()), spy
}

// WHO EACH CALL RUNS AS IS THE WHOLE OF THE AUTHORIZATION STORY (store.go's file
// note), and a wrong answer here does not fail loudly. A code written under the
// caller is owned by the caller. A site read under internal origin would hand a
// caller-scoped read the engine's escape. A session read under a member comes back
// empty and reads as "Not opened yet". So it is asserted call by call.
func TestEachReadAndWriteRunsAsTheActorItMust(t *testing.T) {
	w := newWorld(t, "actors")
	in, spy := w.spied()
	caller := actorContext(w.operator, auth.RoleOwner)
	theCluster := "system:" + systemActorName

	expect := func(t *testing.T, construct, actor string, internal bool) recordedCall {
		t.Helper()
		calls := spy.named(construct)
		require.NotEmpty(t, calls, "%s was not called", construct)
		for _, call := range calls {
			require.Equal(t, actor, call.actor(), "%s ran as the wrong actor", construct)
			require.Equal(t, internal, call.internal(), "%s ran with the wrong origin", construct)
		}
		return calls[0]
	}

	var codeID string
	t.Run("minting", func(t *testing.T) {
		nodes, err := in.handleCreate(caller, w.createArgs(nil), 0)
		require.NoError(t, err)
		codeID = fmt.Sprint(onlyPayload(t, nodes)["codeId"])
		expect(t, "siteById", w.operator, false)
		expect(t, "packageById", w.operator, false)
		write := expect(t, "createSiteAccessCode", w.owner, true)
		require.True(t, memql.HasRowVersionFence(write.ctx, siteConcept, w.site), "a mint carries no witness of the site it copied from")
		expect(t, "createAuditEvent", w.operator, true)
	})

	acts := []struct {
		name  string
		write string
		act   func() error
	}{
		{"regenerating", "rotateSiteAccessCode", func() error {
			_, err := in.handleRegenerate(caller, map[string]any{"codeId": codeID}, 0)
			return err
		}},
		{"re-timing", "setSiteAccessCodeExpiry", func() error {
			_, err := in.handleSetExpiry(caller, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
			return err
		}},
		{"removing", "removeSiteAccessCode", func() error {
			_, err := in.handleRemove(caller, map[string]any{"codeId": codeID}, 0)
			return err
		}},
	}
	for _, tc := range acts {
		t.Run(tc.name, func(t *testing.T) {
			spy.reset()
			require.NoError(t, tc.act())
			expect(t, "siteAccessCodeById", w.operator, true)
			expect(t, "siteById", w.operator, false)
			write := expect(t, tc.write, theCluster, true)
			require.True(t, memql.HasRowVersionFence(write.ctx, codeConcept, codeID), "%s carries no witness of the row it decided from", tc.write)
			expect(t, "createAuditEvent", w.operator, true)
		})
	}

	t.Run("listing", func(t *testing.T) {
		spy.reset()
		_, err := in.handleList(caller, map[string]any{"siteId": w.site}, 0)
		require.NoError(t, err)
		expect(t, "siteById", w.operator, false)
		expect(t, "siteAccessCodeRowsForSite", w.operator, true)
		expect(t, "siteAccessSessionLatestForCode", theCluster, true)
	})

	t.Run("the gates", func(t *testing.T) {
		spy.reset()
		_, err := in.handleGates(caller, map[string]any{"siteId": w.site}, 0)
		require.NoError(t, err)
		expect(t, "siteById", w.operator, false)
		expect(t, "packageById", w.operator, false)
	})

	t.Run("the store reads and updates as the cluster, and creates as the owner it is told, with no caller at all", func(t *testing.T) {
		spy.reset()
		store := NewGraphStore(spy)
		background := context.Background()
		_, err := store.CodeByHash(background, w.site, memql.AccessCodeDigest("nothing"))
		require.NoError(t, err)
		_, err = store.CodeByID(background, codeID)
		require.NoError(t, err)
		_, err = store.SessionByTokenHash(background, memql.AccessSessionTokenDigest("nothing"))
		require.NoError(t, err)
		session := "ac-actors-session-" + w.suffix
		require.NoError(t, store.CreateSession(background, w.owner, Session{
			ID: session, SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now(),
		}, memql.AccessSessionTokenDigest("actors"+w.suffix)))
		require.NoError(t, store.EndSession(background, session, time.Now()))
		for _, construct := range []string{"siteAccessCodeByHash", "siteAccessCodeById", "siteAccessSessionByToken", "endSiteAccessSession"} {
			expect(t, construct, theCluster, true)
		}
		expect(t, "createSiteAccessSession", w.owner, true)
	})
}

// ---------------------------------------------------------------------------
// the fence
// ---------------------------------------------------------------------------

// A LOSS IS RE-READ AND RETRIED ONCE, THEN REPORTED. Each case moves the row the
// act's witness names in the instant before its write, which is the window the
// fence exists for, and the engine's real fence refuses the write.
func TestAFenceLossIsRetriedOnceThenReported(t *testing.T) {
	type outcome struct {
		code string // the plain code the reply carried, when there is one
	}
	cases := []struct {
		name string
		// write is the construct whose call the interference waits for.
		write string
		// read is the construct that gives the act its witness.
		read string
		// setup returns the id of the row the witness names.
		setup func(w *world, caller context.Context) string
		act   func(in *Integration, caller context.Context, w *world, target string) (outcome, error)
		// landed says whether the row shows the act.
		landed func(t *testing.T, w *world, target string, got outcome) bool
	}{
		{
			name: "minting", write: "createSiteAccessCode", read: "siteById",
			setup: func(w *world, _ context.Context) string { return w.site },
			act: func(in *Integration, caller context.Context, w *world, _ string) (outcome, error) {
				nodes, err := in.handleCreate(caller, w.createArgs(nil), 0)
				if err != nil {
					return outcome{}, err
				}
				return outcome{code: fmt.Sprint(onlyPayload(w.t, nodes)["code"])}, nil
			},
			landed: func(t *testing.T, w *world, _ string, got outcome) bool {
				return got.code != "" && w.codesOn(w.site) == 1
			},
		},
		{
			name: "regenerating", write: "rotateSiteAccessCode", read: "siteAccessCodeById",
			setup: func(w *world, caller context.Context) string { id, _ := w.mint(caller, nil); return id },
			act: func(in *Integration, caller context.Context, w *world, target string) (outcome, error) {
				nodes, err := in.handleRegenerate(caller, map[string]any{"codeId": target}, 0)
				if err != nil {
					return outcome{}, err
				}
				return outcome{code: fmt.Sprint(onlyPayload(w.t, nodes)["code"])}, nil
			},
			landed: func(t *testing.T, w *world, target string, got outcome) bool {
				return got.code != "" && w.row(target)["codeHash"] == memql.AccessCodeDigest(got.code)
			},
		},
		{
			name: "re-timing", write: "setSiteAccessCodeExpiry", read: "siteAccessCodeById",
			setup: func(w *world, caller context.Context) string { id, _ := w.mint(caller, nil); return id },
			act: func(in *Integration, caller context.Context, w *world, target string) (outcome, error) {
				_, err := in.handleSetExpiry(caller, map[string]any{"codeId": target, "expiresAt": after(30 * time.Hour)}, 0)
				return outcome{}, err
			},
			landed: func(t *testing.T, w *world, target string, _ outcome) bool {
				at, err := time.Parse(time.RFC3339, fmt.Sprint(w.row(target)["expiresAt"]))
				require.NoError(t, err)
				return at.After(time.Now().Add(29 * time.Hour))
			},
		},
		{
			name: "removing", write: "removeSiteAccessCode", read: "siteAccessCodeById",
			setup: func(w *world, caller context.Context) string { id, _ := w.mint(caller, nil); return id },
			act: func(in *Integration, caller context.Context, w *world, target string) (outcome, error) {
				_, err := in.handleRemove(caller, map[string]any{"codeId": target}, 0)
				return outcome{}, err
			},
			landed: func(t *testing.T, w *world, target string, _ outcome) bool {
				return fmt.Sprint(w.row(target)["revokedAt"]) != "<nil>"
			},
		},
	}

	for _, tc := range cases {
		// interfere moves the row the witness names: the site with a raw write, a
		// code by re-timing it, as a second operator's act would.
		interfere := func(w *world, target string, n int) {
			if tc.write == "createSiteAccessCode" {
				w.insert(siteConcept, target, map[string]any{"title": fmt.Sprintf("touched %d", n)})
				return
			}
			w.mutate(w.asOwner(), "setSiteAccessCodeExpiry", map[string]any{
				"codeId": target, "expiresAt": after(time.Duration(5+n) * time.Hour), "lastChangedBy": w.member,
			})
		}
		arm := func(w *world, spy *spyEngine, target string, losses int) *int {
			moved := 0
			spy.before = func(call recordedCall) {
				if call.construct() == tc.write && moved < losses {
					moved++
					interfere(w, target, moved)
				}
			}
			return &moved
		}

		t.Run(tc.name+": one loss is retried, and the act lands", func(t *testing.T) {
			w := newWorld(t, "fenceone")
			in, spy := w.spied()
			caller := actorContext(w.operator, auth.RoleOwner)
			target := tc.setup(w, caller)
			arm(w, spy, target, 1)

			got, err := tc.act(in, caller, w, target)
			require.NoError(t, err, "one loss was not retried")
			require.Len(t, spy.named(tc.write), 2, "the write should have been tried twice")
			require.Len(t, spy.named(tc.read), 2, "the retry did not read again, so it decided from the read that had just gone stale")
			require.True(t, tc.landed(t, w, target, got), "the act reported success and the row does not show it")
			require.Len(t, spy.named("createAuditEvent"), 1, "one act is one audit event")
		})

		t.Run(tc.name+": a second loss is reported in a sentence, and nothing landed", func(t *testing.T) {
			w := newWorld(t, "fencetwo")
			in, spy := w.spied()
			caller := actorContext(w.operator, auth.RoleOwner)
			target := tc.setup(w, caller)
			moved := arm(w, spy, target, 10)

			got, err := tc.act(in, caller, w, target)
			require.Error(t, err, "an act reported success for a write that did not land")
			require.True(t, errors.Is(err, memql.ErrRowVersionChanged), "a caller cannot tell a conflict from any other failure: %v", err)
			msg := requireSentence(t, err)
			require.Contains(t, msg, "try again", "the conflict does not say what to do")
			require.Equal(t, 2, *moved, "the act kept trying past its one retry")
			require.Len(t, spy.named(tc.write), 2)
			require.False(t, tc.landed(t, w, target, got), "a write that lost twice is in the row")
			require.Empty(t, spy.named("createAuditEvent"), "an act that did not land was audited")
		})
	}

	// THE RETRY DECIDES AGAIN. It is not a blind repeat: what lost may have been an
	// act that makes this one illegal, or one that already did what this one is for.
	t.Run("a regenerate racing a removal is refused on its retry, and does not undo it", func(t *testing.T) {
		w := newWorld(t, "fencerace")
		in, spy := w.spied()
		caller := actorContext(w.operator, auth.RoleOwner)
		target, _ := w.mint(caller, nil)
		digest := w.row(target)["codeHash"]
		once := false
		spy.before = func(call recordedCall) {
			if call.construct() == "rotateSiteAccessCode" && !once {
				once = true
				w.removeCode(target)
			}
		}
		nodes, err := in.handleRegenerate(caller, map[string]any{"codeId": target}, 0)
		require.Nil(t, nodes)
		require.Contains(t, requireSentence(t, err), "add a new code", "the retry should have read the removal and refused for it")
		require.False(t, errors.Is(err, memql.ErrRowVersionChanged), "this is a decision, not a conflict")
		row := w.row(target)
		require.NotEmpty(t, row["revokedAt"], "the regenerate undid the removal")
		require.Equal(t, digest, row["codeHash"], "a removed code was given a new secret")
	})

	t.Run("a remove racing a removal reports success without a second write", func(t *testing.T) {
		w := newWorld(t, "fenceremove")
		in, spy := w.spied()
		caller := actorContext(w.operator, auth.RoleOwner)
		target, _ := w.mint(caller, nil)
		once := false
		spy.before = func(call recordedCall) {
			if call.construct() == "removeSiteAccessCode" && !once {
				once = true
				w.removeCode(target)
			}
		}
		_, err := in.handleRemove(caller, map[string]any{"codeId": target}, 0)
		require.NoError(t, err, "the code is removed, which is what was asked")
		require.Len(t, spy.named("removeSiteAccessCode"), 1)
		require.Empty(t, spy.named("createAuditEvent"), "this act wrote nothing, so it has nothing to audit")
	})

	// THE MINT'S WITNESS IS WHAT CLOSES THE MOVE (D15): a site that changed
	// organization between the read and the write must not end up with a code
	// copying the organization it left.
	t.Run("a mint racing a move copies the organization the site moved to", func(t *testing.T) {
		w := newWorld(t, "fencemove")
		beta, betaGroup := "ac-acct-beta-"+w.suffix, "ac-group-beta-"+w.suffix
		w.organization(beta, betaGroup)
		in, spy := w.spied()
		caller := actorContext(w.operator, auth.RoleOwner)
		once := false
		spy.before = func(call recordedCall) {
			if call.construct() == "createSiteAccessCode" && !once {
				once = true
				w.insert(siteConcept, w.site, map[string]any{"accountId": beta})
			}
		}
		nodes, err := in.handleCreate(caller, w.createArgs(nil), 0)
		require.NoError(t, err)
		stored := w.row(fmt.Sprint(onlyPayload(t, nodes)["codeId"]))
		require.Equal(t, beta, bare(stored["accountId"]), "the mint copied the organization the site had left, so the code would open nothing")
	})
}

// WITHOUT THE WITNESS, two regenerates that both read generation 1 both report
// success at generation 2, and the first operator holds a code that no longer
// works with nothing to tell them so.
func TestTwoRegeneratesFromOneReadDoNotBothSucceed(t *testing.T) {
	t.Run("two writes decided from one read", func(t *testing.T) {
		w := newWorld(t, "tworeads")
		caller := actorContext(w.operator, auth.RoleOwner)
		store := NewGraphStore(liveEngine{w.eng})
		codeID, _ := w.mint(caller, nil)
		read, ok, err := store.codeForCaller(caller, codeID)
		require.NoError(t, err)
		require.True(t, ok)

		write := func(seed string) error {
			return store.rotateCode(context.Background(), rotation{
				CodeID: codeID, Digest: memql.AccessCodeDigest(seed), Hint: "ZZZZ", Generation: read.Generation + 1,
				IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), ChangedBy: w.operator,
			}, read.Version)
		}
		require.NoError(t, write("first"), "a write decided from the current version was refused, so no act could ever land")
		err = write("second")
		require.True(t, errors.Is(err, memql.ErrRowVersionChanged), "the second write from one read was not refused: %v", err)
		require.Equal(t, memql.AccessCodeDigest("first"), w.row(codeID)["codeHash"], "the refused write changed the code")
		require.Equal(t, 1, w.versions(codeID, 2), "two writes landed at generation 2")
	})

	t.Run("two operators released at the same read", func(t *testing.T) {
		// Each round holds both handlers after their first read, so both decide from
		// the same version, and then lets both go. Exactly one is told it worked, and
		// the code that one was handed is the code that works. A plain retry would land
		// the loser at the next generation and silently kill what the winner is holding.
		const rounds = 8
		for round := 0; round < rounds; round++ {
			w := newWorld(t, "twoops")
			caller := actorContext(w.operator, auth.RoleOwner)
			codeID, _ := w.mint(caller, nil)

			var mu sync.Mutex
			reads := 0
			both := make(chan struct{})
			barrier := &afterRead{inner: liveEngine{w.eng}, onRead: func() {
				mu.Lock()
				reads++
				n := reads
				mu.Unlock()
				if n == 2 {
					close(both)
				}
				if n <= 2 {
					<-both
				}
			}}
			in := NewIntegration(barrier, discardLogger())

			type result struct {
				nodes []memorynodes.MemoryNode
				err   error
			}
			results := make(chan result, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					nodes, err := in.handleRegenerate(caller, map[string]any{"codeId": codeID}, 0)
					results <- result{nodes: nodes, err: err}
				}()
			}
			wg.Wait()
			close(results)

			var reported []string
			for r := range results {
				if r.err != nil {
					require.True(t, errors.Is(r.err, memql.ErrRowVersionChanged), "the loser was refused for something else: %v", r.err)
					require.Contains(t, requireSentence(t, r.err), "Someone else regenerated", "the loser is not told what happened")
					continue
				}
				reported = append(reported, fmt.Sprint(onlyPayload(t, r.nodes)["code"]))
			}
			require.Len(t, reported, 1, "from one read, exactly one regenerate succeeds")

			row := w.row(codeID)
			require.EqualValues(t, 2, row["generation"])
			require.Equal(t, 1, w.versions(codeID, 2), "two writes landed at generation 2, one operator's code silently dead")
			require.Zero(t, w.versions(codeID, 3), "the loser's retry replaced the code the winner was handed")
			require.Equal(t, memql.AccessCodeDigest(reported[0]), row["codeHash"], "the operator told it worked holds a code that does not")
			require.Len(t, w.events(codeID), 2, "the mint and the one regenerate that happened")
		}
	})
}

// afterRead lets a test do something right after a handler has made one read: hold it
// after it has read the code it is about to decide from, say. With no prefix given, that
// is the read it waits for.
type afterRead struct {
	inner  Engine
	prefix string
	onRead func()
}

func (a *afterRead) Execute(ctx context.Context, query string) (any, error) {
	res, err := a.inner.Execute(ctx, query)
	prefix := a.prefix
	if prefix == "" {
		prefix = "query siteAccessCodeById("
	}
	if strings.HasPrefix(query, prefix) {
		a.onRead()
	}
	return res, err
}

// A WITNESS APPLIES TO EVERY WRITE UNDER ITS CONTEXT, and the code's own write has
// just moved the row it names. An audit write carrying it is refused, and refused
// quietly, since a failed audit write does not fail the act: the operator sees a
// success and the log is missing the event. So the events are asserted, and so is
// the context each was written under.
func TestTheAuditWriteIsNotFenced(t *testing.T) {
	w := newWorld(t, "auditfence")
	in, spy := w.spied()
	caller := actorContext(w.operator, auth.RoleOwner)

	nodes, err := in.handleCreate(caller, w.createArgs(nil), 0)
	require.NoError(t, err)
	codeID := fmt.Sprint(onlyPayload(t, nodes)["codeId"])
	_, err = in.handleRegenerate(caller, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
	_, err = in.handleSetExpiry(caller, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
	require.NoError(t, err)
	_, err = in.handleRemove(caller, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)

	audits := spy.named("createAuditEvent")
	require.Len(t, audits, 4)
	for _, call := range audits {
		require.False(t, memql.HasRowVersionFence(call.ctx, codeConcept, codeID), "an audit write carries the code's witness")
		require.False(t, memql.HasRowVersionFence(call.ctx, siteConcept, w.site), "an audit write carries the site's witness")
	}
	require.Len(t, w.events(codeID), 4, "an act reported success and its audit event never landed")
}

// ---------------------------------------------------------------------------
// the organization gate, through the real dispatch
// ---------------------------------------------------------------------------

func TestAnActThroughAnotherOrganizationIsRefused(t *testing.T) {
	w := newWorld(t, "otherorg")
	w.installGrants()

	// THE PERSON HOLDS THE PART IN THE WORLD'S ACCOUNT ONLY, and OWNS a deployable in
	// Beta, so the row tier's owner arm admits them to its code. Only the shared gate
	// stands between them and an act there.
	person := "ac-person-" + w.suffix
	w.principal(person, auth.RoleWriter)
	w.join(w.group, person)
	w.grantPart(w.group)
	beta, betaGroup := "ac-acct-beta-"+w.suffix, "ac-group-beta-"+w.suffix
	w.organization(beta, betaGroup)
	betaSite := "ac-beta-site-" + w.suffix
	w.siteRow(betaSite, map[string]any{"accountId": beta, "ownerUserId": "v1:identity:user:" + person})
	betaCode := "ac-beta-code-" + w.suffix
	_, digest, hint, err := memql.MintAccessCode()
	require.NoError(t, err)
	issued := time.Now()
	w.mutate(auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), person)), "createSiteAccessCode", map[string]any{
		"codeId": betaCode, "siteId": betaSite, "accountId": beta, "label": "Beta's", "gate": "dashboard",
		"codeHash": digest, "codeHint": hint, "issuedAt": stamp(issued), "expiresAt": stamp(issued.Add(time.Hour)),
		"lastChangedBy": person,
	})

	caller := actorContext(person, auth.RoleWriter)
	store := NewGraphStore(liveEngine{w.eng})
	read, ok, err := store.codeForCaller(caller, betaCode)
	require.NoError(t, err)
	require.True(t, ok, "the premise failed: the row tier does not admit this person, so the gate below is not the only thing in the way")
	require.Equal(t, beta, read.AccountID)

	before := w.row(betaCode)
	for _, act := range []struct {
		builtin string
		args    map[string]any
	}{
		{"siteAccessCodeCreate", map[string]any{"siteId": betaSite, "label": "Probe", "gate": "dashboard", "expiresAt": after(time.Hour)}},
		{"siteAccessCodesForSite", map[string]any{"siteId": betaSite}},
		{"siteAccessGates", map[string]any{"siteId": betaSite}},
		{"siteAccessCodeRegenerate", map[string]any{"codeId": betaCode}},
		{"siteAccessCodeSetExpiry", map[string]any{"codeId": betaCode, "expiresAt": after(2 * time.Hour)}},
		{"siteAccessCodeRemove", map[string]any{"codeId": betaCode}},
	} {
		t.Run(act.builtin+" is refused", func(t *testing.T) {
			rows, err := w.builtin(caller, act.builtin, act.args)
			require.Error(t, err, "%s was admitted", act.builtin)
			require.Nil(t, rows)
			require.Contains(t, err.Error(), "capability_not_held", "%s was refused, but not by the gate", act.builtin)
			require.Equal(t, before, w.row(betaCode), "%s changed the row", act.builtin)
			require.Zero(t, w.eventsOnSite(betaSite), "%s was audited", act.builtin)
			require.Empty(t, w.events(betaCode), "%s was audited", act.builtin)
		})
	}

	// THE REACHABLE POSITIVE, end to end through the dispatch: the same person, in the
	// organization that granted the part, can do all of it.
	rows, err := w.builtin(caller, "siteAccessCodeCreate", w.createArgs(nil))
	require.NoError(t, err, "the person was refused in the organization that granted the part")
	require.Len(t, rows, 1)
	codeID := fmt.Sprint(rows[0]["codeId"])
	require.Equal(t, memql.AccessCodeDigest(fmt.Sprint(rows[0]["code"])), w.row(codeID)["codeHash"])

	rows, err = w.builtin(caller, "siteAccessGates", map[string]any{"siteId": w.site})
	require.NoError(t, err)
	gates := make([]string, 0, len(rows))
	for _, row := range rows {
		gates = append(gates, fmt.Sprint(row["gate"]))
	}
	sort.Strings(gates)
	require.Equal(t, []string{"clients", "dashboard"}, gates)

	rows, err = w.builtin(caller, "siteAccessCodesForSite", map[string]any{"siteId": w.site})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, codeID, rows[0]["id"])

	_, err = w.builtin(caller, "siteAccessCodeSetExpiry", map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)})
	require.NoError(t, err)
	_, err = w.builtin(caller, "siteAccessCodeRegenerate", map[string]any{"codeId": codeID})
	require.NoError(t, err)
	_, err = w.builtin(caller, "siteAccessCodeRemove", map[string]any{"codeId": codeID})
	require.NoError(t, err)
	require.Len(t, w.events(codeID), 4)
}

// ---------------------------------------------------------------------------
// the rows the ordinary path does not make
// ---------------------------------------------------------------------------

// A SITE WITH NO ORGANIZATION IS A LEGAL SITE, and its codes carry no copy. The
// comparison that finds a stale code must read two blanks as agreement, or every code
// on such a site would be refused as left behind by a move that never happened.
func TestACodeOfASiteWithNoOrganizationCarriesNoCopy(t *testing.T) {
	w := newWorld(t, "untied")
	operator := actorContext(w.operator, auth.RoleOwner)
	w.siteRow(w.site+"-untied", map[string]any{"accountId": nil})
	codeID, _ := w.mint(operator, map[string]any{"siteId": w.site + "-untied"})

	stored := w.row(codeID)
	_, copied := stored["accountId"]
	require.False(t, copied, "a code of a site with no organization carries one: %v", stored)
	_, subject := stored["subject"]
	require.False(t, subject, "a code minted with no subject carries one, as empty rather than absent: %v", stored)

	_, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err, "a code on a site with no organization was read as a stale copy")
	_, err = w.in.handleSetExpiry(operator, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
	require.NoError(t, err)

	nodes, err := w.in.handleList(operator, map[string]any{"siteId": w.site + "-untied"}, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	_, listed := payloadOf(t, nodes[0])["accountId"]
	require.False(t, listed, "the list invented an organization")
}

// A GATE CAN GO AWAY FROM UNDER A CODE: a domain dropped from a bundle stops declaring
// it. The code opens nothing from then on, which is the bff's to refuse, and the code
// must still list, and still be removable, or a person is left with a row they can
// neither see nor end. Only minting asks whether the gate is declared.
func TestACodeWhoseGateIsGoneCanStillBeSeenAndEnded(t *testing.T) {
	w := newWorld(t, "gategone")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	memql.SetAccessGatesForTest()

	nodes, err := w.in.handleList(operator, map[string]any{"siteId": w.site}, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1, "a code whose gate went away vanished from the list")
	require.Equal(t, "dashboard", payloadOf(t, nodes[0])["gate"])

	gates, err := w.in.handleGates(operator, map[string]any{"siteId": w.site}, 0)
	require.NoError(t, err)
	require.Empty(t, gates)

	_, err = w.in.handleCreate(operator, w.createArgs(nil), 0)
	require.Contains(t, requireSentence(t, err), "no gates", "a new code was minted for a gate nobody declares")

	_, err = w.in.handleSetExpiry(operator, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
	require.NoError(t, err)
	_, err = w.in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err, "a code whose gate went away could not be ended")
	require.NotEmpty(t, w.row(codeID)["revokedAt"])
}

// ---------------------------------------------------------------------------
// nothing answered from a cache
// ---------------------------------------------------------------------------

// THE SAME CALL TWICE IS TWO ACTS AND TWO READS. A double click on Add sends two
// identical statements inside a second, and the panel re-lists with the same text
// after every write. If the engine answered either from its result cache, the first
// would hand one code to two rows' worth of intent, and the second would show a row
// that had just changed as though it had not. Through the dispatch, because that is
// where a cache would sit.
func TestTheSameCallTwiceIsAnsweredTwice(t *testing.T) {
	w := newWorld(t, "twice")
	operator := actorContext(w.operator, auth.RoleOwner)
	args := w.createArgs(nil)

	first, err := w.builtin(operator, "siteAccessCodeCreate", args)
	require.NoError(t, err)
	second, err := w.builtin(operator, "siteAccessCodeCreate", args)
	require.NoError(t, err)
	require.NotEqual(t, first[0]["code"], second[0]["code"], "the same statement was answered from a cache, so one code stands for two")
	require.NotEqual(t, first[0]["codeId"], second[0]["codeId"])
	require.Equal(t, 2, w.codesOn(w.site), "each call is a code of its own")

	list := map[string]any{"siteId": w.site}
	before, err := w.builtin(operator, "siteAccessCodesForSite", list)
	require.NoError(t, err)
	require.Len(t, before, 2)
	codeID := fmt.Sprint(first[0]["codeId"])
	_, err = w.builtin(operator, "siteAccessCodeRemove", map[string]any{"codeId": codeID})
	require.NoError(t, err)
	after, err := w.builtin(operator, "siteAccessCodesForSite", list)
	require.NoError(t, err)
	removed := false
	for _, row := range after {
		if row["id"] == codeID {
			removed = row["revokedAt"] != nil
		}
	}
	require.True(t, removed, "the list did not show a removal the moment it landed: %v", after)
}

// ---------------------------------------------------------------------------
// the store the bff reads through
// ---------------------------------------------------------------------------

func TestTheStoreFindsCodesAndSessions(t *testing.T) {
	w := newWorld(t, "store")
	operator := actorContext(w.operator, auth.RoleOwner)
	store := NewGraphStore(liveEngine{w.eng})
	background := context.Background()

	codeID, code := w.mint(operator, map[string]any{"gate": "clients", "subject": "acme"})
	digest := memql.AccessCodeDigest(code)

	got, err := store.CodeByHash(background, w.site, digest)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, codeID, got.ID, "the id comes back bare")
	require.Equal(t, w.site, got.SiteID, "the site comes back bare")
	require.Equal(t, w.account, got.AccountID, "the account comes back bare: the handler compares it with the site's")
	require.Equal(t, "clients", got.Gate)
	require.Equal(t, "acme", got.Subject)
	require.Equal(t, 1, got.Generation)
	require.False(t, got.Revoked)
	require.WithinDuration(t, time.Now().Add(time.Hour), got.ExpiresAt, time.Minute)

	byID, err := store.CodeByID(background, codeID)
	require.NoError(t, err)
	require.Equal(t, got, byID, "the two lookups disagree about one row")

	t.Run("a miss is nil and nil", func(t *testing.T) {
		for name, read := range map[string]func() (*Code, error){
			"an unknown digest": func() (*Code, error) {
				return store.CodeByHash(background, w.site, memql.AccessCodeDigest("nope"))
			},
			"another site":  func() (*Code, error) { return store.CodeByHash(background, w.site+"-other", digest) },
			"an unknown id": func() (*Code, error) { return store.CodeByID(background, codeID+"-nope") },
		} {
			found, err := read()
			require.NoError(t, err, name)
			require.Nil(t, found, name)
		}
		session, err := store.SessionByTokenHash(background, memql.AccessSessionTokenDigest("nope"))
		require.NoError(t, err)
		require.Nil(t, session)
	})

	t.Run("a removed code still answers, and says so", func(t *testing.T) {
		removed, removedCode := w.mint(operator, map[string]any{"label": "Removed"})
		_, err := w.in.handleRemove(operator, map[string]any{"codeId": removed}, 0)
		require.NoError(t, err)
		found, err := store.CodeByHash(background, w.site, memql.AccessCodeDigest(removedCode))
		require.NoError(t, err)
		require.NotNil(t, found)
		require.True(t, found.Revoked)
	})

	t.Run("a session is written in the one form the sort needs, whatever it is given", func(t *testing.T) {
		// THE ENGINE ORDERS issuedAt AS TEXT: `...:20Z` sorts after `...:20.5Z`, and an
		// offset sorts by its digits. So the form is enforced here rather than trusted
		// to a caller, and this is that caller getting it wrong in every way it can.
		zone := time.FixedZone("somewhere", 2*60*60)
		given := time.Date(2026, 10, 30, 12, 34, 56, 987654321, zone)
		token := memql.AccessSessionTokenDigest("written-" + w.suffix)
		session := "ac-session-form-" + w.suffix
		require.NoError(t, store.CreateSession(background, w.owner, Session{
			ID: session, SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: given,
		}, token))

		var node memorynodes.MemoryNode
		require.NoError(t, w.db.NewSelect().Model(&node).
			Where("concept = ?", sessionConcept).Where("id = ?", sessionConcept+":"+session).
			Order("createdAt DESC").Limit(1).Scan(background))
		var payload map[string]any
		require.NoError(t, json.Unmarshal(node.Payload, &payload))
		require.Equal(t, "2026-10-30T10:34:56Z", payload["issuedAt"], "issuedAt is UTC RFC 3339 at whole seconds, always")
		require.Equal(t, token, payload["tokenHash"])
		require.Equal(t, w.owner, bare(payload["ownerUserId"]), "the session is the site's owner's")

		found, err := store.SessionByTokenHash(background, token)
		require.NoError(t, err)
		require.NotNil(t, found)
		require.Equal(t, session, found.ID)
		require.Equal(t, w.site, found.SiteID)
		require.Equal(t, codeID, found.CodeID)
		require.Equal(t, 1, found.Generation)
		require.True(t, found.IssuedAt.Equal(given.Truncate(time.Second)))
		require.False(t, found.Ended)

		// Ending it keeps the row, says so, and leaves it the owner it was stamped with.
		require.NoError(t, store.EndSession(background, session, time.Now()))
		found, err = store.SessionByTokenHash(background, token)
		require.NoError(t, err)
		require.NotNil(t, found, "an ended session must still resolve, so the handler can say 'ended'")
		require.True(t, found.Ended)
		require.Equal(t, w.owner, w.sessionOwner(session), "ending a session moved its owner")
	})

	t.Run("a session with no id is given one", func(t *testing.T) {
		token := memql.AccessSessionTokenDigest("unnamed-" + w.suffix)
		require.NoError(t, store.CreateSession(background, w.owner, Session{
			SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now(),
		}, token))
		found, err := store.SessionByTokenHash(background, token)
		require.NoError(t, err)
		require.NotNil(t, found)
		require.NotEmpty(t, found.ID)
	})

	t.Run("a session with nobody to belong to is refused", func(t *testing.T) {
		require.Error(t, store.CreateSession(background, "", Session{
			SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now(),
		}, memql.AccessSessionTokenDigest("ownerless-"+w.suffix)))
		require.Error(t, store.CreateSession(background, w.owner, Session{
			SiteID: w.site, CodeID: codeID, Generation: 1,
		}, memql.AccessSessionTokenDigest("timeless-"+w.suffix)), "a session with no issuedAt was written")
	})
}

// ---------------------------------------------------------------------------
// what must never be said
// ---------------------------------------------------------------------------

// failingEngine answers the write it is told to with an error that repeats the
// statement it was given, which is what a parse error or a validator can do, and
// everything else from the real engine.
type failingEngine struct {
	inner Engine
	fail  string
}

func (f *failingEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, f.fail) {
		return nil, fmt.Errorf("the engine choked on %s", query)
	}
	return f.inner.Execute(ctx, query)
}

// NEVER A CODE, A TOKEN OR A DIGEST IN A LOG LINE, AN AUDIT EVENT OR AN ERROR. The
// statements this package renders carry digests, and an engine error may repeat a
// statement. So every failure is made to do exactly that, and what comes out the
// other side is searched.
func TestNoFailureCarriesASecret(t *testing.T) {
	w := newWorld(t, "secrets")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, code := w.mint(operator, nil)
	stored := fmt.Sprint(w.row(codeID)["codeHash"])
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	clean := func(t *testing.T, text string) {
		t.Helper()
		require.NotContains(t, text, code)
		require.NotContains(t, text, stored)
		require.False(t, anyDigest.MatchString(text), "a digest is in: %q", text)
	}
	// silent holds a log line to a fixed reason and an id. Taking the digests out of an
	// engine error that repeats its statement would still leave the operator's label, the
	// client's subject and the hint in it, so none of the engine's words may get there.
	silent := func(t *testing.T, text string) {
		t.Helper()
		clean(t, text)
		require.NotContains(t, text, "the engine choked", "the engine's own text reached the log")
		require.NotContains(t, text, "A label nobody logs", "an operator's label reached the log")
		require.NotContains(t, text, "a-subject-nobody-logs", "a client's subject reached the log")
		require.NotContains(t, text, "codeHint", "a code's hint reached the log")
	}

	for _, fail := range []string{"createSiteAccessCode", "rotateSiteAccessCode", "setSiteAccessCodeExpiry", "removeSiteAccessCode"} {
		t.Run("a failed "+fail, func(t *testing.T) {
			in := NewIntegration(&failingEngine{inner: liveEngine{w.eng}, fail: fail}, logger)
			var err error
			switch fail {
			case "createSiteAccessCode":
				_, err = in.handleCreate(operator, w.createArgs(map[string]any{"label": "A label nobody logs", "subject": "a-subject-nobody-logs"}), 0)
			case "rotateSiteAccessCode":
				_, err = in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
			case "setSiteAccessCodeExpiry":
				_, err = in.handleSetExpiry(operator, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
			case "removeSiteAccessCode":
				_, err = in.handleRemove(operator, map[string]any{"codeId": codeID}, 0)
			}
			clean(t, requireSentence(t, err))
			silent(t, logged.String())
			require.Contains(t, logged.String(), "siteaccess: an act on an access code failed", "the failure was not logged, so nobody can find out it happened")
			require.Contains(t, logged.String(), "reason=", "the log line gives no reason")
			logged.Reset()
		})
	}

	t.Run("a failed audit write does not fail the act, and says so only in the log", func(t *testing.T) {
		in := NewIntegration(&failingEngine{inner: liveEngine{w.eng}, fail: "createAuditEvent"}, logger)
		nodes, err := in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
		require.NoError(t, err, "the row is already written, so failing here would lose the only copy of the new code")
		fresh := fmt.Sprint(onlyPayload(t, nodes)["code"])
		require.Equal(t, memql.AccessCodeDigest(fresh), w.row(codeID)["codeHash"])
		text := logged.String()
		require.Contains(t, text, "audit", "a failed audit write was swallowed without a word")
		require.Contains(t, text, codeID, "the log does not say which code went unaudited")
		require.Contains(t, text, "reason=", "the log line gives no reason")
		require.NotContains(t, text, fresh)
		require.NotContains(t, text, memql.AccessCodeDigest(fresh))
		silent(t, text)
		logged.Reset()
	})

	t.Run("the store's own errors", func(t *testing.T) {
		store := NewGraphStore(&failingEngine{inner: liveEngine{w.eng}, fail: "("})
		digest := memql.AccessCodeDigest("something-else-" + w.suffix)
		token := memql.AccessSessionTokenDigest("something-" + w.suffix)
		_, err := store.CodeByHash(context.Background(), w.site, digest)
		require.Error(t, err)
		require.NotContains(t, err.Error(), digest)
		clean(t, err.Error())
		_, err = store.SessionByTokenHash(context.Background(), token)
		require.Error(t, err)
		require.NotContains(t, err.Error(), token)
		clean(t, err.Error())
		err = store.CreateSession(context.Background(), w.owner, Session{SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now()}, token)
		require.Error(t, err)
		require.NotContains(t, err.Error(), token)
		clean(t, err.Error())
	})

	t.Run("a mint logs nothing of what it minted on the way through", func(t *testing.T) {
		in := NewIntegration(liveEngine{w.eng}, logger)
		nodes, err := in.handleCreate(operator, w.createArgs(nil), 0)
		require.NoError(t, err)
		fresh := fmt.Sprint(onlyPayload(t, nodes)["code"])
		require.NotContains(t, logged.String(), fresh)
		require.NotContains(t, logged.String(), memql.AccessCodeDigest(fresh))
	})
}

// A BUILTIN'S ARGUMENTS ARE NOT VALIDATED BEFORE IT RUNS: the dispatch hands the
// handler what the call said, so a declared `datetime!` can arrive as anything or
// not at all. Asserted through the dispatch, where it is true.
func TestTheHandlersCheckTheirOwnArguments(t *testing.T) {
	w := newWorld(t, "arguments")
	operator := actorContext(w.operator, auth.RoleOwner)
	codeID, _ := w.mint(operator, nil)
	before := w.row(codeID)
	for name, act := range map[string]struct {
		builtin string
		args    map[string]any
	}{
		"create with an expiry that is not a date": {"siteAccessCodeCreate", w.createArgs(map[string]any{"expiresAt": "whenever"})},
		"create with no gate":                      {"siteAccessCodeCreate", w.createArgs(map[string]any{"gate": nil})},
		"regenerate with an expiry that is a word": {"siteAccessCodeRegenerate", map[string]any{"codeId": codeID, "expiresAt": "whenever"}},
		"re-time with no expiry":                   {"siteAccessCodeSetExpiry", map[string]any{"codeId": codeID}},
		"re-time with a number":                    {"siteAccessCodeSetExpiry", map[string]any{"codeId": codeID, "expiresAt": 12345}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.builtin(operator, act.builtin, act.args)
			requireSentence(t, err)
			require.Equal(t, before, w.row(codeID), "the call changed the row")
		})
	}
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/verifier"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/integrations/siteaccess"
)

// access_surface_db_test.go -- a visitor and a deployable's access surface, end to
// end: the real verifier middleware, the real mount, the real store, the real engine
// and a real Postgres, with a real gated query and mutation in a throwaway DSL domain
// (design record 2026-10-29-deployable-access-codes, sections 6 to 9).
//
// WHY HERE, and why this package's tier. component/server may not import integrations/
// and has no engine, so it can only hold the handler to what fakes answer. The claims
// below are about what the graph really answers: that the tier's account arm really
// admits a forged owner, that a regenerate really stops the old digest finding its row,
// that a mutation's node really does carry the payload and the concept's description,
// that an id really does arrive canonical and really leaves bare. A fake would answer
// each of those whichever way its author believed.
//
// WITH NO BUILD TAG, because the bff-tagged lane has no database and the db-gated lane
// compiles this package untagged: a tagged test here would skip wherever it ran.
//
// Postgres-gated: a skip is what an unreachable database produces, and
// MEMQL_REQUIRE_DB=1 turns it into a failure.

const (
	accessE2EDomain     = "accesssurfacee2e"
	accessE2EDeployable = "access-surface-e2e"
	accessE2EConcept    = "v1:" + accessE2EDomain + ":accessBrief"

	// accessE2EDescription is in the concept's own doc comment, so it is in the schema
	// on every node of that concept. A reply holding it is a reply that handed over a
	// node rather than a row.
	accessE2EDescription = "THE AUTHOR'S NOTE ABOUT THIS CONCEPT, WHICH NO VISITOR READS"
)

func accessE2ETree() fstest.MapFS {
	concepts := `use platform.concepts.{ site }

/// ` + accessE2EDescription + `.
@rowAuthz(owner="ownerUserId")
concept accessBrief {
  ownerUserId    string
  accessCodeId   string
  clientName     string
  siteId         string
  note           string
  secretNote     string

  @relationship(type="references", as="briefOfSite", field="siteId", target=site, direction="outgoing")
}
`
	shapes := `@row
shape accessBrief accessBriefRow {
  row.id
  accessCodeId
  clientName
  siteId
  note
}
`
	queries := `@accessGate(deployable="` + accessE2EDeployable + `", gate="dashboard")
@actor
@cache(0)
query accessBrief accessBriefs {
  args {
    accessCodeId string!
  }
  filter row => row.ownerUserId == actor.userId && row.accessCodeId == args.accessCodeId
  paginate 20
  shape accessBriefRow
}

@accessGate(deployable="` + accessE2EDeployable + `", gate="clients")
@actor
@cache(0)
query accessBrief accessClientBriefs {
  args {
    accessSubject string
  }
  filter row => row.ownerUserId == actor.userId && row.clientName == args.accessSubject
  paginate 20
  shape accessBriefRow
}

@accessGate(deployable="` + accessE2EDeployable + `", gate="dashboard")
@actor
@cache(0)
query accessBrief accessBriefsUnshaped {
  args {
    accessCodeId string!
  }
  filter row => row.ownerUserId == actor.userId && row.accessCodeId == args.accessCodeId
  paginate 20
}

@accessGate(deployable="` + accessE2EDeployable + `", gate="dashboard")
@actor
query accessBrief accessBriefsCached {
  args {
    accessCodeId string!
  }
  filter row => row.ownerUserId == actor.userId && row.accessCodeId == args.accessCodeId
  paginate 20
  shape accessBriefRow
}

@accessGate(deployable="some-other-deployable", gate="dashboard")
@actor
@cache(0)
query accessBrief accessBriefsOfAnotherDeployable {
  args {
    accessCodeId string!
  }
  filter row => row.ownerUserId == actor.userId && row.accessCodeId == args.accessCodeId
  paginate 20
  shape accessBriefRow
}
`
	mutations := `@accessGate(deployable="` + accessE2EDeployable + `", gate="dashboard")
@actor
mutation accessBrief accessBriefRecord {
  args {
    noteId       string!
    accessCodeId string!
    siteId       string!
    note         string!
    secretNote   string
  }
  insert {
    accept { note, secretNote }
    stamp {
      id:           args.noteId
      accessCodeId: args.accessCodeId
      siteId:       args.siteId
      ownerUserId:  actor.userId
    }
  }
}

@accessGate(deployable="` + accessE2EDeployable + `", gate="clients")
@actor
mutation accessBrief accessClientRecord {
  args {
    noteId        string!
    siteId        string!
    accessCodeId  string!
    accessGate    string!
    accessSubject string
  }
  insert {
    accept { siteId }
    stamp {
      id:           args.noteId
      accessCodeId: args.accessCodeId
      clientName:   args.accessSubject
      note:         args.accessGate
      ownerUserId:  actor.userId
    }
  }
}
`
	return fstest.MapFS{
		dslfs.ManifestFile: {Data: []byte(dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render())},
		"concepts.memql":   {Data: []byte(concepts)},
		"shapes.memql":     {Data: []byte(shapes)},
		"queries.memql":    {Data: []byte(queries)},
		"mutations.memql":  {Data: []byte(mutations)},
	}
}

// accessSurfaceRig is the one engine and the one mount every case shares: a boot costs
// seconds, and the worlds below stay out of each other's way by their ids.
type accessSurfaceRig struct {
	eng        *memql.MemQLEngine
	db         *bun.DB
	surface    http.Handler
	logs       *bytes.Buffer
	engineLogs *bytes.Buffer
}

func bootAccessSurface(t *testing.T) *accessSurfaceRig {
	t.Helper()
	// The domain's concept lands in the process-wide registry and its gates in the
	// process-wide view, so both go back as they were found, for the next test to boot.
	conceptsBefore, gatesBefore := memorynodes.All(), memql.AccessGateEntries()
	dsl.RegisterTree(accessE2EDomain, accessE2ETree())
	t.Cleanup(func() {
		dsl.UnregisterTree(accessE2EDomain)
		memorynodes.ReplaceAll(conceptsBefore)
		memql.SetAccessGatesForTest(gatesBefore...)
	})

	eng, db := workTemplateDBEngineAndDB(t)

	// THE LOGS OF BOTH, at their most talkative, because "never logged" is a claim about
	// every line and not only the handler's own. The engine's goes on before the
	// integration is built from it, so what the operator's acts log is in it as well.
	rig := &accessSurfaceRig{eng: eng, db: db, logs: &bytes.Buffer{}, engineLogs: &bytes.Buffer{}}
	eng.Logger = slog.New(slog.NewTextHandler(rig.engineLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name == "siteAccess" {
			provider, err := plugin.Factory(memql.PluginContext{Engine: eng, Logger: eng.Logger})
			require.NoError(t, err)
			require.NoError(t, eng.RegisterIntegration(provider))
		}
	}
	a := &App{
		mux: http.NewServeMux(), engine: eng,
		Logger:           slog.New(slog.NewTextHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		identityVerifier: &verifier.Verifier{},
	}
	a.mountAccessSurface()
	// THE REAL MIDDLEWARE AHEAD OF THE REAL MUX. A request with no bearer gets to the
	// handler only because the third tier says so, and it is the one thing no handler
	// test can show by accident.
	rig.surface = a.identityHTTPMiddleware()(a.mux)
	return rig
}

// accessWorld is one case's deployable and the people around it.
type accessWorld struct {
	t   *testing.T
	rig *accessSurfaceRig

	suffix   string
	owner    string
	operator string
	member   string
	stranger string
	account  string
	group    string
	pkg      string
	site     string

	rows []string
}

func accessSeedContext() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "access-surface-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "access-surface-seeder"})
}

func accessPersonContext(user string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: user})
}

func newAccessWorld(t *testing.T, rig *accessSurfaceRig) *accessWorld {
	t.Helper()
	s := id.NewShortId()
	w := &accessWorld{
		t: t, rig: rig, suffix: s,
		owner: "as-owner-" + s, operator: "as-operator-" + s, member: "as-member-" + s, stranger: "as-stranger-" + s,
		account: "as-acct-" + s, group: "as-group-" + s, pkg: "as-pkg-" + s, site: "as-site-" + s,
	}
	t.Cleanup(w.sweep)
	w.principal(w.owner, auth.RoleWriter)
	w.principal(w.operator, auth.RoleOwner)
	w.principal(w.member, auth.RoleWriter)
	w.principal(w.stranger, auth.RoleWriter)
	w.insert("v1:accounts:account", w.account, map[string]any{"name": w.account, "status": "active", "domainStatus": "unverified"})
	w.mutate(accessSeedContext(), "writeGroup", map[string]any{
		"groupId": w.group, "name": w.group, "kind": "account", "accountId": w.account, "status": "active",
	})
	w.rows = append(w.rows, "v1:identity:group:"+w.group)
	for _, user := range []string{w.owner, w.member} {
		membership := w.group + "-" + user
		w.mutate(accessSeedContext(), "writeGroupMembership", map[string]any{
			"membershipId": membership, "groupId": w.group, "userId": user, "origin": "added", "status": "active",
			"accountId": w.account,
		})
		w.rows = append(w.rows, "v1:identity:groupMembership:"+membership)
	}
	w.packageRow(w.pkg, []string{accessE2EDomain})
	w.siteRow(w.site, w.pkg, w.account)
	return w
}

// sweep takes away what this case wrote. The ids isolate one case from another; this
// is for the shared lane's table. Codes and sessions are found by their site, because
// the handlers mint their ids, and briefs by their owner, because the visitor names them.
func (w *accessWorld) sweep() {
	ctx := context.Background()
	for _, row := range w.rows {
		_, _ = w.rig.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", row).Exec(ctx)
	}
	// By the site and not the owner, since a case here writes one under somebody else.
	for _, concept := range []string{"v1:platform:siteAccessCode", "v1:platform:siteAccessSession"} {
		_, _ = w.rig.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", concept).
			Where("payload->>'siteId' LIKE ?", "%"+w.suffix).Exec(ctx)
	}
	_, _ = w.rig.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", accessE2EConcept).
		Where("payload->>'ownerUserId' LIKE ?", "%"+w.owner).Exec(ctx)
	_, _ = w.rig.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", "v1:identity:auditEvent").
		Where("payload#>>'{detail,siteId}' LIKE ?", "%"+w.suffix).Exec(ctx)
}

func (w *accessWorld) insert(concept, rowID string, payload map[string]any) {
	w.t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(w.t, err)
	_, err = w.rig.eng.Execute(accessSeedContext(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(concept), langparser.QuoteString(rowID), encoded))
	require.NoError(w.t, err, "seeding %s %s", concept, rowID)
	w.rows = append(w.rows, concept+":"+memql.BareShortId(rowID))
}

func (w *accessWorld) principal(user string, role auth.Role) {
	w.t.Helper()
	w.insert("v1:identity:user", user, map[string]any{
		"displayName": user, "primaryEmail": user + "@example.test", "role": string(role), "active": true,
	})
}

func (w *accessWorld) packageRow(pkg string, domains []string) {
	w.t.Helper()
	payload := map[string]any{"name": pkg, "sourceKind": "repo", "status": "active", "ownerUserId": w.owner, "accountId": w.account}
	if domains != nil {
		payload["dslDomains"] = domains
	}
	w.insert("v1:platform:package", pkg, payload)
}

// siteRow writes a site as a raw insert, for the reason integrations/siteaccess's own
// fixture gives: the cases are about sites a flow would not make.
func (w *accessWorld) siteRow(site, pkg, account string) {
	w.t.Helper()
	w.insert("v1:platform:site", site, map[string]any{
		"hostname": site + ".example.test", "kind": "spa", "status": "draft", "bundleRef": "blob://access-surface/v1",
		"ownerUserId": "v1:identity:user:" + w.owner, "accountId": account,
		"packageId": "v1:platform:package:" + pkg, "packageDeployableName": accessE2EDeployable,
	})
}

func (w *accessWorld) mutate(ctx context.Context, name string, args map[string]any) {
	w.t.Helper()
	call, err := langparser.RenderCall(name, args)
	require.NoError(w.t, err)
	_, err = w.rig.eng.Execute(ctx, "mutation "+call)
	require.NoError(w.t, err, "mutation %s", name)
}

func (w *accessWorld) asOwner() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), w.owner))
}

// builtin is an operator's act, through the engine's dispatch, as a person's call is.
func (w *accessWorld) builtin(name string, args map[string]any) map[string]any {
	w.t.Helper()
	call, err := langparser.RenderCall(name, args)
	require.NoError(w.t, err)
	result, err := w.rig.eng.Execute(accessPersonContext(w.operator, auth.RoleOwner), "builtin "+call)
	require.NoError(w.t, err, "builtin %s", name)
	rows := memql.MaterializeRows(result)
	require.Len(w.t, rows, 1, "builtin %s", name)
	return rows[0]
}

// mint is an operator minting a code, so every code a case starts from is one a person
// could have.
func (w *accessWorld) mint(site, gate, subject string) (codeID, code string) {
	w.t.Helper()
	args := map[string]any{
		"siteId": site, "label": "Reviewers " + id.NewShortId(), "gate": gate,
		"expiresAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	if subject != "" {
		args["subject"] = subject
	}
	reply := w.builtin("siteAccessCodeCreate", args)
	codeID, _ = reply["codeId"].(string)
	code, _ = reply["code"].(string)
	require.NotEmpty(w.t, codeID)
	require.NotEmpty(w.t, code)
	return memql.BareShortId(codeID), code
}

// forgeCode writes a code straight through the mutation, for the rows no operator's
// act would mint: an out-of-date copy of the account, a gate nobody declares.
func (w *accessWorld) forgeCode(site, account, gate string) (codeID, code string) {
	w.t.Helper()
	return w.forgeCodeAs(w.asOwner(), site, account, gate)
}

// forgeCodeAs writes it under whoever ctx names, so its row can belong to somebody who
// is not the site's owner.
func (w *accessWorld) forgeCodeAs(ctx context.Context, site, account, gate string) (codeID, code string) {
	w.t.Helper()
	code, digest, hint, err := memql.MintAccessCode()
	require.NoError(w.t, err)
	codeID = "as-forged-" + id.NewShortId()
	args := map[string]any{
		"codeId": codeID, "siteId": site, "label": "Forged", "gate": gate, "codeHash": digest, "codeHint": hint,
		"issuedAt": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), "lastChangedBy": w.owner,
		"expiresAt": time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339),
	}
	if account != "" {
		args["accountId"] = account
	}
	w.mutate(ctx, "createSiteAccessCode", args)
	return codeID, code
}

// moveExpiry re-times a code past every bound the operator's act enforces, which is how
// a case gets one already expired: time expires a code, and these do not wait.
func (w *accessWorld) moveExpiry(codeID string, to time.Time) {
	w.t.Helper()
	w.mutate(w.asOwner(), "setSiteAccessCodeExpiry", map[string]any{
		"codeId": codeID, "expiresAt": to.UTC().Truncate(time.Second).Format(time.RFC3339), "lastChangedBy": w.owner,
	})
}

// storedCode reads a code's newest row off the table: what was stored is the question,
// and no shape would answer it.
func (w *accessWorld) storedCode(codeID string) map[string]any {
	w.t.Helper()
	var stored []byte
	require.NoError(w.t, w.rig.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("payload").
		Where("id = ?", "v1:platform:siteAccessCode:"+codeID).
		OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background(), &stored))
	var row map[string]any
	require.NoError(w.t, json.Unmarshal(stored, &row))
	return row
}

// send is one request from a browser, through the edge's stamp.
func (w *accessWorld) send(method, path, body, token, site, owner string) *httptest.ResponseRecorder {
	w.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if site != "" {
		req.Header.Set(server.AccessStampSiteHeader, site)
	}
	if owner != "" {
		req.Header.Set(server.AccessStampOwnerHeader, owner)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: server.AccessCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	w.rig.surface.ServeHTTP(rec, req)
	return rec
}

func (w *accessWorld) unlockOn(site, code string) *httptest.ResponseRecorder {
	w.t.Helper()
	body, err := json.Marshal(map[string]string{"code": code})
	require.NoError(w.t, err)
	return w.send(http.MethodPost, "/access/unlock", string(body), "", site, "v1:identity:user:"+w.owner)
}

func (w *accessWorld) unlock(code string) *httptest.ResponseRecorder {
	w.t.Helper()
	return w.unlockOn(w.site, code)
}

var accessE2ECookie = regexp.MustCompile(`^memql_access=(mql_acs_[A-Za-z0-9_-]{43}); Path=/_memql/access; HttpOnly; Secure; SameSite=Strict$`)

func (w *accessWorld) open(code string) string {
	w.t.Helper()
	rec := w.unlock(code)
	require.Equal(w.t, http.StatusOK, rec.Code, rec.Body.String())
	return accessE2EToken(w.t, rec)
}

func accessE2EToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	match := accessE2ECookie.FindStringSubmatch(rec.Header().Get("Set-Cookie"))
	require.NotNil(t, match, "Set-Cookie = %q", rec.Header().Get("Set-Cookie"))
	return match[1]
}

func (w *accessWorld) session(token string) *httptest.ResponseRecorder {
	w.t.Helper()
	return w.send(http.MethodGet, "/access/session", "", token, w.site, "v1:identity:user:"+w.owner)
}

func (w *accessWorld) leave(token string) *httptest.ResponseRecorder {
	w.t.Helper()
	return w.send(http.MethodPost, "/access/leave", "{}", token, w.site, "v1:identity:user:"+w.owner)
}

func (w *accessWorld) call(token, construct string, args map[string]any) *httptest.ResponseRecorder {
	w.t.Helper()
	body, err := json.Marshal(map[string]any{"call": construct, "args": args})
	require.NoError(w.t, err)
	return w.send(http.MethodPost, "/access/call", string(body), token, w.site, "v1:identity:user:"+w.owner)
}

// accessActingEngine is the real engine with something landing as the call finishes: it
// does the work first, so the rows are in hand, and only then runs the act, once.
type accessActingEngine struct {
	inner  server.MemQLExecutor
	after  func()
	landed int
}

func (e *accessActingEngine) Execute(ctx context.Context, query string) (any, error) {
	result, err := e.inner.Execute(ctx, query)
	if e.after != nil {
		after := e.after
		e.after = nil
		e.landed++
		after()
	}
	return result, err
}

func requireAccessReply(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	require.Equal(t, body, rec.Body.String())
}

func requireAccessCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	raw := rec.Header().Get("Set-Cookie")
	require.Contains(t, raw, "memql_access=;")
	require.Contains(t, raw, "Max-Age=0")
	require.Contains(t, raw, "Path=/_memql/access")
}

func accessDataRows(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotNil(t, body.Data, "data must be an array: %s", rec.Body.String())
	return body.Data
}

func TestAccessSurfaceEndToEnd(t *testing.T) {
	rig := bootAccessSurface(t)
	require.NotNil(t, memql.AccessGateFor(accessE2EDeployable, "dashboard", "accessBriefs", []string{accessE2EDomain}),
		"the throwaway domain's gate did not load")

	t.Run("a stranger learns nothing and nobody's bearer is asked for", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		rec := w.session("")
		requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"none"}`)
		// The verifier really is in front of the mux: what is not one of the four exact
		// routes does not get past it.
		rec = w.send(http.MethodGet, "/access/nope", "", "", w.site, w.owner)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "the verifier stepped aside for a route nobody exempted")
		// And a request the edge did not stamp reaches nothing.
		rec = w.send(http.MethodGet, "/access/session", "", "", "", "")
		requireAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)
	})

	t.Run("unlock, list and call, with every id bare", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		codeID, code := w.mint(w.site, "dashboard", "")

		// Typed the way people type. A real code, a real digest, a real lookup.
		rec := w.unlock(strings.ToLower(strings.ReplaceAll(code, "-", " ")))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		token := accessE2EToken(t, rec)
		var opened struct {
			Gate        string `json:"gate"`
			Subject     string `json:"subject"`
			ExpiresAt   string `json:"expiresAt"`
			RemainingMs int64  `json:"remainingMs"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &opened))
		require.Equal(t, "dashboard", opened.Gate)
		require.Empty(t, opened.Subject)
		require.InDelta(t, time.Hour.Milliseconds(), opened.RemainingMs, float64(time.Minute.Milliseconds()))
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

		// THE ROW HOLDS THE DIGEST AND THE ONE FORM OF THE TIME THE SORT NEEDS. Read off
		// the table, since what was stored is the question, and no shape would answer it.
		var stored []byte
		require.NoError(t, rig.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("payload").
			Where("concept = ?", "v1:platform:siteAccessSession").
			Where("payload->>'tokenHash' = ?", memql.AccessSessionTokenDigest(token)).
			Limit(1).Scan(context.Background(), &stored))
		var row map[string]any
		require.NoError(t, json.Unmarshal(stored, &row))
		require.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, row["issuedAt"], "issuedAt is not whole-second UTC, and the latest-session sort compares it as text")
		require.NotContains(t, string(stored), token, "the row holds the plain token")

		// Still in, on the next request, by the cookie alone.
		require.Equal(t, http.StatusOK, w.session(token).Code)

		// THE MUTATION'S REPLY IS ONLY ITS BARE ID. Its node has every payload field, the
		// creator and the concept's description in it, which is what this holds off.
		noteID := "as-note-" + w.suffix
		rec = w.call(token, "accessBriefRecord", map[string]any{"noteId": noteID, "note": "visible", "secretNote": "only-in-the-row"})
		requireAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"`+noteID+`"}]}`)

		// IT REALLY RAN AS THE OWNER, with what was stamped, on the row itself.
		var note []byte
		var createdBy string
		require.NoError(t, rig.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr(`payload, "createdBy"`).
			Where("id = ?", accessE2EConcept+":"+noteID).Limit(1).Scan(context.Background(), &note, &createdBy))
		var written map[string]any
		require.NoError(t, json.Unmarshal(note, &written))
		require.Equal(t, codeID, written["accessCodeId"], "the code's id was not stamped")
		require.Equal(t, w.owner, memql.BareShortId(fmt.Sprint(written["ownerUserId"])), "the row is not the owner's")
		require.Equal(t, w.owner, memql.BareShortId(createdBy), "the row was not written by the owner")
		require.Contains(t, fmt.Sprint(written["siteId"]), w.site, "the site was not stamped")

		// THE QUERY ANSWERS WITH ITS SHAPE'S ROWS, AND BARE IDS.
		rec = w.call(token, "accessBriefs", nil)
		rows := accessDataRows(t, rec)
		require.Len(t, rows, 1, rec.Body.String())
		require.Equal(t, noteID, rows[0]["id"], "the row's id is not bare")
		require.Equal(t, w.site, rows[0]["siteId"], "the relationship field is not bare")
		require.Equal(t, codeID, rows[0]["accessCodeId"])
		require.Equal(t, "visible", rows[0]["note"])
		require.NotContains(t, rows[0], "secretNote", "a field outside the shape left the handler")
		require.NotContains(t, rec.Body.String(), "v1:", "a canonical id or a concept's name left the handler")
		require.NotContains(t, rec.Body.String(), accessE2EDescription, "the concept's description left the handler")

		// AND THE QUERY WITH NO SHAPE GETS WHAT A MUTATION GETS.
		rec = w.call(token, "accessBriefsUnshaped", nil)
		requireAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"`+noteID+`"}]}`)

		// Another code on the same gate reads none of it: the filter on the stamped id is
		// how a product isolates codes, and the stamp is what makes the filter mean that.
		_, otherCode := w.mint(w.site, "dashboard", "")
		other := w.open(otherCode)
		require.Empty(t, accessDataRows(t, w.call(other, "accessBriefs", nil)), "one code read another code's rows")
	})

	t.Run("a call cannot name its own stamps or undeclared arguments", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		codeID, code := w.mint(w.site, "dashboard", "")
		token := w.open(code)
		w.call(token, "accessBriefRecord", map[string]any{"noteId": "as-note-" + w.suffix, "note": "mine"})

		for _, reserved := range memql.ReservedAccessArgs {
			rec := w.call(token, "accessBriefs", map[string]any{reserved: "forged"})
			requireAccessReply(t, rec, http.StatusBadRequest, `{"error":"invalid_args"}`)
		}
		for _, name := range []string{"bogus", `x: 1) mutation evil(`} {
			rec := w.call(token, "accessBriefs", map[string]any{name: 1})
			requireAccessReply(t, rec, http.StatusBadRequest, `{"error":"invalid_args"}`)
		}
		// THE ENGINE IS NO SECOND CHECK, which is why the handler's is the only one:
		// measured, it ignores an argument nobody declared.
		result, err := rig.eng.Execute(auth.ContextWithUserActor(context.Background(), w.owner),
			fmt.Sprintf(`query %s.accessBriefs(accessCodeId: %s, bogus: 1)`, accessE2EDomain, langparser.QuoteString(codeID)))
		require.NoError(t, err, "the premise failed: the engine refuses an undeclared argument by itself")
		require.Len(t, memql.MaterializeRows(result), 1)

		// A VALUE IS DATA, however much it looks like the language.
		hostile := `x") mutation evil(y: "`
		noteID := "as-hostile-" + w.suffix
		rec := w.call(token, "accessBriefRecord", map[string]any{"noteId": noteID, "note": hostile})
		requireAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"`+noteID+`"}]}`)
		var found bool
		for _, row := range accessDataRows(t, w.call(token, "accessBriefs", nil)) {
			found = found || row["note"] == hostile
		}
		require.True(t, found, "a value did not come back as the string it was sent as")
	})

	t.Run("regenerating ends the session at the next call", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		codeID, code := w.mint(w.site, "dashboard", "")
		// THE HANDLER SHUTS OUT A GENERATION BELOW 1, so a real one has to start there:
		// what a mint and a regenerate write, read off the row.
		require.EqualValues(t, 1, w.storedCode(codeID)["generation"], "a mint does not write generation 1")
		token := w.open(code)
		require.Equal(t, http.StatusOK, w.call(token, "accessBriefs", nil).Code)

		// The operator's own act, through the dispatch and the capability gate.
		reply := w.builtin("siteAccessCodeRegenerate", map[string]any{"codeId": codeID})
		fresh, _ := reply["code"].(string)
		require.NotEmpty(t, fresh)
		require.NotEqual(t, code, fresh)
		require.EqualValues(t, 2, w.storedCode(codeID)["generation"], "a regenerate does not move the generation on")

		rec := w.call(token, "accessBriefs", nil)
		requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessCookieCleared(t, rec)
		rec = w.session(token)
		requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessCookieCleared(t, rec)

		// THE OLD CODE IS GONE, byte for byte a code nobody ever minted, and the old row
		// version still in the table is not what answers.
		unknown := w.unlock("00000-00000-00000-00000-00000")
		requireAccessReply(t, unknown, http.StatusUnauthorized, `{"state":"invalid"}`)
		requireAccessReply(t, w.unlock(code), http.StatusUnauthorized, unknown.Body.String())

		// The new one opens, and its session is a live one of the new generation.
		token = w.open(fresh)
		require.Equal(t, http.StatusOK, w.session(token).Code)
		require.Equal(t, http.StatusOK, w.call(token, "accessBriefs", nil).Code)
	})

	t.Run("an expiry in the past", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		codeID, code := w.mint(w.site, "dashboard", "")
		token := w.open(code)
		require.Equal(t, http.StatusOK, w.session(token).Code)

		w.moveExpiry(codeID, time.Now().Add(-time.Minute))

		rec := w.session(token)
		requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessCookieCleared(t, rec)
		requireAccessReply(t, w.call(token, "accessBriefs", nil), http.StatusUnauthorized, `{"state":"ended"}`)
		rec = w.unlock(code)
		requireAccessReply(t, rec, http.StatusForbidden, `{"state":"expired"}`)
		require.Empty(t, rec.Header().Get("Set-Cookie"))

		// Extended, the same cookie is good again, and remainingMs says so at once.
		w.builtin("siteAccessCodeSetExpiry", map[string]any{
			"codeId": codeID, "expiresAt": time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339),
		})
		rec = w.session(token)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var opened struct {
			RemainingMs int64 `json:"remainingMs"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &opened))
		require.Greater(t, opened.RemainingMs, time.Hour.Milliseconds(), "the extension did not move what the browser is told")

		// Removed, it is out, and the code answers as one nobody minted.
		w.builtin("siteAccessCodeRemove", map[string]any{"codeId": codeID})
		requireAccessReply(t, w.session(token), http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessReply(t, w.unlock(code), http.StatusUnauthorized, `{"state":"invalid"}`)
	})

	t.Run("a code from another site", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		otherSite := "as-other-" + w.suffix
		w.siteRow(otherSite, w.pkg, w.account)
		_, otherCode := w.mint(otherSite, "dashboard", "")

		unknown := w.unlock("00000-00000-00000-00000-00000")
		requireAccessReply(t, w.unlock(otherCode), http.StatusUnauthorized, unknown.Body.String())

		// It opens where it was minted, and that session is not good on this site.
		rec := w.unlockOn(otherSite, otherCode)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		token := accessE2EToken(t, rec)
		rec = w.session(token)
		requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessCookieCleared(t, rec)
		requireAccessReply(t, w.call(token, "accessBriefs", nil), http.StatusUnauthorized, `{"state":"ended"}`)

		// Nor is it this site's to end: leaving here says 204, and it is still open there.
		requireAccessReply(t, w.leave(token), http.StatusNoContent, "")
		rec = w.send(http.MethodGet, "/access/session", "", token, otherSite, w.owner)
		require.Equal(t, http.StatusOK, rec.Code, "leaving on one site ended a session of another: %s", rec.Body.String())
	})

	// THE STORE READS AS THE CLUSTER, so no tier stands between one owner's site and
	// another's: this is the row's site being compared with the stamped one, and nothing
	// else, on a site whose owner has nothing to do with this one.
	t.Run("a code and a session of somebody else's site", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		theirSite := "as-theirs-" + w.suffix
		w.insert("v1:platform:site", theirSite, map[string]any{
			"hostname": theirSite + ".example.test", "kind": "spa", "status": "draft", "bundleRef": "blob://access-surface/v1",
			"ownerUserId": "v1:identity:user:" + w.member, "accountId": w.account,
			"packageId": "v1:platform:package:" + w.pkg, "packageDeployableName": accessE2EDeployable,
		})
		asThem := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), w.member))
		_, theirCode := w.forgeCodeAs(asThem, theirSite, w.account, "dashboard")
		theirOwner := "v1:identity:user:" + w.member

		rec := w.send(http.MethodPost, "/access/unlock", `{"code":"`+theirCode+`"}`, "", theirSite, theirOwner)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		theirToken := accessE2EToken(t, rec)

		unknown := w.unlock("00000-00000-00000-00000-00000")
		requireAccessReply(t, w.unlock(theirCode), http.StatusUnauthorized, unknown.Body.String())
		rec = w.session(theirToken)
		requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessCookieCleared(t, rec)
		requireAccessReply(t, w.call(theirToken, "accessBriefs", nil), http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessReply(t, w.leave(theirToken), http.StatusNoContent, "")

		rec = w.send(http.MethodGet, "/access/session", "", theirToken, theirSite, theirOwner)
		require.Equal(t, http.StatusOK, rec.Code, "somebody else's site ended a session of theirs: %s", rec.Body.String())
	})

	t.Run("a stamp that does not name the owner", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		_, code := w.mint(w.site, "dashboard", "")
		token := w.open(code)

		// THE PREMISE, on the real tier: the account arm admits the member to the row, so
		// the read returning a row proves nothing, and this is why the handler compares.
		result, err := rig.eng.Execute(auth.ContextWithUserActor(context.Background(), w.member),
			`query siteById(siteId: `+langparser.QuoteString(w.site)+`)`)
		require.NoError(t, err)
		rows := memql.MaterializeRows(result)
		require.Len(t, rows, 1, "the premise failed: the tier does not admit a member, so this proves nothing")

		for _, forged := range []string{w.member, "v1:identity:user:" + w.member, w.stranger, w.operator} {
			for name, rec := range map[string]*httptest.ResponseRecorder{
				"session": w.send(http.MethodGet, "/access/session", "", token, w.site, forged),
				"leave":   w.send(http.MethodPost, "/access/leave", "{}", token, w.site, forged),
				"call":    w.send(http.MethodPost, "/access/call", `{"call":"accessBriefs"}`, token, w.site, forged),
				"unlock":  w.send(http.MethodPost, "/access/unlock", `{"code":"`+code+`"}`, "", w.site, forged),
			} {
				requireAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)
				require.Empty(t, rec.Header().Get("Set-Cookie"), "%s as %s", name, forged)
			}
		}
		// Nothing above touched the session: a forged leave ended nothing.
		require.Equal(t, http.StatusOK, w.session(token).Code)
	})

	// THE TIER READS THE STAMP'S OWNER THROUGH THE ORGANIZATION, so an owner who is not a
	// member of the site's organization, which is what somebody who has left it looks like,
	// does not read their own site. The surface answers as it does for any stamp that does
	// not check out: nothing opens until somebody who can act has dealt with the site.
	t.Run("an owner outside the site's organization opens nothing", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		outsiders := "as-outsiders-" + w.suffix
		w.insert("v1:platform:site", outsiders, map[string]any{
			"hostname": outsiders + ".example.test", "kind": "spa", "status": "draft", "bundleRef": "blob://access-surface/v1",
			"ownerUserId": "v1:identity:user:" + w.stranger, "accountId": w.account,
			"packageId": "v1:platform:package:" + w.pkg, "packageDeployableName": accessE2EDeployable,
		})
		result, err := rig.eng.Execute(auth.ContextWithUserActor(context.Background(), w.stranger),
			`query siteById(siteId: `+langparser.QuoteString(outsiders)+`)`)
		require.NoError(t, err)
		require.Empty(t, memql.MaterializeRows(result), "the premise failed: the tier admits an owner outside the organization")

		rig.logs.Reset()
		rec := w.send(http.MethodGet, "/access/session", "", "", outsiders, "v1:identity:user:"+w.stranger)
		requireAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)

		// AND AN OPERATOR CAN SEE WHY, since nothing else anywhere says so.
		logged := rig.logs.String()
		require.Contains(t, logged, "level=WARN")
		require.Contains(t, logged, "the stamped owner cannot read this site")
		require.Contains(t, logged, "subject.id="+outsiders)
		require.Contains(t, logged, "subject.concept=v1:platform:site")
		require.NotContains(t, logged, w.stranger, "the line names the owner the stamp named")
	})

	t.Run("a code whose copy of the account is out of date", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		codeID, code := w.forgeCode(w.site, "as-acct-the-site-left-"+w.suffix, "dashboard")

		unknown := w.unlock("00000-00000-00000-00000-00000")
		requireAccessReply(t, w.unlock(code), http.StatusUnauthorized, unknown.Body.String())

		// And a session already open on it, as one would be had the move raced the mint.
		const token = "mql_acs_a-session-opened-before-the-move-landed"
		store := siteaccess.NewGraphStore(&AccessEngineAdapter{Engine: rig.eng})
		require.NoError(t, store.CreateSession(context.Background(), w.owner, siteaccess.Session{
			SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now(),
		}, memql.AccessSessionTokenDigest(token)))
		rec := w.session(token)
		requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		requireAccessCookieCleared(t, rec)
	})

	// THE REVIEW THAT MOVED THE STORE TO THE CLUSTER: a whole-user ownership transfer could
	// leave a code with an heir while the site kept its owner, and a read under the site's
	// owner then did not find it. So a code answers whoever's row it is, and what scopes it
	// is the site: the other half of this case is that nothing about the row's owner lets
	// it open a site it does not name.
	t.Run("a row's current owner does not decide whether a code opens", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		heir := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), w.member))
		codeID, code := w.forgeCodeAs(heir, w.site, w.account, "dashboard")

		var stored []byte
		require.NoError(t, rig.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("payload").
			Where("id = ?", "v1:platform:siteAccessCode:"+codeID).Limit(1).Scan(context.Background(), &stored))
		var row map[string]any
		require.NoError(t, json.Unmarshal(stored, &row))
		require.Equal(t, w.member, memql.BareShortId(fmt.Sprint(row["ownerUserId"])), "the premise failed: the code is the site owner's after all")

		token := w.open(code)
		require.Equal(t, http.StatusOK, w.session(token).Code)
		require.Equal(t, http.StatusOK, w.call(token, "accessBriefs", nil).Code)
		requireAccessReply(t, w.leave(token), http.StatusNoContent, "")
		requireAccessReply(t, w.session(token), http.StatusUnauthorized, `{"state":"ended"}`)

		// A session under somebody else is honored the same way, and ended the same way.
		const moved = "mql_acs_a-session-whose-row-belongs-to-an-heir"
		store := siteaccess.NewGraphStore(&AccessEngineAdapter{Engine: rig.eng})
		require.NoError(t, store.CreateSession(context.Background(), w.member, siteaccess.Session{
			SiteID: w.site, CodeID: codeID, Generation: 1, IssuedAt: time.Now(),
		}, memql.AccessSessionTokenDigest(moved)))
		require.Equal(t, http.StatusOK, w.session(moved).Code)
		requireAccessReply(t, w.leave(moved), http.StatusNoContent, "")
		requireAccessReply(t, w.session(moved), http.StatusUnauthorized, `{"state":"ended"}`)

		// And the site still scopes it: the same code is nothing on another of his sites.
		otherSite := "as-heirs-other-" + w.suffix
		w.siteRow(otherSite, w.pkg, w.account)
		unknown := w.unlockOn(otherSite, "00000-00000-00000-00000-00000")
		requireAccessReply(t, unknown, http.StatusUnauthorized, `{"state":"invalid"}`)
		requireAccessReply(t, w.unlockOn(otherSite, code), http.StatusUnauthorized, unknown.Body.String())
	})

	// THE ACTS THE REVIEW MEASURED, on the real store: each of them used to answer 200 with
	// rows, because nothing re-read the session once the construct had run. The engine here
	// is the real one, doing what it was asked and only then letting the act land, so the
	// rows are really in hand when the handler comes to deliver them.
	t.Run("an operator's act during a call delivers nothing", func(t *testing.T) {
		for name, act := range map[string]func(w *accessWorld, codeID string){
			"a regenerate": func(w *accessWorld, codeID string) {
				w.builtin("siteAccessCodeRegenerate", map[string]any{"codeId": codeID})
			},
			"a removal": func(w *accessWorld, codeID string) {
				w.builtin("siteAccessCodeRemove", map[string]any{"codeId": codeID})
			},
			"an expiry moved into the past": func(w *accessWorld, codeID string) {
				w.moveExpiry(codeID, time.Now().Add(-time.Minute))
			},
		} {
			t.Run(name, func(t *testing.T) {
				w := newAccessWorld(t, rig)
				codeID, code := w.mint(w.site, "dashboard", "")
				token := w.open(code)
				const note = "a-note-this-visitor-may-no-longer-read"
				noteID := "as-acting-" + w.suffix
				requireAccessReply(t, w.call(token, "accessBriefRecord", map[string]any{"noteId": noteID, "note": note}),
					http.StatusOK, `{"data":[{"id":"`+noteID+`"}]}`)

				acting := &accessActingEngine{inner: &AccessEngineAdapter{Engine: rig.eng}}
				logger := slog.New(slog.DiscardHandler)
				handler := server.NewAccessHandler(acting, newAccessStore(&AccessEngineAdapter{Engine: rig.eng}, logger), logger, time.Now)
				serve := func() *httptest.ResponseRecorder {
					req := httptest.NewRequest(http.MethodPost, "/access/call", strings.NewReader(`{"call":"accessBriefs"}`))
					req.Header.Set(server.AccessStampSiteHeader, w.site)
					req.Header.Set(server.AccessStampOwnerHeader, "v1:identity:user:"+w.owner)
					req.AddCookie(&http.Cookie{Name: server.AccessCookieName, Value: token})
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, req)
					return rec
				}

				// The control: with nothing landing, this handler delivers the row.
				rec := serve()
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), note, "the row is not there to be withheld, so this proves nothing")

				acting.after = func() { act(w, codeID) }
				rec = serve()
				require.Equal(t, 1, acting.landed, "the act did not land during the call")
				requireAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
				requireAccessCookieCleared(t, rec)
			})
		}
	})

	t.Run("what a session cannot reach is one 404", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		_, code := w.mint(w.site, "dashboard", "")
		token := w.open(code)
		const notFound = `{"error":"not_found"}`

		requireAccessReply(t, w.call(token, "nobodyDeclaredThis", nil), http.StatusNotFound, notFound)
		requireAccessReply(t, w.call(token, "accessClientBriefs", nil), http.StatusNotFound, notFound)
		requireAccessReply(t, w.call(token, "accessBriefsOfAnotherDeployable", nil), http.StatusNotFound, notFound)
		// A real construct that is not gated at all, by its short and its qualified name.
		requireAccessReply(t, w.call(token, "siteById", map[string]any{"siteId": w.site}), http.StatusNotFound, notFound)
		requireAccessReply(t, w.call(token, "platform.siteById", map[string]any{"siteId": w.site}), http.StatusNotFound, notFound)

		// A site whose package has no recorded domains reaches nothing, and a code on a
		// gate nobody declares reaches nothing: the code is good, and the call is not.
		bareSite, barePkg := "as-bare-"+w.suffix, "as-bare-pkg-"+w.suffix
		w.packageRow(barePkg, nil)
		w.siteRow(bareSite, barePkg, w.account)
		_, bareCode := w.forgeCode(bareSite, w.account, "dashboard")
		rec := w.unlockOn(bareSite, bareCode)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		bareToken := accessE2EToken(t, rec)
		rec = w.send(http.MethodPost, "/access/call", `{"call":"accessBriefs"}`, bareToken, bareSite, w.owner)
		requireAccessReply(t, rec, http.StatusNotFound, notFound)

		_, droppedCode := w.forgeCode(w.site, w.account, "gate-dropped-from-the-bundle")
		dropped := w.open(droppedCode)
		requireAccessReply(t, w.call(dropped, "accessBriefs", nil), http.StatusNotFound, notFound)
		require.Equal(t, http.StatusOK, w.session(dropped).Code, "a code whose gate is gone is still a live session; it just opens nothing")
	})

	t.Run("all four stamps land on the row", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		codeID, code := w.mint(w.site, "clients", "acme")
		token := w.open(code)
		noteID := "as-stamped-" + w.suffix

		rec := w.call(token, "accessClientRecord", map[string]any{"noteId": noteID})
		requireAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"`+noteID+`"}]}`)

		var stored []byte
		require.NoError(t, rig.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("payload").
			Where("id = ?", accessE2EConcept+":"+noteID).Limit(1).Scan(context.Background(), &stored))
		var row map[string]any
		require.NoError(t, json.Unmarshal(stored, &row))
		require.Equal(t, codeID, row["accessCodeId"], "accessCodeId")
		require.Equal(t, "acme", row["clientName"], "accessSubject")
		require.Equal(t, "clients", row["note"], "accessGate")
		require.Equal(t, w.site, memql.BareShortId(fmt.Sprint(row["siteId"])), "siteId")
	})

	t.Run("a subject scopes a call, and no subject opens no subject's rows", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		seed := func(noteID, client string) {
			payload := map[string]any{"ownerUserId": w.owner, "note": noteID}
			if client != "" {
				payload["clientName"] = client
			}
			w.insert(accessE2EConcept, noteID, payload)
		}
		seed("as-acme-"+w.suffix, "acme")
		seed("as-other-client-"+w.suffix, "other-client")
		seed("as-nobodys-"+w.suffix, "")

		_, code := w.mint(w.site, "clients", "acme")
		token := w.open(code)
		rows := accessDataRows(t, w.call(token, "accessClientBriefs", nil))
		require.Len(t, rows, 1)
		require.Equal(t, "as-acme-"+w.suffix, rows[0]["id"])

		// THE PREMISE: stamping an empty string would match the row with no subject at
		// all, which is what stamping nothing holds off.
		result, err := rig.eng.Execute(auth.ContextWithUserActor(context.Background(), w.owner),
			fmt.Sprintf(`query %s.accessClientBriefs(accessSubject: "")`, accessE2EDomain))
		require.NoError(t, err)
		require.NotEmpty(t, memql.MaterializeRows(result), "the premise failed: an empty subject matches nothing")

		_, bareCode := w.mint(w.site, "clients", "")
		bare := w.open(bareCode)
		requireAccessReply(t, w.call(bare, "accessClientBriefs", nil), http.StatusBadGateway, `{"error":"refused"}`)
	})

	t.Run("leaving ends this session only, once", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		_, code := w.mint(w.site, "dashboard", "")
		first, second := w.open(code), w.open(code)

		rec := w.leave(first)
		requireAccessReply(t, rec, http.StatusNoContent, "")
		requireAccessCookieCleared(t, rec)
		requireAccessReply(t, w.session(first), http.StatusUnauthorized, `{"state":"ended"}`)
		require.Equal(t, http.StatusOK, w.session(second).Code, "leaving in one browser took the other out")

		versions := func() int64 {
			n, err := rig.db.NewSelect().TableExpr(`"MemoryNodes"`).
				Where("concept = ?", "v1:platform:siteAccessSession").
				Where("payload->>'tokenHash' = ?", memql.AccessSessionTokenDigest(first)).
				Count(context.Background())
			require.NoError(t, err)
			return n
		}
		before := versions()
		requireAccessReply(t, w.leave(first), http.StatusNoContent, "")
		require.Equal(t, before, versions(), "a second leave wrote to the session")
	})

	t.Run("a @serverOnly construct is out of reach, with a gate or without", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		_, code := w.mint(w.site, "dashboard", "")
		token := w.open(code)
		digest := memql.AccessCodeDigest(code)

		// LOAD REFUSES THIS PAIR, so the entry is written by hand: it is what the handler
		// would do with one if a load ever let it through. Under borrowed authority the
		// engine refuses it, which is the whole of why it must not be internal origin.
		restore := memql.AccessGateEntries()
		t.Cleanup(func() { memql.SetAccessGatesForTest(restore...) })
		memql.SetAccessGatesForTest(append(append([]memql.AccessGateEntry(nil), restore...), memql.AccessGateEntry{
			Deployable: accessE2EDeployable, Gate: "dashboard", Construct: "siteAccessCodeByHash",
			Kind: memql.AccessKindQuery, Domain: accessE2EDomain, Namespace: "platform",
			Args: []string{"codeHash"}, Stamped: []string{memql.AccessArgSiteID},
		})...)

		// THE PREMISE: with internal origin the same statement finds the row.
		result, err := rig.eng.Execute(w.asOwner(), fmt.Sprintf(`query platform.siteAccessCodeByHash(codeHash: %s, siteId: %s)`,
			langparser.QuoteString(digest), langparser.QuoteString(w.site)))
		require.NoError(t, err)
		require.Len(t, memql.MaterializeRows(result), 1, "the premise failed: the statement finds nothing, so a refusal proves nothing")

		rec := w.call(token, "siteAccessCodeByHash", map[string]any{"codeHash": digest})
		requireAccessReply(t, rec, http.StatusBadGateway, `{"error":"refused"}`)
	})

	t.Run("neither log holds a secret", func(t *testing.T) {
		w := newAccessWorld(t, rig)
		rig.logs.Reset()
		rig.engineLogs.Reset()
		codeID, code := w.mint(w.site, "dashboard", "")
		token := w.open(code)
		const secretValue = "a-value-only-the-caller-knows"

		// Every path a refusal can take, the engine's own included: `note` wants a string,
		// so this one is refused by the engine and with the statement in its hands.
		w.call(token, "accessBriefRecord", map[string]any{"noteId": "as-refused-" + w.suffix, "note": map[string]any{"secret": secretValue}})
		// A READ THE RESULT CACHE KEEPS, AND THEN A WRITE THAT EVICTS IT. The engine is nearly
		// silent on this path, and this is the line it does write on it, at Debug: without
		// this pair the capture below was empty, and its half of this test was true of an
		// empty buffer whatever the engine did.
		w.call(token, "accessBriefsCached", nil)
		w.call(token, "accessBriefRecord", map[string]any{"noteId": "as-logged-" + w.suffix, "note": secretValue})
		w.call(token, "accessBriefs", nil)
		w.call(token, "accessBriefs", map[string]any{"bogus": secretValue})
		w.call(token, "accessBriefs", map[string]any{memql.AccessArgCodeID: secretValue})
		w.call(token, "nobodyDeclaredThis", map[string]any{"window": secretValue})
		w.unlock("00000-00000-00000-00000-00000")
		w.session(token)
		w.leave(token)
		w.session(token)

		handler, engine := rig.logs.String(), rig.engineLogs.String()
		// THE ABSENCES BELOW MEAN NOTHING OF A LOG NOTHING WROTE TO. The engine's half has to
		// have captured something from this flow, or "the engine's log holds no secret" is
		// true of an empty buffer whatever the engine does.
		require.NotEmpty(t, engine, "the engine logged nothing in this flow, so its half of this test proves nothing")
		for name, secret := range map[string]string{
			"the code":            code,
			"the normalized code": memql.NormalizeAccessCode(code),
			"the code's digest":   memql.AccessCodeDigest(code),
			"the token":           token,
			"the token's digest":  memql.AccessSessionTokenDigest(token),
			"a call argument":     secretValue,
		} {
			require.NotContains(t, handler, secret, "the handler's log holds %s", name)
			require.NotContains(t, engine, secret, "the engine's log holds %s", name)
		}
		require.NotContains(t, handler, codeID, "the handler's log holds the code's id, which nothing it logs is about")
		// IT STILL SAYS WHAT HAPPENED: the site and the construct.
		require.Contains(t, handler, w.site)
		require.Contains(t, handler, "accessBriefRecord")
	})
}

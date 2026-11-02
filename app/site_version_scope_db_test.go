package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// site_version_scope_db_test.go -- a site names and serves its own versions, end
// to end: a person's client call through the real engine, onto a real Postgres,
// read back by the real edge resolver and handler over an in-memory object store.
//
// WHY HERE. component/edge imports component/memql, so neither package's tests can
// hold both halves, and the claim is about the two of them together.
//
// Postgres-gated: a skip is what an unreachable database produces, and
// MEMQL_REQUIRE_DB=1 turns it into a failure.

type versionScopeBlobs map[string][]byte

func (b versionScopeBlobs) Get(_ context.Context, key string) ([]byte, error) {
	if data, ok := b[key]; ok {
		return data, nil
	}
	return nil, edge.ErrNotFound
}

func (b versionScopeBlobs) Put(_ context.Context, key string, data []byte) error {
	b[key] = data
	return nil
}

type versionScopeRig struct {
	t       *testing.T
	eng     *memql.MemQLEngine
	db      *bun.DB
	blobs   versionScopeBlobs
	handler http.Handler
	domain  string
	account string
	rows    []string
}

func bootVersionScopeRig(t *testing.T) *versionScopeRig {
	t.Helper()
	eng, db := workTemplateDBEngineAndDB(t)
	domain := "scope-" + strings.ToLower(id.NewShortId()) + ".example.test"
	t.Setenv("MEMQL_DOMAIN", domain)

	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	rig := &versionScopeRig{t: t, eng: eng, db: db, blobs: versionScopeBlobs{}, domain: domain, account: "scope-acct-" + id.NewShortId()}
	t.Cleanup(func() {
		for _, row := range rig.rows {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", row).Exec(context.Background())
		}
	})
	rig.seed("v1:accounts:account", rig.account, map[string]any{"name": rig.account, "status": "active", "domainStatus": "unverified"})
	// A TTL too short to hide a row flip: every claim here is about the row's current value.
	rig.handler = edge.NewHandler(edge.Options{
		Resolver: edge.NewResolver(edge.NewEngineExecutor(&EdgeEngineAdapter{Engine: eng}), time.Nanosecond),
		Opener: edge.NewMuxOpener(map[string]edge.BundleOpener{
			"file": edge.NewFileOpener(),
			"blob": edge.NewBlobOpener(rig.blobs),
		}),
	})
	return rig
}

func (r *versionScopeRig) seed(concept, rowID string, payload map[string]any) {
	r.t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(r.t, err)
	_, err = r.eng.Execute(accessSeedContext(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(concept), langparser.QuoteString(rowID), encoded))
	require.NoError(r.t, err, "seeding %s %s", concept, rowID)
	r.rows = append(r.rows, concept+":"+memql.BareShortId(rowID))
}

func (r *versionScopeRig) mutation(name string, args map[string]any) string {
	r.t.Helper()
	call, err := langparser.RenderCall(name, args)
	require.NoError(r.t, err)
	return "mutation " + call
}

// person seeds one signed-in member of the rig's organization, and answers the
// context their client calls run under: no internal origin, no synthetic actor.
func (r *versionScopeRig) person(name string, role auth.Role) (string, context.Context) {
	r.t.Helper()
	user := name + "-" + id.NewShortId()
	r.seed("v1:identity:user", user, map[string]any{
		"displayName": user, "primaryEmail": user + "@example.test", "role": string(role), "active": true,
	})
	group := "scope-group-" + user
	_, err := r.eng.Execute(accessSeedContext(), r.mutation("writeGroup", map[string]any{
		"groupId": group, "name": group, "kind": "account", "accountId": r.account, "status": "active",
	}))
	require.NoError(r.t, err)
	r.rows = append(r.rows, "v1:identity:group:"+group)
	membership := group + "-m"
	_, err = r.eng.Execute(accessSeedContext(), r.mutation("writeGroupMembership", map[string]any{
		"membershipId": membership, "groupId": group, "userId": user, "origin": "added", "status": "active", "accountId": r.account,
	}))
	require.NoError(r.t, err)
	r.rows = append(r.rows, "v1:identity:groupMembership:"+membership)
	return user, accessPersonContext(user, role)
}

// grant gives a person one part of the Deployables app.
func (r *versionScopeRig) grant(user, part string) {
	r.t.Helper()
	resource := "app:deployables/" + part
	grantID := auth.GrantRowID(auth.SubjectKindUser, user, auth.VerbExecute, resource)
	_, err := r.eng.Execute(accessSeedContext(), r.mutation("writeGrant", map[string]any{
		"grantId": grantID, "subjectKind": auth.SubjectKindUser, "subjectId": user, "verb": auth.VerbExecute,
		"resourceType": resource, "effect": auth.GrantAllow, "grantedBy": "scope-seeder",
	}))
	require.NoError(r.t, err)
	r.rows = append(r.rows, "v1:rbac:grant:"+memql.BareShortId(grantID))
}

func (r *versionScopeRig) host() string {
	slug := "s" + strings.ToLower(strings.ReplaceAll(id.NewShortId(), "_", ""))
	if len(slug) > 30 {
		slug = slug[:30]
	}
	return strings.TrimRight(slug, "-") + "." + r.domain
}

// site seeds a row owned by user, holding the placeholder a new deployable holds.
func (r *versionScopeRig) site(user, status string) (siteID, host string) {
	r.t.Helper()
	siteID, host = "scope-site-"+id.NewShortId(), r.host()
	r.seed("v1:platform:site", siteID, map[string]any{
		"hostname": host, "kind": "static", "status": status, "bundleRef": "blob://sites/" + siteID + "/pending/",
		"ownerUserId": "v1:identity:user:" + user, "accountId": r.account,
	})
	return siteID, host
}

func (r *versionScopeRig) get(host string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// foreignVersions are the references no site in this test may name, with the
// page each would serve. A neighbouring site's version is the first of them.
func (r *versionScopeRig) foreignVersions() map[string]string {
	r.t.Helper()
	directory := r.t.TempDir()
	require.NoError(r.t, os.WriteFile(filepath.Join(directory, "index.html"), []byte("PAGE-FROM-A-DIRECTORY"), 0o644))

	operator, _ := r.person("scope-neighbour", auth.RoleDeveloper)
	neighbour, _ := r.site(operator, "live")
	neighbourKey := "sites/" + neighbour + "/v1/"
	r.blobs[neighbourKey+"index.html"] = []byte("PAGE-FROM-A-NEIGHBOUR")
	r.blobs["elsewhere/v1/index.html"] = []byte("PAGE-FROM-ELSEWHERE")
	return map[string]string{
		"blob://" + neighbourKey: "PAGE-FROM-A-NEIGHBOUR",
		"blob://elsewhere/v1/":   "PAGE-FROM-ELSEWHERE",
		"file://" + directory:    "PAGE-FROM-A-DIRECTORY",
	}
}

// Every role, on every route a reference reaches a row by. The refusal differs by
// role, because a role with no deployable parts never reaches the reference at
// all, and that difference is the part of the table worth keeping.
func TestAPersonPointsASiteOnlyAtItsOwnVersions(t *testing.T) {
	rig := bootVersionScopeRig(t)
	foreign := rig.foreignVersions()

	for _, tc := range []struct {
		role auth.Role
		// refused is what the person is told, which says which rule stopped them.
		refused string
	}{
		{auth.RoleReader, "organization_forbidden"},
		{auth.RoleWriter, memql.CodeCapabilityNotHeld},
		{auth.RoleAdmin, memql.CodeCapabilityNotHeld},
		{auth.RoleDeveloper, memql.CodeSiteVersionNotItsOwn},
		{auth.RoleOwner, memql.CodeSiteVersionNotItsOwn},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			user, ctx := rig.person("scope-"+string(tc.role), tc.role)
			for ref, page := range foreign {
				siteID, host := rig.site(user, "live")
				rig.blobs["sites/"+siteID+"/pending/index.html"] = []byte("PAGE-OF-ITS-OWN")

				_, err := rig.eng.Execute(ctx, rig.mutation("updateSiteBundle", map[string]any{"siteId": siteID, "bundleRef": ref}))
				require.ErrorContains(t, err, tc.refused, "updateSiteBundle %s", ref)

				newSite, newHost := "scope-site-"+id.NewShortId(), rig.host()
				rig.rows = append(rig.rows, "v1:platform:site:"+newSite)
				_, err = rig.eng.Execute(ctx, rig.mutation("createSite", map[string]any{
					"siteId": newSite, "accountId": rig.account, "hostname": newHost, "kind": "static", "bundleRef": ref, "status": "draft",
				}))
				require.Error(t, err, "createSite %s", ref)

				code, body := rig.get(host)
				require.NotContains(t, body, page, "the edge served it with status %d", code)
				require.Contains(t, body, "PAGE-OF-ITS-OWN", "the site stopped serving its own version: status %d", code)
			}
		})
	}
}

// A part granted to a lower role reaches the same rule, not around it.
func TestAGrantedPartIsHeldToTheSameRule(t *testing.T) {
	rig := bootVersionScopeRig(t)
	foreign := rig.foreignVersions()
	for _, part := range []string{"deploy", "publish"} {
		t.Run(part, func(t *testing.T) {
			user, ctx := rig.person("scope-granted-"+part, auth.RoleWriter)
			rig.grant(user, part)
			for ref := range foreign {
				siteID, _ := rig.site(user, "live")
				_, err := rig.eng.Execute(ctx, rig.mutation("updateSiteBundle", map[string]any{"siteId": siteID, "bundleRef": ref}))
				require.ErrorContains(t, err, memql.CodeSiteVersionNotItsOwn, "updateSiteBundle %s", ref)
			}
		})
	}
	t.Run("preview", func(t *testing.T) {
		user, ctx := rig.person("scope-granted-preview", auth.RoleWriter)
		rig.grant(user, "preview")
		for ref := range foreign {
			siteID, _ := rig.site(user, "live")
			_, err := rig.eng.Execute(ctx, rig.mutation("setSiteCandidate", map[string]any{"siteId": siteID, "candidateRef": ref}))
			require.ErrorContains(t, err, memql.CodeSiteVersionNotItsOwn, "setSiteCandidate %s", ref)
		}
	})
}

// No writer is exempt: the rule is about what the row says, not who said it.
func TestTheEngineItselfIsHeldToTheSameRule(t *testing.T) {
	rig := bootVersionScopeRig(t)
	foreign := rig.foreignVersions()
	operator, _ := rig.person("scope-operator", auth.RoleDeveloper)
	siteID, _ := rig.site(operator, "live")
	for ref := range foreign {
		_, err := rig.eng.Execute(accessSeedContext(), rig.mutation("updateSiteBundle", map[string]any{"siteId": siteID, "bundleRef": ref}))
		require.ErrorContains(t, err, memql.CodeSiteVersionNotItsOwn, "updateSiteBundle %s", ref)
		encoded, err := json.Marshal(map[string]any{"title": "raw", "bundleRef": ref})
		require.NoError(t, err)
		_, err = rig.eng.Execute(accessSeedContext(), fmt.Sprintf(`insert("v1:platform:site", id=%s, payload=%s)`, langparser.QuoteString(siteID), encoded))
		require.ErrorContains(t, err, memql.CodeSiteVersionNotItsOwn, "raw insert %s", ref)
	}
}

// Every way a version legitimately reaches a row, spelled the way each route
// spells the site's id, and each one read back through the edge: a publish under
// the CI route's actor, a publish under a person, a candidate and its promotion,
// a rollback, and the placeholders a new deployable starts with.
func TestThePublishPathsStillPointASiteAtItsVersions(t *testing.T) {
	rig := bootVersionScopeRig(t)
	_, ctx := rig.person("scope-publisher", auth.RoleDeveloper)
	bundle := func(page string) edge.Bundle { return edge.Bundle{"index.html": []byte(page)} }
	serves := func(t *testing.T, host, page string) {
		t.Helper()
		code, body := rig.get(host)
		require.Equal(t, http.StatusOK, code, "%q", body)
		require.Contains(t, body, page)
	}

	for name, spell := range map[string]func(string) string{
		"bare id":      func(siteID string) string { return siteID },
		"canonical id": func(siteID string) string { return "v1:platform:site:" + siteID },
	} {
		t.Run(name, func(t *testing.T) {
			// The placeholder the OS writes, then the empty one a package run writes.
			siteID, host := "scope-site-"+id.NewShortId(), rig.host()
			rig.rows = append(rig.rows, "v1:platform:site:"+siteID)
			_, err := rig.eng.Execute(ctx, rig.mutation("createSite", map[string]any{
				"siteId": spell(siteID), "accountId": rig.account, "hostname": host, "kind": "static",
				"bundleRef": "blob://sites/" + spell(siteID) + "/pending/", "status": "draft",
			}))
			require.NoError(t, err)
			emptySite := "scope-site-" + id.NewShortId()
			rig.rows = append(rig.rows, "v1:platform:site:"+emptySite)
			_, err = rig.eng.Execute(ctx, rig.mutation("createSite", map[string]any{
				"siteId": spell(emptySite), "accountId": rig.account, "hostname": rig.host(), "kind": "static", "bundleRef": "", "status": "draft",
			}))
			require.NoError(t, err)

			// The CI route: a server actor, through the engine's site store.
			ci := edge.NewPublisher(rig.blobs, edge.NewEngineSiteStore(&EdgeEngineAdapter{Engine: rig.eng}))
			first, err := ci.Publish(context.Background(), spell(siteID), bundle("PAGE-ONE"), edge.TargetServing)
			require.NoError(t, err)
			_, err = rig.eng.Execute(ctx, rig.mutation("updateSiteStatus", map[string]any{"siteId": siteID, "status": "live"}))
			require.NoError(t, err)
			serves(t, host, "PAGE-ONE")

			// The Library and the package pipeline: the same statement, under a person.
			person := edge.NewPublisher(rig.blobs, versionScopePersonStore{rig: rig, ctx: ctx})
			second, err := person.Publish(context.Background(), spell(siteID), bundle("PAGE-TWO"), edge.TargetServing)
			require.NoError(t, err)
			require.NotEqual(t, first.BundleRef, second.BundleRef)
			serves(t, host, "PAGE-TWO")

			// A rollback is the same write pointed at an earlier version.
			_, err = rig.eng.Execute(ctx, rig.mutation("updateSiteBundle", map[string]any{"siteId": siteID, "bundleRef": first.BundleRef}))
			require.NoError(t, err)
			serves(t, host, "PAGE-ONE")

			// A candidate, and its promotion.
			next, err := person.Publish(context.Background(), spell(siteID), bundle("PAGE-THREE"), edge.TargetCandidate)
			require.NoError(t, err)
			serves(t, host, "PAGE-ONE")
			_, err = rig.eng.Execute(ctx, rig.mutation("promoteSiteCandidate", map[string]any{"siteId": siteID, "candidateRef": next.BundleRef}))
			require.NoError(t, err)
			serves(t, host, "PAGE-THREE")

			// The promotion leaves an empty candidate on the row, and the publish
			// and the rollback after it are still only those.
			_, err = person.Publish(context.Background(), spell(siteID), bundle("PAGE-FOUR"), edge.TargetServing)
			require.NoError(t, err)
			serves(t, host, "PAGE-FOUR")
			_, err = rig.eng.Execute(ctx, rig.mutation("updateSiteBundle", map[string]any{"siteId": siteID, "bundleRef": next.BundleRef}))
			require.NoError(t, err)
			serves(t, host, "PAGE-THREE")

			// Withdrawing a candidate leaves the same empty value, and the same publish.
			_, err = person.Publish(context.Background(), spell(siteID), bundle("PAGE-FIVE"), edge.TargetCandidate)
			require.NoError(t, err)
			_, err = rig.eng.Execute(ctx, rig.mutation("clearSiteCandidate", map[string]any{"siteId": siteID}))
			require.NoError(t, err)
			serves(t, host, "PAGE-THREE")
			_, err = person.Publish(context.Background(), spell(siteID), bundle("PAGE-SIX"), edge.TargetServing)
			require.NoError(t, err)
			serves(t, host, "PAGE-SIX")
		})
	}
}

// versionScopePersonStore points a version the way the Library deploy and the
// package pipeline do: edge.PointVersionStatement under the caller's own actor.
type versionScopePersonStore struct {
	rig *versionScopeRig
	ctx context.Context
}

func (s versionScopePersonStore) PointVersion(_ context.Context, siteID string, target edge.Target, ref string) error {
	statement, err := edge.PointVersionStatement(siteID, target, ref, "")
	if err != nil {
		return err
	}
	_, err = s.rig.eng.Execute(s.ctx, statement)
	return err
}

// A row written before the rule keeps working as a row: its reference is what is
// refused, by the edge, and everything else about it stays editable, so a row in
// that state can be repaired by the ordinary write.
func TestARowWrittenBeforeTheRuleStaysWritable(t *testing.T) {
	rig := bootVersionScopeRig(t)
	foreign := rig.foreignVersions()
	user, ctx := rig.person("scope-older", auth.RoleDeveloper)

	for ref, page := range foreign {
		siteID, host := rig.site(user, "live")
		rig.blobs["sites/"+siteID+"/v1/index.html"] = []byte("PAGE-OF-ITS-OWN")
		// Past the engine, as a row older than the rule is.
		_, err := rig.db.ExecContext(context.Background(),
			`UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{bundleRef}', to_jsonb(?::text)) WHERE id = ?`,
			ref, "v1:platform:site:"+siteID)
		require.NoError(t, err)

		code, body := rig.get(host)
		require.NotContains(t, body, page)
		require.Equal(t, http.StatusServiceUnavailable, code, "%q", body)

		_, err = rig.eng.Execute(ctx, rig.mutation("updateSiteStatus", map[string]any{"siteId": siteID, "status": "disabled"}))
		require.NoError(t, err, "a write that does not move the reference")

		_, err = rig.eng.Execute(ctx, rig.mutation("updateSiteBundle", map[string]any{"siteId": siteID, "bundleRef": "blob://sites/" + siteID + "/v1/"}))
		require.NoError(t, err)
		_, err = rig.eng.Execute(ctx, rig.mutation("updateSiteStatus", map[string]any{"siteId": siteID, "status": "live"}))
		require.NoError(t, err)
		code, body = rig.get(host)
		require.Equal(t, http.StatusOK, code, "%q", body)
		require.Contains(t, body, "PAGE-OF-ITS-OWN")
	}
}

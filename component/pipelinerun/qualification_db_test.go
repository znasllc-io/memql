package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

// Real DSL rows, create-only mutation behavior, owner predicates and advisory
// gates carry authority from an opener to a separate driver. Only the provider
// and execution substrate are fake. MEMQL_REQUIRE_DB=1 makes absence a failure.
func TestQualificationAuthoritySurvivesRealRowsAndReplacementDriver(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()
	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner, pkg, account := "qualification-owner-"+suffix, "qualification-source-"+suffix, "qualification-account-"+suffix
	repo := "qualification-" + suffix + "/shop"
	credential := "qualification-credential-" + suffix
	// Borrowed owner reads resolve the person's live role, not the original
	// request's transient access context. Persist the actual fixture principal.
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createUser(userId: %s, displayName: "Qualification owner", primaryEmail: %s, role: "developer")`, langparser.QuoteString(owner), langparser.QuoteString(owner+"@example.test"))); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Qualification test")`, langparser.QuoteString(account))); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Execute(signedIn(owner), fmt.Sprintf(`mutation createPackage(accountId: %s, packageId: %s, name: "shop", sourceKind: "repo", repoUrl: %s, credentialId: %s)`, langparser.QuoteString(account), langparser.QuoteString(pkg), langparser.QuoteString("https://github.com/"+repo), langparser.QuoteString(credential))); err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t)
	gh.repos[repo] = githubapp.RepositoryInfo{ID: 100, FullName: repo, DefaultBranch: "main"}
	pr := qualificationPR()
	pr.HeadRepository, pr.BaseRepository = repo, repo
	gh.pulls[repo] = []githubapp.PullRequestHead{pr}
	gh.trees[repo+"@"+shaA] = fstest.MapFS{pipelines.ManifestPath: &fstest.MapFile{Data: []byte(strings.ReplaceAll(driveManifest, "          secrets: [SHOP_TOKEN]\n", ""))}, "go.mod": &fstest.MapFile{Data: []byte("module example.test/shop\n")}}
	newIntegration := func(node string, drive bool) *Integration {
		pool := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
		t.Cleanup(func() { _ = pool.Close() })
		i := New(Deps{Store: NewDSLStore(eng), GitHub: gh, NodeID: node, OSOrigin: func() string { return testOSOrigin }, EngineRevision: func() string { return "qualification-db-test" }, HeartbeatEvery: time.Hour,
			Gate: watchedGate(t, func(c context.Context, key string, fn func(context.Context) error) error {
				return githubconnect.WithGate(c, pool, key, fn)
			}),
			Journal: workjournal.New(workjournal.ExecutorFunc(func(c context.Context, q string) (any, error) { return eng.Execute(c, q) }), nil, node),
			Secrets: func(context.Context, string) (string, error) { return "fixture-only-secret", nil },
		})
		if drive {
			i.EnableDriver()
		}
		return i
	}
	opener := newIntegration("qualification-opener-"+suffix, false)
	preview, err := opener.PreviewPullRequest(signedIn(owner), pkg, 42)
	if err != nil || preview.Refusal != nil {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	req := QualifyPullRequestRequest{PackageID: pkg, RequestID: "first", PullRequest: 42, ExpectedHeadSHA: preview.SHA, ExpectedBaseSHA: preview.BaseSHA, ExpectedManifestDigest: preview.ManifestDigest, SecretNames: []string{}, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	r, err := opener.QualifyPullRequest(signedIn(owner), req)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.PipelineByID(memql.ContextWithFreshRead(ctx), r.PipelineID)
	if err != nil || p == nil || p.Status != PipelineQualification {
		t.Fatalf("inactive creation: %+v %v", p, err)
	}
	rows, err := store.PipelinesForRepository(ctx, repo)
	if err != nil || !hasPipeline(rows, p.ID) {
		t.Fatal("qualification reservation invisible")
	}
	automatic, err := store.PipelinesPolled(ctx)
	if err != nil || hasPipeline(automatic, p.ID) {
		t.Fatal("qualification visible to automatic poll")
	}
	before := mustRun(t, store, r.ID)
	if err := ValidateQualification(*p, before, time.Now()); err != nil {
		t.Fatalf("%v; qualification=%#v", err, before.Qualification)
	}
	changed := before
	changed.Qualification = map[string]any{"version": 999, "requestId": "forged"}
	if err := store.CreateRun(ctx, changed); err != nil {
		t.Fatal(err)
	}
	after := mustRun(t, store, r.ID)
	if !reflect.DeepEqual(before.Qualification, after.Qualification) {
		t.Fatal("create-only qualification authority was replaced")
	}
	stranger, err := store.RunForOwner(signedInAs("qualification-stranger-"+suffix, auth.RoleWriter), r.ID)
	if err != nil || stranger != nil {
		t.Fatal("another owner read qualification")
	}
	exec := &fakeExecutor{}
	installExecutor(t, exec)
	replacement := newIntegration("qualification-driver-"+suffix, true)
	deliver(t, replacement, after)
	finished := mustRun(t, store, r.ID)
	if finished.Conclusion != ConclusionSuccess || len(exec.sent()) != 3 || finished.DriverNodeID != replacement.snapshot().NodeID {
		t.Fatalf("replacement: %+v calls=%v", finished, exec.sentKeys())
	}
	if !reflect.DeepEqual(before.Qualification, finished.Qualification) {
		t.Fatal("driver rewrote approval")
	}
	req.RequestID = "second"
	second, err := opener.QualifyPullRequest(signedIn(owner), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opener.Disconnect(signedIn(owner), p.ID); err != nil {
		t.Fatal(err)
	}
	deliver(t, replacement, second)
	refused := mustRun(t, store, second.ID)
	if refused.Conclusion != ConclusionFailure || len(exec.sent()) != 3 {
		t.Fatalf("revoked authority dispatched: %+v", refused)
	}
}

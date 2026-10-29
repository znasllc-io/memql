package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// The public builtins must preserve their argument types and receipt through
// the actual engine/API boundary, not just direct calls to the Go integration.
// Each replica owns a separate engine and database pool. Provider reads/checks
// are fixtures; this does not claim a live GitHub qualification.
func TestQualificationBuiltinsKeepAuthorityAndReceiptsAcrossEngines(t *testing.T) {
	base := dbEngine(t) // Ensures the shipped registry is loaded and DB is required.
	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner, pkg, account := "qual-wire-owner-"+suffix, "qual-wire-package-"+suffix, "qual-wire-account-"+suffix
	repo, credential := "qual-wire-"+suffix+"/shop", "qual-wire-grant-"+suffix
	quote := langparser.QuoteString
	_, err := base.Execute(seederCtx(), fmt.Sprintf(`mutation createUser(userId: %s, displayName: "Wire owner", primaryEmail: %s, role: "developer")`, quote(owner), quote(owner+"@example.test")))
	require.NoError(t, err)
	_, err = base.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Wire fixture")`, quote(account)))
	require.NoError(t, err)
	_, err = base.Execute(signedIn(owner), fmt.Sprintf(`mutation createPackage(accountId: %s, packageId: %s, name: "shop", sourceKind: "repo", repoUrl: %s, credentialId: %s)`, quote(account), quote(pkg), quote("https://github.com/"+repo), quote(credential)))
	require.NoError(t, err)
	gh := newFakeGitHub(t)
	gh.repos[repo] = githubapp.RepositoryInfo{ID: 100, FullName: repo, DefaultBranch: "main"}
	pr := qualificationPR()
	pr.HeadRepository, pr.BaseRepository = repo, repo
	gh.pulls[repo] = []githubapp.PullRequestHead{pr}
	manifest := strings.ReplaceAll(driveManifest, "          secrets: [SHOP_TOKEN]\n", "")
	gh.trees[repo+"@"+shaA] = fstest.MapFS{pipelines.ManifestPath: {Data: []byte(manifest)}, "go.mod": {Data: []byte("module example.test/shop\n")}}
	engines := make([]*memql.MemQLEngine, 2)
	for n := range engines {
		pool := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
		db := bun.NewDB(pool, pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		engines[n], err = memql.New(db)
		require.NoError(t, err)
		engines[n].Logger = base.Logger
		require.NoError(t, engines[n].Init(memorynodes.DefaultRegistry()))
		integration := New(Deps{Store: NewDSLStore(engines[n]), GitHub: gh, NodeID: fmt.Sprintf("qualification-wire-%d-%s", n, suffix),
			OSOrigin: func() string { return testOSOrigin }, EngineRevision: func() string { return "qualification-wire-test" },
			Gate: watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
				return githubconnect.WithGate(ctx, pool, key, fn)
			}),
		})
		require.NoError(t, engines[n].RegisterIntegration(integration))
	}
	read := func(engine *memql.MemQLEngine, name string, args map[string]any) map[string]any {
		t.Helper()
		call, err := langparser.RenderCall(name, args)
		require.NoError(t, err)
		result, err := engine.Execute(signedIn(owner), "builtin "+call)
		require.NoError(t, err)
		bundle, values, err := result.ToAPIResult()
		require.NoError(t, err)
		require.Nil(t, bundle)
		require.Len(t, values, 1, "public qualification must return its receipt")
		encoded, err := protojson.Marshal(values[0])
		require.NoError(t, err)
		var decoded structpb.Value
		require.NoError(t, protojson.Unmarshal(encoded, &decoded))
		nodes := decoded.GetStructValue().GetFields()
		require.Len(t, nodes, 1)
		for _, value := range nodes {
			return value.GetStructValue().GetFields()["payload"].GetStructValue().AsMap()
		}
		t.Fatal("missing qualification receipt")
		return nil
	}
	preview := read(engines[0], "pipelinesPreviewPullRequest", map[string]any{"packageId": pkg, "pullRequest": 42})
	require.Equal(t, shaA, preview["headSha"])
	require.Equal(t, shaBase, preview["baseSha"])
	require.Equal(t, float64(42), preview["pullRequest"])
	require.Len(t, preview["manifestDigest"].(string), 64)
	store := NewDSLStore(engines[1])
	p, err := store.PipelineByID(memql.ContextWithFreshRead(context.Background()), PipelineIDFor(pkg))
	require.NoError(t, err)
	require.Nil(t, p, "preview must not reserve the repository")
	require.Empty(t, gh.created)
	args := map[string]any{"packageId": pkg, "pullRequest": 42, "requestId": "wire-request", "expectedHeadSha": preview["headSha"], "expectedBaseSha": preview["baseSha"], "expectedManifestDigest": preview["manifestDigest"], "expiresAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
	call, err := langparser.RenderCall("pipelinesQualifyPullRequest", args)
	require.NoError(t, err)
	_, err = engines[0].Execute(signedInAs(owner, auth.RoleReader), "builtin "+call)
	require.Error(t, err, "ownership alone must not replace the execute capability")
	require.Empty(t, gh.created)
	first := read(engines[0], "pipelinesQualifyPullRequest", args)
	again := read(engines[1], "pipelinesQualifyPullRequest", args)
	require.NotEmpty(t, first["runId"])
	require.Equal(t, first["runId"], again["runId"])
	require.Equal(t, float64(42), again["pullRequest"])
	require.Len(t, gh.created, 1, "a second engine must recover the original request")
	r := mustRun(t, store, first["runId"].(string))
	saved := mustPipeline(t, store, r.PipelineID)
	require.NoError(t, ValidateQualification(saved, r, time.Now().UTC()))
	require.Empty(t, saved.SecretNames, "omitted secret scope cannot invent permission")
	require.Equal(t, pipelines.ComputeCluster, saved.Compute, "omitted compute retains the cluster-only default")
	_, err = engines[1].Execute(signedIn("other-"+suffix), "builtin "+call)
	require.Error(t, err, "another developer cannot recover private owner authority")
	require.Len(t, gh.created, 1)
}

package pipelinerun

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

// A release reuses a pull request's checks over REAL rows: the DSL store and its
// new query, the journal over the engine, the engine's concept checks. Then the
// rows the driver wrote go through the proof itself, pipelines.ProveReusedStep, over
// the owner-scoped reads candidate coverage makes, because a skip the proof refuses
// costs a release its version. integrations/release is another module and cannot run
// the driver, so its own tests prove hand-written journal rows; this is what ties
// those rows to what the driver really writes.
func TestAReleaseReusesItsTreeOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	})

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner := "reuse-owner-" + suffix
	repo := "acme-" + suffix + "/shop"
	pkgID := "reuse-pkg-" + suffix
	account := "reuse-account-" + suffix
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Same tree test")`,
		langparser.QuoteString(account))); err != nil {
		t.Fatalf("createClientAccount: %v", err)
	}
	if _, err := eng.Execute(signedIn(owner), fmt.Sprintf(
		`mutation createPackage(accountId: %s, packageId: %s, name: "shop", sourceKind: "repo", repoUrl: %s, credentialId: %s)`,
		langparser.QuoteString(account), langparser.QuoteString(pkgID), langparser.QuoteString("https://github.com/"+repo),
		langparser.QuoteString("cred-"+suffix))); err != nil {
		t.Fatalf("createPackage: %v", err)
	}
	pkg, err := store.PackageForCaller(signedIn(owner), pkgID)
	if err != nil || pkg == nil {
		t.Fatalf("the source: %+v %v", pkg, err)
	}
	if err := store.CreatePipeline(ctx, Pipeline{
		ID: PipelineIDFor(pkg.ID), OwnerUserID: pkg.OwnerUserID, AccountID: pkg.AccountID, PackageID: pkg.ID, Name: "shop",
		Repository: repo, DefaultBranch: "main", InstallationID: 51234567, CredentialID: pkg.CredentialID, Delivery: DeliveryWebhook,
	}); err != nil {
		t.Fatalf("createPipeline: %v", err)
	}
	p := mustPipeline(t, store, PipelineIDFor(pkg.ID))

	gh := newFakeGitHub(t)
	for _, sha := range []string{shaA, shaB} {
		gh.trees[repo+"@"+sha] = fstest.MapFS{
			pipelines.ManifestPath: {Data: []byte(sameTreeManifest)},
			"go.mod":               {Data: []byte("module acme.test/shop\n")},
			"main.go":              {Data: []byte("package main\n")},
		}
		gh.commitTrees[repo+"@"+sha] = treeOne
	}
	exec := &fakeExecutor{entered: make(chan string, 64)}
	exec.answer = artifactFreeAnswer
	installExecutor(t, exec)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	policy := workflowSource(t, admitEveryReuse)
	d := Deps{
		EngineRevision: func() string { return "db-test-engine-revision" },
		Store:          store, GitHub: gh, Gate: gate, NodeID: "reuse-agent-" + suffix, Logger: quiet,
		OSOrigin:       func() string { return "https://os.example.test" },
		HeartbeatEvery: time.Hour,
		Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			return eng.Execute(ctx, q)
		}), quiet, "reuse-agent-"+suffix),
	}
	bindTestWorkflow(&d, policy)
	integ := New(d)
	integ.EnableDriver()

	drive := func(o Opening) Run {
		t.Helper()
		opened, err := integ.open(ctx, integ.snapshot(), p, o)
		if err != nil || !opened.Opened {
			t.Fatalf("open: %+v %v", opened, err)
		}
		integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, opened.Run))
		waitDrives(t, integ)
		return mustRun(t, store, opened.Run.ID)
	}

	// The pull request, at one commit.
	source := drive(prOpening())
	if source.Conclusion != ConclusionSuccess || source.TreeHash != treeOne {
		t.Fatalf("the pull request: %s on tree %q (refusal %q %q)", source.Conclusion, source.TreeHash, source.RefusalCode, source.RefusalMessage)
	}
	found, err := store.RunsForPipelineTree(memqlengine.ContextWithFreshRead(ctx), p.ID, treeOne)
	if err != nil || len(found) != 1 || found[0].ID != source.ID {
		t.Fatalf("pipelineRunsForPipelineTree: %+v %v", found, err)
	}
	if none, err := store.RunsForPipelineTree(memqlengine.ContextWithFreshRead(ctx), p.ID, treeTwo); err != nil || len(none) != 0 {
		t.Fatalf("another tree's runs: %+v %v", none, err)
	}

	// The release, at the merge commit with its tree.
	before := len(exec.sent())
	release := drive(releaseOpening())
	if release.Conclusion != ConclusionSuccess {
		t.Fatalf("the release: %s (refusal %q %q, failure %q)", release.Conclusion, release.RefusalCode, release.RefusalMessage, release.FailureMessage)
	}
	var sent []string
	for _, req := range exec.sent()[before:] {
		sent = append(sent, req.StepKey)
	}
	slices.Sort(sent)
	if want := []string{releasedVet, releasedImage, releasedAdvisories}; !slices.Equal(sent, want) {
		t.Fatalf("the release executed %v, want %v", sent, want)
	}

	// The proof's reads: the owner's queries, read fresh, identities bare.
	bare := memqlengine.BareShortId(p.OwnerUserID)
	read := func(query string) map[string]any {
		t.Helper()
		res, err := eng.Execute(memqlengine.ContextWithFreshRead(signedIn(bare)), query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		body, err := json.Marshal(res.OutputPayload())
		if err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 1 {
			t.Fatalf("%s answered %s: %v", query, body, err)
		}
		return receiptRow(rows[0])
	}
	runRow := func(id string) (pipelines.SameTreeRun, error) {
		return pipelines.SameTreeRunFromRow(read(fmt.Sprintf(`query pipelineRunForOwner(runId: %s)`, langparser.QuoteString(id))), bareID)
	}
	reads := pipelines.ReusedStepReads{
		PipelineRun: runRow,
		WorkRun: func(id string) (map[string]any, error) {
			return read(fmt.Sprintf(`query workRunForOwner(runId: %s)`, langparser.QuoteString(id))), nil
		},
		WorkStep: func(id, key string) (map[string]any, error) {
			return read(fmt.Sprintf(`query workStepForOwnerRun(runId: %s, stepKey: %s)`, langparser.QuoteString(id), langparser.QuoteString(key))), nil
		},
	}
	reuser, err := runRow(release.ID)
	if err != nil {
		t.Fatal(err)
	}
	stepOf := func(workRun, key string) map[string]any {
		t.Helper()
		row, err := reads.WorkStep(bareID(workRun), key)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	for _, key := range []string{releasedUnit, releasedIntegration} {
		// The whole proof, as coverage runs it, over what the driver wrote.
		proved, err := pipelines.ProveReusedStep(reads, bare, reuser, stepOf(release.WorkRunID, key), key)
		if err != nil {
			t.Fatalf("%s: the proof refuses the receipt the runner reused: %v", key, err)
		}
		executed, err := pipelines.InspectReleaseStepReceipt(bare, bareID(source.WorkRunID), key,
			read(fmt.Sprintf(`query workRunForOwner(runId: %s)`, langparser.QuoteString(bareID(source.WorkRunID)))), stepOf(source.WorkRunID, key))
		if err != nil {
			t.Fatal(err)
		}
		want := pipelines.ReleaseReusedFrom{PipelineRunID: source.ID, WorkRunID: bareID(source.WorkRunID), Attempt: 1, Event: string(pipelines.EventPullRequest), Commit: shaA, TreeHash: treeOne, ReceiptDigest: executed.ReceiptDigest}
		if proved != want {
			t.Errorf("%s: proved %+v\nwant %+v", key, proved, want)
		}
	}

	// And the rest of what coverage holds a complete run to: every declared step is a
	// receipt a candidate can name, under the run's definition fingerprint.
	workRow := read(fmt.Sprintf(`query workRunForOwner(runId: %s)`, langparser.QuoteString(bareID(release.WorkRunID))))
	order := argStrings(workRow, "stepOrder")
	if len(order) == 0 {
		t.Fatalf("the release declared no steps: %v", workRow)
	}
	decls := make([]workjournal.StepDecl, 0, len(order))
	for _, key := range order {
		row := stepOf(release.WorkRunID, key)
		if _, err := pipelines.InspectReleaseStepReceipt(bare, bareID(release.WorkRunID), key, workRow, row); err != nil {
			t.Errorf("%s is not a receipt a candidate can hold: %v", key, err)
		}
		decls = append(decls, workjournal.StepDecl{Key: key, Kind: rowString(row, "kind"), StepType: rowString(row, "stepType"), DependsOn: argStrings(row, "dependsOn"), Call: argObject(row, "call")})
	}
	fingerprint, err := workjournal.DefinitionFingerprint(rowString(workRow, "automationName"), decls)
	if err != nil || fingerprint != workRow["templateFingerprint"] {
		t.Errorf("the rows no longer match the declared execution: %s, want %v (%v)", fingerprint, workRow["templateFingerprint"], err)
	}

	// Retention holds a live candidate's release attempts by these fields, read straight
	// out of the stored rows (retentionHeldBackSQL, integrations/work). A hold that names
	// a field the driver no longer writes holds nothing, and says so only a month later,
	// when a candidate's next overlay is refused.
	runFields := read(fmt.Sprintf(`query pipelineRunForOwner(runId: %s)`, langparser.QuoteString(release.ID)))
	for name, fields := range map[string]map[string]any{"the run row": runFields, "the work run's input": argObject(workRow, "input")} {
		if fields["repository"] != repo || fields["sha"] != release.SHA || fields["event"] != string(pipelines.EventRelease) {
			t.Errorf("%s no longer names the release retention holds it by: %v", name, fields)
		}
	}

	// A run whose tree could not be read is found under no tree. In the query itself a
	// blank value equals an absent field, so a blank tree must read nothing at all.
	gh.mu.Lock()
	gh.commitTreeErr = errors.New("GitHub answered 500")
	gh.mu.Unlock()
	unread := drive(pushOpening())
	if unread.Conclusion != ConclusionSuccess || unread.TreeHash != "" {
		t.Fatalf("a run whose tree could not be read: %s on tree %q", unread.Conclusion, unread.TreeHash)
	}
	if none, err := store.RunsForPipelineTree(memqlengine.ContextWithFreshRead(ctx), p.ID, ""); err != nil || len(none) != 0 {
		t.Errorf("a blank tree answered %d runs, %v", len(none), err)
	}
}

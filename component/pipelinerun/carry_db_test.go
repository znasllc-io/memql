package pipelinerun

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
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

// A push carries a step forward over REAL rows: the earlier push found through
// the DSL store's pipelineRunsForPipelineEvent, its receipts read back through the
// work spine, the carried step's call -- its record of the change, a list inside
// an object -- written by the journal over the engine and read back unchanged. Then
// the two halves that must never stand on what a push carried, run over the rows
// the driver really wrote: the proof a release candidate is held to
// (pipelines.ProveReusedStep, InspectReusedReleaseReceipt), and a release of the
// same tree, which executes the carried step rather than reusing it.
func TestAPushCarriesForwardOverRealRows(t *testing.T) {
	eng := dbEngine(t)
	store := NewDSLStore(eng)
	ctx := context.Background()

	gateDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gateDB.Close() })
	gate := watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, gateDB, key, fn)
	})

	suffix := strings.ReplaceAll(id.NewShortId(), "-", "")[:12]
	owner := "carry-owner-" + suffix
	repo := "acme-" + suffix + "/shop"
	pkgID := "carry-pkg-" + suffix
	account := "carry-account-" + suffix
	if _, err := eng.Execute(seederCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Carry forward test")`,
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

	// Two commits of main, b/b.go changed between them; the second's tree is the
	// one the release below is cut from. With no timing table the go step splits
	// by name: [root b] and [a c], so the change reaches only the first slice.
	gh := newFakeGitHub(t)
	for _, sha := range []string{shaA, shaB} {
		gh.trees[repo+"@"+sha] = fstest.MapFS{
			pipelines.ManifestPath: {Data: []byte(carryManifest)},
			"go.mod":               {Data: []byte("module acme.test/shop\n")},
			"main.go":              {Data: []byte("package main\n")},
			"a/a.go":               {Data: []byte("package a\n")},
			"b/b.go":               {Data: []byte("package b\n")},
			"c/c.go":               {Data: []byte("package c\n\nimport _ \"acme.test/shop/a\"\n")},
		}
	}
	gh.commitTrees[repo+"@"+shaB] = treeOne
	gh.setAncestor(repo, shaA, shaB, "b/b.go")
	exec := &fakeExecutor{entered: make(chan string, 64)}
	exec.answer = artifactFreeAnswer
	installExecutor(t, exec)

	var hours atomic.Int64
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := Deps{
		EngineRevision: func() string { return "db-test-engine-revision" },
		Store:          store, GitHub: gh, Gate: gate, NodeID: "carry-agent-" + suffix, Logger: quiet,
		OSOrigin:       func() string { return "https://os.example.test" },
		HeartbeatEvery: time.Hour,
		Now:            func() time.Time { return time.Now().Add(time.Duration(hours.Load()) * time.Hour) },
		Journal: workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			return eng.Execute(ctx, q)
		}), quiet, "carry-agent-"+suffix),
	}
	bindTestWorkflow(&d, workflowSource(t, carryAncestors))
	integ := New(d)
	integ.EnableDriver()

	drive := func(o Opening) (Run, []string) {
		t.Helper()
		before := len(exec.sent())
		opened, err := integ.open(ctx, integ.snapshot(), p, o)
		if err != nil || !opened.Opened {
			t.Fatalf("open: %+v %v", opened, err)
		}
		integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, opened.Run))
		waitDrives(t, integ)
		var sent []string
		for _, req := range exec.sent()[before:] {
			sent = append(sent, req.StepKey)
		}
		slices.Sort(sent)
		return mustRun(t, store, opened.Run.ID), sent
	}

	first, _ := drive(pushOf(shaA, shaBase))
	if first.Conclusion != ConclusionSuccess {
		t.Fatalf("the first push: %s (refusal %q %q)", first.Conclusion, first.RefusalCode, first.RefusalMessage)
	}
	hours.Store(1)
	push, sent := drive(pushOf(shaB, shaA))
	if push.Conclusion != ConclusionSuccess {
		t.Fatalf("the second push: %s (refusal %q %q, failure %q)", push.Conclusion, push.RefusalCode, push.RefusalMessage, push.FailureMessage)
	}
	if want := []string{carryHistory, carryVet, carryWhole, carryShardOne}; !slices.Equal(sent, want) {
		t.Fatalf("the second push executed %v, want %v", sent, want)
	}

	// The owner's reads, fresh, identities bare: what any reader of the journal,
	// candidate coverage among them, sees.
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
	stepOf := func(workRun, key string) map[string]any {
		t.Helper()
		return read(fmt.Sprintf(`query workStepForOwnerRun(runId: %s, stepKey: %s)`, langparser.QuoteString(bareID(workRun)), langparser.QuoteString(key)))
	}
	runRow := func(id string) (pipelines.SameTreeRun, error) {
		return pipelines.SameTreeRunFromRow(read(fmt.Sprintf(`query pipelineRunForOwner(runId: %s)`, langparser.QuoteString(id))), bareID)
	}

	for _, key := range []string{carryShardTwo, carryDocs} {
		row, executed := stepOf(push.WorkRunID, key), stepOf(first.WorkRunID, key)
		call, sourceCall := argObject(row, "call"), argObject(executed, "call")
		if row["status"] != WorkStepSkipped || row["errorMessage"] != carriedReason(first) || call["evidenceRunId"] != first.ID {
			t.Errorf("%s was not carried from %s: %v", key, first.ID, row)
		}
		if executed["status"] != WorkStepDone {
			t.Errorf("%s stands on a receipt the first push did not execute: %v", key, executed)
		}
		if fp, _ := call["carryFingerprint"].(string); fp == "" || fp != sourceCall["carryFingerprint"] {
			t.Errorf("%s declares %v, the receipt it stands on %v", key, call["carryFingerprint"], sourceCall["carryFingerprint"])
		}
		from := argObject(call, "carriedFrom")
		if from["pipelineRunId"] != first.ID || from["workRunId"] != bareID(first.WorkRunID) || from["sha"] != shaA ||
			!slices.Equal(argStrings(from, "changedPaths"), []string{"b/b.go"}) {
			t.Errorf("%s records %v", key, from)
		}
	}
	// The store reads the record back as the driver declared it.
	rows, err := store.WorkSteps(memqlengine.ContextWithFreshRead(ctx), push.WorkRunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Key != carryShardTwo {
			continue
		}
		if row.CarriedFrom == nil || row.CarriedFrom.PipelineRunID != first.ID || !slices.Equal(row.CarriedFrom.ChangedPaths, []string{"b/b.go"}) || row.CarryFingerprint == "" {
			t.Errorf("the store reads back %+v", row)
		}
	}

	// Every declared call, the record of the change among them, under the run's
	// definition fingerprint: the rows came back as they were declared.
	workRow := read(fmt.Sprintf(`query workRunForOwner(runId: %s)`, langparser.QuoteString(bareID(push.WorkRunID))))
	order := argStrings(workRow, "stepOrder")
	decls := make([]workjournal.StepDecl, 0, len(order))
	for _, key := range order {
		row := stepOf(push.WorkRunID, key)
		decls = append(decls, workjournal.StepDecl{Key: key, Kind: rowString(row, "kind"), StepType: rowString(row, "stepType"), DependsOn: argStrings(row, "dependsOn"), Call: argObject(row, "call")})
	}
	fingerprint, err := workjournal.DefinitionFingerprint(rowString(workRow, "automationName"), decls)
	if err != nil || fingerprint != workRow["templateFingerprint"] {
		t.Errorf("the rows no longer match the declared execution: %s, want %v (%v)", fingerprint, workRow["templateFingerprint"], err)
	}

	// The proof refuses what a push carried, both ways a candidate could reach it.
	reads := pipelines.ReusedStepReads{
		PipelineRun: runRow,
		WorkRun: func(id string) (map[string]any, error) {
			return read(fmt.Sprintf(`query workRunForOwner(runId: %s)`, langparser.QuoteString(id))), nil
		},
		WorkStep: func(id, key string) (map[string]any, error) { return stepOf(id, key), nil },
	}
	pushed, err := runRow(push.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipelines.ProveReusedStep(reads, bare, pushed, stepOf(push.WorkRunID, carryShardTwo), carryShardTwo); err == nil || !strings.Contains(err.Error(), "only a full release") {
		t.Errorf("a push's carried step was proven as a release's reuse: %v", err)
	}
	if _, err := pipelines.InspectReusedReleaseReceipt(bare, bareID(push.WorkRunID), carryShardTwo, workRow, stepOf(push.WorkRunID, carryShardTwo), pushed); err == nil {
		t.Error("a carried row passed as an executed receipt a release could reuse")
	}

	// A release of the second push's tree reuses what that push executed, and
	// executes what it carried: every shard of the go step, and the docs step.
	hours.Store(2)
	release, sent := drive(releaseOpening())
	if release.Conclusion != ConclusionSuccess {
		t.Fatalf("the release: %s (refusal %q %q, failure %q)", release.Conclusion, release.RefusalCode, release.RefusalMessage, release.FailureMessage)
	}
	if want := sortedCopy([]string{carryHistory, carryVet, carryDocs, carryShardOne, carryShardTwo}); !slices.Equal(sent, want) {
		t.Fatalf("the release executed %v, want %v", sent, want)
	}
	reuser, err := runRow(release.ID)
	if err != nil {
		t.Fatal(err)
	}
	proved, err := pipelines.ProveReusedStep(reads, bare, reuser, stepOf(release.WorkRunID, carryWhole), carryWhole)
	if err != nil || proved.PipelineRunID != push.ID {
		t.Fatalf("the release's reuse of what the push executed: %+v %v", proved, err)
	}
}

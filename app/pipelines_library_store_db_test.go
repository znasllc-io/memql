package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// pipelines_library_store_db_test.go -- the pipeline store's file row against
// the REAL DSL (epic memql#5478, issue memql#5495).
//
// The unit tests beside this file drive a fake engine that records the
// composed createLibraryFile call and parses none of it. That is the shape
// that hides a call the engine would refuse -- a retired argument form, an
// enum value the concept lacks, a field the mutation does not declare -- and
// every check on the call's text would stay green. So here a run file goes
// through the store the node builds, over a real engine and a real database,
// under the owner's actor, and is read back through the receipt read a work
// step keys on (integrations/agents' workFileReceipt): the owner's ready file
// for this work run and step.
//
// DB-GATED: it skips without a database, and fails under MEMQL_REQUIRE_DB=1.
func TestAPipelineFileIsARowTheRealLibraryReadsBack(t *testing.T) {
	engine := workTemplateDBEngine(t)
	a := &App{engine: engine, Logger: quietLogger()}
	up := &pipelinesFakeUploader{}
	store := a.pipelinesLibraryStoreFor(up, "memql")

	owner, run := id.NewShortId(), id.NewShortId()
	f := pipelinesteps.RunFile{
		OwnerUserID: owner,
		WorkRunID:   run,
		StepKey:     "tests/go-tests#2",
		Name:        "tests-go-tests-2.log",
		MimeType:    "text/plain; charset=utf-8",
		Bytes:       []byte("=== RUN   TestCheckoutTotals\n--- PASS: TestCheckoutTotals (0.03s)\n"),
	}
	stored, err := store.StoreRunFile(context.Background(), f)
	if err != nil {
		t.Fatalf("StoreRunFile through the real engine: %v", err)
	}
	if stored.FileID == "" || stored.Omitted != "" {
		t.Fatalf("StoreRunFile = %+v, want a stored file", stored)
	}

	call, err := langparser.RenderCall("libraryFilesForOwner", map[string]any{
		"runId": run, "stepKey": f.StepKey, "status": "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := memql.ContextWithFreshRead(auth.ContextWithUserActor(context.Background(), owner))
	res, err := engine.Execute(ownerCtx, "query "+call)
	if err != nil {
		t.Fatalf("the receipt read: %v", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("the owner's ready files for this run and step = %d rows, want exactly this one: %v", len(rows), rows)
	}
	row := rows[0]
	text := func(key string) string { return fmt.Sprint(row[key]) }

	sum := sha256.Sum256(f.Bytes)
	for key, want := range map[string]string{
		"source":            "pipeline",
		"producedByStepKey": f.StepKey,
		"name":              "tests-go-tests-2.log",
		"mimeType":          "text/plain",
		"format":            "text",
		"status":            "ready",
		"sha256":            hex.EncodeToString(sum[:]),
		"size":              fmt.Sprint(len(f.Bytes)),
		"blobUrl":           "https://blob.example/memql/library/" + owner + "/" + stored.FileID + "/tests-go-tests-2.log",
	} {
		if got := text(key); got != want {
			t.Errorf("the stored row's %s = %q, want %q", key, got, want)
		}
	}
	// Ids compared bare, the way the receipt read compares them: a
	// relationship field is stored canonical and may be answered either way.
	for key, want := range map[string]string{
		"id":              stored.FileID,
		"producedByRunId": run,
		"ownerUserId":     owner,
	} {
		if got := memql.BareShortId(text(key)); got != want {
			t.Errorf("the stored row's %s = %q, want %q", key, text(key), want)
		}
	}
}

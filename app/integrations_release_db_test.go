package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/buildinfo"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"github.com/znasllc-io/memql/integrations/release"
)

func candidateEngineRows(t *testing.T, engine *memql.MemQLEngine, ctx context.Context, kind, name string, args map[string]any) []map[string]any {
	t.Helper()
	values := []string{}
	for key, value := range args {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, key+":"+string(encoded))
	}
	result, err := engine.Execute(ctx, kind+" "+name+"("+strings.Join(values, ",")+")")
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		t.Fatalf("%s returned no rows", name)
	}
	return rows
}

// This proof connects real work and upload journals, engine owner reads,
// independent Azure clients and a real Distribution registry. The work receipts
// are fixture executions; this is not the installed Workbench release rehearsal.
func TestReleaseCandidateThroughNativeLibraryAndSecondEngine(t *testing.T) {
	connection := os.Getenv("MEMQL_AZURITE_TEST_CONNECTION_STRING")
	origin, archive := os.Getenv("MEMQL_OCI_TEST_REGISTRY"), os.Getenv("MEMQL_CANDIDATE_TEST_OCI_ARCHIVE")
	if connection == "" || origin == "" || archive == "" {
		t.Skip("requires explicit disposable Azurite, Distribution and OCI archive fixtures")
	}
	if len(buildinfo.Commit()) != 40 {
		t.Fatal("stamp the test binary with its immutable engine commit")
	}
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", connection)
	archiveDigest, imageDigest := os.Getenv("MEMQL_CANDIDATE_TEST_ARCHIVE_DIGEST"), os.Getenv("MEMQL_CANDIDATE_TEST_IMAGE_DIGEST")
	if len(archiveDigest) != 71 || len(imageDigest) != 71 {
		t.Fatal("independently recorded archive and image digests are required")
	}
	info, err := os.Stat(archive)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<30 {
		t.Fatal("invalid archive", err)
	}
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	client, err := azblob.NewClientFromConnectionString(connection, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	container := "candidate-proof-" + id.NewShortId()
	if _, err := client.CreateContainer(ctx, container, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := client.DeleteContainer(cleanup, container, nil); err != nil {
			t.Error(err)
			return
		}
		_, err := client.ServiceClient().NewContainerClient(container).GetProperties(cleanup, nil)
		if !bloberror.HasCode(err, bloberror.ContainerNotFound) {
			t.Error("fixture container deletion was not confirmed", err)
		}
	})
	uploader, err := azureblob.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, engine, db := pipelineStreamDBStore(t, uploader)
	store.bucket = container
	owner := "candidate-owner-" + id.NewShortId()
	actor := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
	actor = auth.ContextWithToken(actor, &auth.TokenInfo{Subject: owner})
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM release_publication_intents WHERE candidate_id IN (SELECT candidate_id FROM release_candidates WHERE owner_user_id=$1)`,
			`DELETE FROM release_candidate_approvals WHERE approved_by=$1`,
			`DELETE FROM release_candidates WHERE owner_user_id=$1`,
			`DELETE FROM pipeline_artifact_references WHERE owner_user_id=$1`,
			`DELETE FROM pipeline_artifact_uploads WHERE owner_user_id=$1`,
			`DELETE FROM "MemoryNodes" WHERE payload->>'ownerUserId'=$1`,
		} {
			if _, err := db.Exec(statement, owner); err != nil {
				t.Error(err)
			}
		}
	})
	commit := strings.Repeat("b", 40)
	fingerprint := "work-definition-v2:" + strings.Repeat("d", 64)
	work := workjournal.Work{OwnerUserID: owner, Template: "pipeline:candidate-proof", GoalKey: id.NewShortId(), RunKey: "1", Statement: "Native candidate protocol fixture", TriggeredBy: pl.WorkTriggerPrefix + "full",
		Input: map[string]any{"repository": "acme/engine", "sha": commit, "mode": "full", "event": "push", "pipelineId": "candidate-fixture", "pipelineRunId": "candidate-fixture", "attempt": 1},
		Steps: []workjournal.StepDecl{
			{Key: "build.image", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fingerprint}},
			{Key: "test.full", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fingerprint}},
		},
	}
	_, runID, err := workjournal.IDs(work)
	if err != nil {
		t.Fatal(err)
	}
	journal := workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return engine.Execute(ctx, q) }), quietLogger(), "candidate-proof-first")
	run, err := journal.Begin(actor, work)
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.StoreRunFileStream(actor, pipelinesteps.StreamRunFile{OwnerUserID: owner, WorkRunID: runID, StepKey: "build.image", Attempt: 1, Path: "output/image.tar", Name: "image.tar", MimeType: "application/x-tar", Size: int64(len(body)), SHA256: strings.TrimPrefix(archiveDigest, "sha256:"), Body: bytes.NewReader(body)})
	if err != nil || file.Receipt == nil {
		t.Fatal("native artifact storage failed", err)
	}
	for _, key := range []string{"build.image", "test.full"} {
		step, err := run.Step(actor, key)
		if err != nil {
			t.Fatal(err)
		}
		metadata := map[string]any{"exitCode": 0}
		if key == "build.image" {
			metadata["artifactIntentIds"] = []string{file.Receipt.IntentID}
		}
		if err := step.Finish(actor, workjournal.Receipt{Status: "done", Result: map[string]any{"status": "succeeded", "metadata": metadata}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Succeeded(actor, map[string]any{"status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	runRow := candidateEngineRows(t, engine, actor, "query", "workRunForOwner", map[string]any{"runId": runID})[0]
	for _, field := range []string{"id", "ownerUserId", "runId"} {
		if text, ok := runRow[field].(string); ok {
			runRow[field] = memql.BareShortId(text)
		}
	}
	candidate := pl.ReleaseCandidate{FormatVersion: 1, OwnerUserID: owner, Components: []pl.ReleaseComponent{{Name: "engine", Version: "0.25.0", Repository: "acme/engine", Commit: commit}}}
	for _, key := range []string{"build.image", "test.full"} {
		row := candidateEngineRows(t, engine, actor, "query", "workStepForOwnerRun", map[string]any{"runId": runID, "stepKey": key})[0]
		for _, field := range []string{"id", "ownerUserId", "runId"} {
			if text, ok := row[field].(string); ok {
				row[field] = memql.BareShortId(text)
			}
		}
		receipt, err := pl.InspectReleaseWorkReceipt(owner, runID, key, runRow, row)
		if err != nil {
			t.Fatal(err)
		}
		candidate.Evidence = append(candidate.Evidence, pl.ReleaseEvidence{Name: key, Component: "engine", WorkRunID: runID, StepKey: key, Attempt: receipt.Attempt, ReceiptID: receipt.ReceiptID, ReceiptDigest: receipt.ReceiptDigest, DefinitionDigest: receipt.DefinitionDigest, ArtifactIntentIDs: receipt.ArtifactIntentIDs})
		if key == "build.image" {
			candidate.Components[0].Artifacts = []pl.ReleaseArtifact{{Name: "image", Kind: "oci", Platform: "linux/arm64", Digest: archiveDigest, Size: int64(len(body)), ImageDigest: imageDigest, Receipt: pl.ReleaseReceiptReference{WorkRunID: runID, StepKey: key, Attempt: 1, IntentID: file.Receipt.IntentID, ReceiptDigest: receipt.ReceiptDigest, DefinitionDigest: receipt.DefinitionDigest}}}
		}
	}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/repos/acme/engine/contents/VERSION" || r.URL.Query().Get("ref") != commit || r.Header.Get("Authorization") != "Bearer fixture-version-reader" {
			t.Error("unexpected source-version request")
			w.WriteHeader(403)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("0.25.0\n")), "sha": commit})
	}))
	defer github.Close()
	configuration, err := json.Marshal(map[string]any{"formatVersion": 1, "sources": []any{map[string]any{"component": "engine", "repository": "acme/engine", "path": "VERSION", "credentialSecret": "CANDIDATE_FIXTURE_SOURCE"}}, "registries": []any{map[string]any{"id": "registry", "component": "engine", "artifact": "image", "origin": origin, "repository": "candidate/" + id.NewShortId(), "allowLoopbackHttp": true}}})
	if err != nil {
		t.Fatal(err)
	}
	register := func(e *memql.MemQLEngine, lib release.CandidateLibrary, database *sql.DB) {
		for _, registration := range memql.RegisteredPlugins() {
			if registration.Name != "release" {
				continue
			}
			provider, err := registration.Factory(memql.PluginContext{Logger: quietLogger(), Engine: e,
				ResolveSystemVariable: func(_ context.Context, name string) (string, error) {
					if name != release.CandidateConfigurationVariable {
						return "", nil
					}
					return string(configuration), nil
				},
				ResolveSystemSecret: func(_ context.Context, name string) (string, error) {
					if name != "CANDIDATE_FIXTURE_SOURCE" {
						t.Fatal("unexpected credential lookup")
					}
					return "fixture-version-reader", nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			integration := provider.(*release.Integration)
			if err := integration.ConfigureCandidates(release.CandidateDependencies{Database: func() *sql.DB { return database }, Library: lib, SourceClient: release.NewClient().WithBaseURL(github.URL)}); err != nil {
				t.Fatal(err)
			}
			if err := e.RegisterIntegration(integration); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Fatal("release plugin was not materialized")
	}
	register(engine, store, db)
	configured := candidateEngineRows(t, engine, actor, "builtin", "releaseCandidateConfiguration", nil)[0]
	targets, err := json.Marshal(configured["targets"])
	if err != nil {
		t.Fatal(err)
	}
	var destinationRows []map[string]any
	if err := json.Unmarshal(targets, &destinationRows); err != nil || len(destinationRows) != 1 {
		t.Fatal("missing configured destination", err)
	}
	destination := destinationRows[0]
	candidate.Destinations = []pl.ReleaseDestination{{TargetID: "registry", TargetDigest: fmt.Sprint(destination["targetDigest"]), Component: "engine", Artifact: "image", Operation: "publish"}}
	prepared := candidateEngineRows(t, engine, actor, "builtin", "releasePrepareCandidate", map[string]any{"candidate": candidate})[0]
	key := fmt.Sprint(prepared["candidateId"])
	if prepared["state"] != "ready" {
		t.Fatal("candidate did not become ready", prepared["state"])
	}
	// A second engine and independent blob client approve and publish without
	// using the first reader's state, prepared handle, or artifact stream.
	secondUploader, err := azureblob.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondStore, secondEngine, secondDB := pipelineStreamDBStore(t, secondUploader)
	secondStore.bucket = container
	register(secondEngine, secondStore, secondDB)
	approved := candidateEngineRows(t, secondEngine, actor, "builtin", "releaseApproveCandidate", map[string]any{"candidateId": key})[0]
	approval := fmt.Sprint(approved["approvalId"])
	if approved["state"] != "approved" || approval == "" {
		t.Fatal("approval not persisted")
	}
	review := candidateEngineRows(t, engine, actor, "builtin", "releaseGetCandidate", map[string]any{"candidateId": key})[0]
	if review["approvalId"] != approval {
		t.Fatal("other replica could not recover approval")
	}
	history := candidateEngineRows(t, secondEngine, actor, "builtin", "releaseCandidates", map[string]any{"limit": 1})[0]
	historyBody, err := json.Marshal(history["candidates"])
	if err != nil {
		t.Fatal(err)
	}
	var summaries []map[string]any
	if err := json.Unmarshal(historyBody, &summaries); err != nil || len(summaries) != 1 || summaries[0]["candidateId"] != key || summaries[0]["state"] != "approved" {
		t.Fatal("other replica could not discover approved candidate", err)
	}
	publication := candidateEngineRows(t, secondEngine, actor, "builtin", "releasePublishCandidate", map[string]any{"candidateId": key, "approvalId": approval, "targetId": "registry", "component": "engine", "artifact": "image"})[0]
	if publication["state"] != "complete" {
		t.Fatal("native publication incomplete")
	}
	t.Logf("native journal+Library+Azurite+second-engine publication: candidate=%s effect=%s image=%s", key, publication["effectId"], imageDigest)
}

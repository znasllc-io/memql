package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func assemblyEngine(t *testing.T, db *sql.DB) *memql.MemQLEngine {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine, err := memql.New(bun.NewDB(db, pgdialect.New()))
	if err != nil {
		t.Fatal(err)
	}
	engine.Logger = slog.New(slog.DiscardHandler)
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return engine
}

func assemblyCompletedRun(t *testing.T, engine *memql.MemQLEngine, repository, commit, mode, outcome string, intents []string) string {
	t.Helper()
	owner, _ := candidateOwner(ownerCtx())
	journal := workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return engine.Execute(ctx, q) }), slog.New(slog.DiscardHandler), "assembly-producer")
	fp := "work-definition-v2:" + strings.Repeat("d", 64)
	work := workjournal.Work{OwnerUserID: owner, Template: "pipeline:assembly", GoalKey: id.NewShortId(), RunKey: "1", Statement: "Assembly fixture", TriggeredBy: pl.WorkTriggerPrefix + mode,
		Input: map[string]any{"repository": repository, "sha": commit, "mode": mode, "event": "push", "pipelineId": "assembly-fixture", "pipelineRunId": id.NewShortId(), "attempt": 1},
		Steps: []workjournal.StepDecl{
			{Key: "build.image", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fp}},
			{Key: "test.full", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fp}},
		}}
	_, runID, err := workjournal.IDs(work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Begin(ownerCtx(), work)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"build.image", "test.full"} {
		step, err := run.Step(ownerCtx(), key)
		if err != nil {
			t.Fatal(err)
		}
		metadata := map[string]any{"exitCode": 0}
		if key == "build.image" {
			metadata["artifactIntentIds"] = intents
		}
		result, status := "succeeded", "done"
		if outcome == "failed check" && key == "test.full" {
			status, result, metadata["exitCode"] = "failed", "failed", 1
		}
		if err := step.Finish(ownerCtx(), workjournal.Receipt{Status: status, Result: map[string]any{"status": result, "metadata": metadata}}); err != nil {
			t.Fatal(err)
		}
	}
	if outcome != "unfinished" {
		if err := run.Succeeded(ownerCtx(), map[string]any{"status": "succeeded"}); err != nil {
			t.Fatal(err)
		}
	}
	return runID
}

func assemblyPreparationFixture(t *testing.T, db *sql.DB, producer, reader *memql.MemQLEngine, mode, outcome string) (*candidatePreparer, map[string]string, *assemblyLibraryFixture) {
	t.Helper()
	p, c, library, _ := candidatePreparationFixture(t, db)
	metadataID := strings.Repeat("f", 64)
	runID := assemblyCompletedRun(t, producer, c.Components[0].Repository, c.Components[0].Commit, mode, outcome, []string{library.receipt.IntentID, metadataID})
	library.receipt.WorkRunID, library.receipt.StepKey, library.receipt.Path = runID, "build.image", "build/image.oci.tar"
	l := &assemblyLibraryFixture{candidateLibraryFixture: library, metadata: library.receipt}
	l.metadata.IntentID, l.metadata.Path, l.metadata.ETag = metadataID, "build/metadata.json", "metadata-v1"
	metadata, _ := json.Marshal(map[string]any{"containerimage.digest": c.Components[0].Artifacts[0].ImageDigest})
	l.setMetadata(metadata)
	p.library = l
	p.evidence = candidateEvidenceReader{engine: evidenceFreshRead{t, reader}}
	p.assemblyPlans = []candidateAssemblyPlan{assemblyTestPlan()}
	return p, map[string]string{"engine_arm64": runID}, l
}

func TestCandidateAssemblyUsesRealCompleteJournalAcrossHosts(t *testing.T) {
	db := candidateTestDB(t)
	producer, reader := assemblyEngine(t, db), assemblyEngine(t, db)
	p, runs, library := assemblyPreparationFixture(t, db, producer, reader, "full", "")
	record, err := p.assemble(ownerCtx(), "coordinated", runs)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, record.ID)
	if record.State != "ready" || record.ApprovalID != "" || len(record.Manifest.Evidence) != 2 || record.Manifest.Components[0].Version != "0.25.0" {
		t.Fatal("assembly did not resolve complete native facts", record)
	}
	if library.metadataCloses != 2 || library.opens != 1 || library.closes != 1 {
		t.Fatal("discovery or verification stream leaked")
	}
	var effects int
	if err := db.QueryRow(`SELECT count(*) FROM release_publication_intents WHERE candidate_id=$1`, record.ID).Scan(&effects); err != nil || effects != 0 {
		t.Fatal("assembly performed publication", effects, err)
	}
	// A new native host owns no call-local assembly state. Both the producer
	// and the first reader are absent from this path.
	other := *p
	other.evidence = candidateEvidenceReader{engine: evidenceFreshRead{t, assemblyEngine(t, db)}}
	other.ledger = &candidateLedger{db: func() *sql.DB { return db }}
	again, err := other.assemble(ownerCtx(), "coordinated", runs)
	if err != nil || again.ID != record.ID || again.State != "ready" {
		t.Fatal("assembly did not survive another host", again, err)
	}
	if _, err := other.ledger.approve(ownerCtx(), record.ID); err != nil {
		t.Fatal(err)
	}
	other.assemblyPlans = []candidateAssemblyPlan{assemblyTestPlan()}
	other.assemblyPlans[0].Name = "review-again"
	changed, err := other.assemble(ownerCtx(), "review-again", runs)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, changed.ID)
	if changed.ID == record.ID || changed.ApprovalID != "" || changed.State != "ready" {
		t.Fatal("changed plan reused approval")
	}
	definition, err := workflowhost.Load(candidateAssembleWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	other.assemblyWorkflow, err = automations.NewLoader(automations.LoaderOptions{}).Snapshot(definition)
	if err != nil {
		t.Fatal(err)
	}
	other.assemblyWorkflow.Name = "changedAssemblyPolicy"
	policyChanged, err := other.prepare(ownerCtx(), changed.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, policyChanged.ID)
	if policyChanged.ID == changed.ID {
		t.Fatal("changed assembly recipe reused identity")
	}
}

func TestCandidateAssemblyRefusesIncompleteOrSubstitutedInputs(t *testing.T) {
	db := candidateTestDB(t)
	producer, reader := assemblyEngine(t, db), assemblyEngine(t, db)
	for _, failure := range []string{"partial mode", "unfinished", "failed check", "unowned", "mixed commits", "missing input", "extra input", "missing artifact", "duplicate path", "foreign receipt", "metadata different producer", "invalid metadata digest"} {
		t.Run(failure, func(t *testing.T) {
			mode := "full"
			if failure == "partial mode" {
				mode = "affected"
			}
			p, runs, library := assemblyPreparationFixture(t, db, producer, reader, mode, failure)
			ctx := ownerCtx()
			switch failure {
			case "unowned":
				ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "other-owner", Role: auth.RoleOwner})
			case "mixed commits":
				p.assemblyPlans[0].Components[0].Runs = append(p.assemblyPlans[0].Components[0].Runs, "other_run")
				runs["other_run"] = assemblyCompletedRun(t, producer, "acme/engine", strings.Repeat("e", 40), "full", "", nil)
			case "missing input":
				delete(runs, "engine_arm64")
			case "extra input":
				runs["extra"] = "unknown"
			case "missing artifact":
				p.assemblyPlans[0].Components[0].Artifacts[0].Path = "unproduced/image.oci.tar"
			case "duplicate path":
				library.rows = func(rows []pipelinesteps.StoredFileReceipt) []pipelinesteps.StoredFileReceipt {
					rows[1].Path = rows[0].Path
					return rows
				}
			case "foreign receipt":
				library.rows = func(rows []pipelinesteps.StoredFileReceipt) []pipelinesteps.StoredFileReceipt {
					rows[0].OwnerUserID = "other-owner"
					return rows
				}
			case "metadata different producer":
				library.rows = func(rows []pipelinesteps.StoredFileReceipt) []pipelinesteps.StoredFileReceipt {
					rows[1].StepKey = "test.full"
					return rows
				}
			case "invalid metadata digest":
				library.setMetadata([]byte(`{"containerimage.digest":"sha256:` + strings.Repeat("e", 64) + `"}`))
			}
			if _, err := p.assemble(ctx, "coordinated", runs); err == nil {
				t.Fatal("invalid assembly accepted", failure)
			}
			key, state := candidatePreparedID(t, db, library.receipt.WorkRunID)
			if key != "" {
				cleanupCandidate(t, db, key)
				if state != "preparing" {
					t.Fatal("invalid assembly became approvable", state)
				}
			}
			if library.opens != library.closes {
				t.Fatal("archive stream leaked")
			}
		})
	}
}

func TestCandidateAssemblyProjectionCannotOmitDeclaredEvidence(t *testing.T) {
	db := candidateTestDB(t)
	engine := assemblyEngine(t, db)
	p, runs, l := assemblyPreparationFixture(t, db, engine, engine, "full", "")
	owner, _ := candidateOwner(ownerCtx())
	s := &candidateAssemblyScope{p: p, owner: owner, plan: p.assemblyPlans[0], runs: runs, reader: p.evidence.(candidateEvidenceReader), library: l,
		resolvedRuns: map[string]candidateRunPlan{}, versions: map[string]pl.ReleaseComponent{}, artifacts: map[string]pl.ReleaseArtifact{}, targets: map[string]pl.ReleaseDestination{}}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"releaseAssemblyReadRun", map[string]any{"componentName": "engine", "runName": "engine_arm64"}},
		{"releaseAssemblyReadVersion", map[string]any{"componentName": "engine"}},
		{"releaseAssemblyReadArtifact", map[string]any{"componentName": "engine", "artifactName": "image"}},
		{"releaseAssemblyReadTarget", map[string]any{"targetId": "registry"}},
	} {
		if _, err := s.operations()[call.name](ownerCtx(), call.args); err != nil {
			t.Fatal(err)
		}
	}
	want, err := s.resolved()
	if err != nil {
		t.Fatal(err)
	}
	want.Evidence = want.Evidence[:1]
	if _, err := s.prepare(ownerCtx(), map[string]any{"candidate": assemblyOrdinaryMap(t, want)}); err == nil {
		t.Fatal("DSL projection omitted completed checks")
	}
	if len(l.pins) != 0 || l.opens != 0 {
		t.Fatal("invalid projection reached artifact preparation")
	}
}

type assemblyStores map[string]*assemblyLibraryFixture

func (s assemblyStores) ReadRunFileReceipts(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, ids []string) ([]pipelinesteps.StoredFileReceipt, error) {
	return s[scope.WorkRunID].ReadRunFileReceipts(ctx, scope, ids)
}
func (s assemblyStores) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	return s[ref.Scope.WorkRunID].PinRunFileReceipts(ctx, ref)
}
func (s assemblyStores) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	return s[ref.Scope.WorkRunID].ReleaseRunFileReference(ctx, ref)
}
func (s assemblyStores) RetireRunFile(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) (pipelinesteps.RetiredFileReceipt, error) {
	return s[scope.WorkRunID].RetireRunFile(ctx, scope, id)
}
func (s assemblyStores) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	return s[scope.WorkRunID].OpenRunFileReceipt(ctx, scope, id)
}

func TestCandidateAssemblyCombinesIndependentRepositoriesAndArtifactKinds(t *testing.T) {
	db := candidateTestDB(t)
	producer, reader := assemblyEngine(t, db), assemblyEngine(t, db)
	p, runs, image := assemblyPreparationFixture(t, db, producer, reader, "full", "")
	_, _, base, _ := candidatePreparationFixture(t, db)
	base.receipt.IntentID, base.receipt.Path = strings.Repeat("1", 64), "dist/client.tar"
	commit := strings.Repeat("e", 40)
	clientRun := assemblyCompletedRun(t, producer, "acme/client", commit, "full", "", []string{base.receipt.IntentID})
	base.receipt.WorkRunID = clientRun
	file := &assemblyLibraryFixture{candidateLibraryFixture: base}
	p.library = assemblyStores{image.receipt.WorkRunID: image, clientRun: file}
	source := newFakeGitHub(t, nil, "different-main").withVersionFile("0.25.0\n")
	source.files["client/VERSION"] = "1.2.3\n"
	p.versionReader.client = NewClient().WithBaseURL(source.server.URL)
	p.versionReader.sources["client"] = candidateVersionSource{Component: "client", Repository: "acme/client", Path: "client/VERSION"}
	p.targetReader.files = map[string]candidateFileTarget{"client-draft": {ID: "client-draft", Component: "client", Artifact: "archive", APIOrigin: "https://api.github.com", UploadOrigin: "https://uploads.github.com", Repository: "acme/client", SourceCommit: commit, Tag: "v1.2.3", ReleaseID: 73, AssetName: "client.tar", CredentialSecret: "RELEASE_TOKEN"}}
	p.assemblyPlans[0].Components = append(p.assemblyPlans[0].Components, candidateAssemblyComponent{Name: "client", Runs: []string{"client_build"}, Artifacts: []candidateAssemblyArtifact{{Name: "archive", Run: "client_build", StepKey: "build.image", Path: "dist/client.tar", Kind: "file", Platform: "darwin/arm64"}}})
	p.assemblyPlans[0].Targets = append(p.assemblyPlans[0].Targets, "client-draft")
	p.assemblyPlans[0].Compatibility = []pl.ReleaseCompatibility{{Component: "client", Requires: "engine", MinVersion: "0.25.0", MaxExclusive: "0.26.0"}}
	runs["client_build"] = clientRun
	record, err := p.assemble(ownerCtx(), "coordinated", runs)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, record.ID)
	if record.State != "ready" || len(record.Manifest.Components) != 2 || len(record.Manifest.Evidence) != 4 || len(record.Manifest.Destinations) != 2 {
		t.Fatal("multi-repository candidate lost inputs", record)
	}
	client := record.Manifest.Components[0]
	if client.Name != "client" || client.Version != "1.2.3" || client.Repository != "acme/client" || client.Commit != commit || client.Artifacts[0].Kind != "file" {
		t.Fatal("component source or artifact kind crossed", client)
	}
	p.assemblyPlans[0].Compatibility[0].MinVersion = "0.26.0"
	if _, err := p.assemble(ownerCtx(), "coordinated", runs); err == nil {
		t.Fatal("incompatible components were assembled")
	}
}

package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

const profileFixtureSource = `@template
automation fixtureProfile {
 args { facts object! }
 facts := args.facts
 if facts.components.0.runs.0.event != "release" || facts.components.0.runs.0.mode != "full" {
   return {components: [], publications: []}
 }
 return {
  components: [{name: "engine", artifacts: [{name: "image", run: "build", stepKey: "build.image", path: "build/image.oci.tar", imageMetadataPath: "build/metadata.json", kind: "oci", platform: "linux/arm64"}]}],
  publications: [{targetId: "registry", tag: facts.components.0.version}]
 }
}`

func profileMutation(t *testing.T, engine *memql.MemQLEngine, name string, args map[string]any) {
	t.Helper()
	owner, err := candidateOwner(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	// Match the native pipeline store's original-owner write context.
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ownerCtx(), owner))
	if _, err := engine.Execute(ctx, "mutation "+renderCall(name, args)); err != nil {
		t.Fatal(name, err)
	}
}

// Real native pipeline rows and real journal receipts, read through a separate
// engine. The fixture never supplies an approval or an evidence verdict to DSL.
func profileCompletedRun(t *testing.T, engine *memql.MemQLEngine, repository, commit, version, event, mode string, intents []string, attempts ...int) (string, string) {
	t.Helper()
	pipelineAttempt, workAttempt := 1, 1
	if len(attempts) > 0 {
		pipelineAttempt, workAttempt = attempts[0], attempts[0]
	}
	if len(attempts) > 1 {
		workAttempt = attempts[1]
	}
	owner, _ := candidateOwner(ownerCtx())
	pid, rid := id.NewShortId(), id.NewShortId()
	profileMutation(t, engine, "createPipeline", map[string]any{"pipelineId": pid, "packageId": id.NewShortId(), "name": "Profile fixture", "repository": repository, "defaultBranch": "main", "installationId": "1", "credentialId": id.NewShortId(), "delivery": "webhook"})
	profileMutation(t, engine, "createPipelineRun", map[string]any{"runId": rid, "pipelineId": pid, "repository": repository, "sha": commit, "mode": mode, "event": event, "runKey": id.NewShortId(), "attempt": pipelineAttempt, "trigger": "manual", "releaseTag": "v" + version, "version": "v" + version, "status": "completed", "conclusion": "success", "queuedAt": time.Now().UTC().Format(time.RFC3339)})
	journal := workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return engine.Execute(ctx, q) }), slog.New(slog.DiscardHandler), "profile-producer")
	fp := "work-definition-v2:" + strings.Repeat("d", 64)
	work := workjournal.Work{OwnerUserID: owner, Template: "pipeline:profile", GoalKey: id.NewShortId(), RunKey: "1", Statement: "Profile fixture", TriggeredBy: pl.WorkTriggerPrefix + mode,
		Input: map[string]any{"repository": repository, "sha": commit, "mode": mode, "event": event, "pipelineId": pid, "pipelineRunId": rid, "attempt": workAttempt},
		Steps: []workjournal.StepDecl{{Key: "build.image", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fp}}, {Key: "test.full", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fp}}}}
	_, wid, err := workjournal.IDs(work)
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
		if err := step.Finish(ownerCtx(), workjournal.Receipt{Status: "done", Result: map[string]any{"status": "succeeded", "metadata": metadata}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Succeeded(ownerCtx(), map[string]any{"status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	profileMutation(t, engine, "updatePipelineRun", map[string]any{"runId": rid, "workRunId": wid})
	return wid, rid
}

type profileDBFixture struct {
	i        *Integration
	db       *sql.DB
	producer *memql.MemQLEngine
	config   *candidateProfileConfiguration
	source   *string
	runs     map[string]string
	runID    string
	library  *assemblyLibraryFixture
}

func newProfileDBFixture(t *testing.T, event, mode string, attempts ...int) profileDBFixture {
	t.Helper()
	db := candidateTestDB(t)
	producer, reader := assemblyEngine(t, db), assemblyEngine(t, db)
	p, c, library, _ := candidatePreparationFixture(t, db)
	metadataID := strings.Repeat("f", 64)
	wid, rid := profileCompletedRun(t, producer, c.Components[0].Repository, c.Components[0].Commit, "0.25.0", event, mode, []string{library.receipt.IntentID, metadataID}, attempts...)
	library.receipt.WorkRunID, library.receipt.StepKey, library.receipt.Path = wid, "build.image", "build/image.oci.tar"
	l := &assemblyLibraryFixture{candidateLibraryFixture: library, metadata: library.receipt}
	l.metadata.IntentID, l.metadata.Path, l.metadata.ETag = metadataID, "build/metadata.json", "metadata-v1"
	metadata, _ := json.Marshal(map[string]any{"containerimage.digest": c.Components[0].Artifacts[0].ImageDigest})
	l.setMetadata(metadata)
	config := &candidateProfileConfiguration{FormatVersion: 1, Profiles: []candidateProfile{profileTestAuthority()}}
	source := profileFixtureSource
	i := NewIntegration(nil, reader, resolver{systemVariable: func(ctx context.Context, name string) (string, error) {
		if !memql.FreshReadFromContext(ctx) {
			t.Fatal("profile authority was cached")
		}
		if name != CandidateProfilesVariable {
			return "", errors.New("legacy configuration intentionally absent")
		}
		body, _ := json.Marshal(config)
		return string(body), nil
	}})
	i.profileSource = func(kind, name string) (string, error) {
		if kind == "automation" && name == "fixtureProfile" {
			return source, nil
		}
		return "", workflowhost.ErrSourceNotFound
	}
	i.profileRevision = func() string { return strings.Repeat("d", 40) }
	if err := i.ConfigureCandidates(CandidateDependencies{Database: func() *sql.DB { return db }, Library: l, SourceClient: p.versionReader.client}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM release_candidate_profile_bindings WHERE body->'runs'->>'build'=$1`, wid); err != nil {
			t.Error(err)
		}
	})
	return profileDBFixture{i, db, producer, config, &source, map[string]string{"build": wid}, rid, l}
}

func (f profileDBFixture) materialize(t *testing.T) (*candidatePreparer, candidateProfileBinding) {
	t.Helper()
	_, _, digest, err := f.i.candidateProfile(ownerCtx(), "engine")
	if err != nil {
		t.Fatal(err)
	}
	p, b, err := f.i.materializeProfile(ownerCtx(), "engine", digest, f.runs)
	if err != nil {
		t.Fatal(err)
	}
	return p, b
}

func TestCandidateProfileRecoversImmutableSelectionAcrossReplicas(t *testing.T) {
	f := newProfileDBFixture(t, "release", "full")
	p, b := f.materialize(t)
	if b.Facts.Components[0].Runs[0].PipelineRunID != f.runID || b.Facts.Components[0].ReleaseTag != "v0.25.0" {
		t.Fatal("native association absent", b.Facts)
	}
	if len(f.library.pins) != 0 {
		t.Fatal("materialization pinned before native preparation")
	}
	// Interrupt after native selection, then resume with a fresh engine/integration.
	peer := NewIntegration(nil, assemblyEngine(t, f.db), f.i.resolver)
	peer.profileSource = f.i.profileSource
	peer.profileRevision = f.i.profileRevision
	if err := peer.ConfigureCandidates(*f.i.candidateDeps.Load()); err != nil {
		t.Fatal(err)
	}
	other, recovered, err := peer.materializeProfile(ownerCtx(), "engine", b.Configuration, f.runs)
	if err != nil || !reflect.DeepEqual(b, recovered) {
		t.Fatal("materialization did not recover", err)
	}
	record, err := other.assemble(ownerCtx(), "engine", f.runs)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, f.db, record.ID)
	if record.State != "ready" || record.ApprovalID != "" {
		t.Fatal("implicit approval", record)
	}
	resumed, err := f.i.configuredCandidateFor(ownerCtx(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := resumed.prepare(ownerCtx(), record.Manifest)
	if err != nil || again.ID != record.ID {
		t.Fatal("manifest resume lost binding", err)
	}
	if p.profileBinding.Digest != resumed.profileBinding.Digest {
		t.Fatal("replica changed selection")
	}
	approved, err := resumed.approve(ownerCtx(), record.ID)
	if err != nil || approved.ApprovalID == "" {
		t.Fatal("separate approval failed", err)
	}
	rows, err := peer.handleCandidateTargets(ownerCtx(), map[string]any{"candidateId": record.ID}, 1)
	if err != nil || len(rows) != 1 {
		t.Fatal("candidate-bound targets unavailable", err)
	}
	wire := rows[0].Payload
	var projection struct {
		CandidateID string                `json:"candidateId"`
		Targets     []candidateTargetView `json:"targets"`
	}
	if json.Unmarshal(wire, &projection) != nil || projection.CandidateID != record.ID || len(projection.Targets) != 1 || projection.Targets[0].TargetDigest != record.Manifest.Destinations[0].TargetDigest {
		t.Fatal("target wire projection lost its candidate or authority binding", string(wire))
	}
	// An unrelated valid profile cannot invalidate this selected recipe.
	unrelated := profileTestAuthority()
	unrelated.Name = "other"
	unrelated.Registries[0].Repository = "acme/other"
	f.config.Profiles = append(f.config.Profiles, unrelated)
	current, err := peer.configuredCandidateFor(ownerCtx(), record.ID)
	if err != nil {
		t.Fatal("unrelated profile invalidated candidate", err)
	}
	if _, err := current.approve(ownerCtx(), record.ID); err != nil {
		t.Fatal(err)
	}
	var effects int
	if err := f.db.QueryRow(`SELECT count(*) FROM release_publication_intents WHERE candidate_id=$1`, record.ID).Scan(&effects); err != nil || effects != 0 {
		t.Fatal("preparation approved a remote effect", effects, err)
	}
}

// A product resolver may answer an empty optional list. The first call must
// accept its own stored row, not only a retry that reuses the row.
func TestCandidateProfileFirstSelectionAcceptsEmptyCompatibility(t *testing.T) {
	f := newProfileDBFixture(t, "release", "full")
	*f.source = strings.Replace(*f.source, `publications: [{targetId`, `compatibility: [], publications: [{targetId`, 1)
	_, first := f.materialize(t)
	_, again, err := f.i.materializeProfile(ownerCtx(), "engine", first.Configuration, f.runs)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("first selection differs from the one a retry resumes", err)
	}
}

// Values the JSON round trip drops are the same durable selection. A different
// selection another replica stored under the same ID still refuses.
func TestCandidateProfileBindingRefusesOnlyADifferentSelection(t *testing.T) {
	f := newProfileDBFixture(t, "release", "full")
	_, b := f.materialize(t)
	storage, err := f.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	same, snapshot := b, *b.Snapshot
	snapshot.Entries, snapshot.Phases = []string{}, map[string]string{}
	same.Snapshot, same.Selection.Compatibility = &snapshot, []pl.ReleaseCompatibility{}
	if stored, err := storage.ledger.saveProfileBinding(ownerCtx(), same); err != nil || stored.Digest != b.Digest {
		t.Fatal("equivalent selection refused", err)
	}
	other := b
	other.Selection.Publications = append(b.Selection.Publications[:0:0], b.Selection.Publications...)
	other.Selection.Publications[0].Tag = "v" + other.Selection.Publications[0].Tag
	if _, err := storage.ledger.saveProfileBinding(ownerCtx(), other); err == nil || !strings.Contains(err.Error(), "disagrees with its durable materialization") {
		t.Fatal("different selection accepted", err)
	}
}

func TestCandidateProfileRefusesRevokedAuthorityBeforeEffect(t *testing.T) {
	f := newProfileDBFixture(t, "release", "full")
	p, b := f.materialize(t)
	called := false
	ops := profileOperations(p, map[string]workflowhost.Operation{"effect": func(context.Context, map[string]any) (any, error) { called = true; return nil, nil }})
	f.config.Profiles[0].Registries[0].Repository = "acme/changed"
	if _, err := ops["effect"](ownerCtx(), nil); err == nil || called {
		t.Fatal("stale in-flight profile dispatched effect")
	}
	if _, _, err := f.i.materializeProfile(ownerCtx(), "engine", b.Configuration, f.runs); err == nil {
		t.Fatal("old reviewed digest accepted")
	}
	f.config.Profiles = nil
	if _, err := ops["effect"](ownerCtx(), nil); err == nil || called {
		t.Fatal("removed profile dispatched effect")
	}
}

func TestCandidateProfileRefusesSubstitutedRunAndPolicy(t *testing.T) {
	for _, kind := range []string{"foreign owner", "wrong work link", "wrong source", "cancelled", "failed", "nonrelease", "partial", "changed recipe", "corrupt journal"} {
		t.Run(kind, func(t *testing.T) {
			event, mode := "release", "full"
			if kind == "nonrelease" {
				event = "push"
			}
			if kind == "partial" {
				mode = "affected"
			}
			f := newProfileDBFixture(t, event, mode)
			_, _, digest, err := f.i.candidateProfile(ownerCtx(), "engine")
			if err != nil {
				t.Fatal(err)
			}
			ctx := ownerCtx()
			switch kind {
			case "foreign owner":
				ctx = auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:another", Role: auth.RoleOwner})
			case "wrong work link":
				profileMutation(t, f.producer, "updatePipelineRun", map[string]any{"runId": f.runID, "workRunId": id.NewShortId()})
			case "wrong source":
				f.config.Profiles[0].Components[0].Repository = "acme/other"
				_, _, digest, err = f.i.candidateProfile(ownerCtx(), "engine")
				if err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				profileMutation(t, f.producer, "updatePipelineRun", map[string]any{"runId": f.runID, "cancelRequested": true})
			case "failed":
				profileMutation(t, f.producer, "updatePipelineRun", map[string]any{"runId": f.runID, "conclusion": "failure"})
			case "changed recipe":
				*f.source = strings.ReplaceAll(*f.source, "build/image.oci.tar", "other/image.oci.tar")
			case "corrupt journal":
				_, b := f.materialize(t)
				if _, err := f.db.Exec(`UPDATE release_candidate_profile_bindings SET body=jsonb_set(body,'{facts,components,0,version}','"9.9.9"') WHERE binding_id=$1`, b.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := f.i.materializeProfile(ctx, "engine", digest, f.runs); err == nil {
				t.Fatal("invalid profile materialized")
			}
			if len(f.library.pins) != 0 {
				t.Fatal("invalid profile retained artifacts")
			}
		})
	}
}

func TestCandidateProfileBindsPipelineGenerationNotStepAttempt(t *testing.T) {
	f := newProfileDBFixture(t, "release", "full", 2)
	_, b := f.materialize(t)
	plan, err := (candidateEvidenceReader{engine: f.producer}).completeRun(ownerCtx(), f.runs["build"])
	if err != nil || len(plan.Receipts) != 2 || plan.Receipts[0].Attempt != 1 || b.Facts.Components[0].Runs[0].PipelineRunID != f.runID {
		t.Fatal("valid rerun generation with first step attempts was refused", err)
	}
	bad := newProfileDBFixture(t, "release", "full", 2, 1)
	_, _, digest, err := bad.i.candidateProfile(ownerCtx(), "engine")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bad.i.materializeProfile(ownerCtx(), "engine", digest, bad.runs); err == nil {
		t.Fatal("mismatched work generation accepted")
	}
}

func TestCandidateProfileDerivesFileTargetsAndAuxiliarySourceVersion(t *testing.T) {
	f := newProfileDBFixture(t, "release", "full")
	commit := strings.Repeat("b", 40)
	fake := newFakeGitHub(t, nil, commit).withVersionFile("0.25.0\n")
	fake.files["sdk/package.json"] = `{"version":"3.4.5"}`
	f.i.candidateDeps.Load().SourceClient = NewClient().WithBaseURL(fake.server.URL)
	f.config.Profiles[0].VersionFacts = []candidateProfileVersionFact{{Name: "sdk", Component: "engine", Path: "sdk/package.json", JSONField: "version"}}
	f.config.Profiles[0].ReleaseAssets = []candidateProfileFileAuthority{{ID: "files", Component: "engine", Artifact: "docs", APIOrigin: "https://api.github.com", UploadOrigin: "https://uploads.github.com", Repository: "acme/engine", TagPrefix: "v", UseGitHubApp: true}}
	*f.source = strings.Replace(*f.source, `publications: [{targetId: "registry", tag: facts.components.0.version}]`, `publications: [{targetId: "registry", tag: facts.components.0.version}, {targetId: "files", assetName: "docs-" + facts.versions.sdk + ".tgz", draft: {name: "Engine " + facts.components.0.version, body: "Review", prerelease: false, latest: true}}]`, 1)
	*f.source = strings.Replace(*f.source, `kind: "oci", platform: "linux/arm64"}`, `kind: "oci", platform: "linux/arm64"}, {name: "docs", run: "build", stepKey: "build.docs", path: "docs/docs-" + facts.components.0.version + ".tgz", kind: "file", platform: "any"}`, 1)
	p, b := f.materialize(t)
	target := p.targetReader.files["files"]
	fact := b.Facts.Components[0]
	if b.Facts.Versions["sdk"] != "3.4.5" || target.Tag != "v0.25.0" || target.SourceCommit != fact.Commit || target.Repository != "acme/engine" || target.AssetName != "docs-3.4.5.tgz" || target.ReleaseID != 0 || target.Draft == nil {
		t.Fatal("resolved target did not bind native source/version and fixed authority", target, b.Facts)
	}
	fake.mu.Lock()
	reads := append([]string(nil), fake.fileReads...)
	fake.mu.Unlock()
	found := false
	for _, read := range reads {
		if read == "sdk/package.json@"+fact.Commit {
			found = true
		}
	}
	if !found {
		t.Fatal("auxiliary version did not use exact component commit", reads)
	}
	// A resolver may select fixed authority, never introduce another connection.
	broken := b
	broken.Selection.Publications[1].TargetID = "unconfigured"
	storage, err := f.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.i.profilePreparer(ownerCtx(), storage, f.config.Profiles[0], broken); err == nil {
		t.Fatal("unconfigured destination accepted")
	}
}

func TestCandidateProfileRevocationDuringVerificationCannotBecomeReady(t *testing.T) {
	f := newProfileDBFixture(t, "release", "full")
	p, b := f.materialize(t)
	f.library.onOpen = func() { f.config.Profiles = nil }
	if _, err := p.assemble(ownerCtx(), "engine", f.runs); err == nil {
		t.Fatal("revoked profile completed verification")
	}
	var key string
	if err := f.db.QueryRow(`SELECT candidate_id FROM release_candidate_profile_bindings WHERE binding_id=$1`, b.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, f.db, key)
	record, err := p.ledger.get(ownerCtx(), key)
	if err != nil || record.State != "preparing" || record.ApprovalID != "" {
		t.Fatal("revoked candidate became approvable", record, err)
	}
	if f.library.opens != f.library.closes {
		t.Fatal("revocation leaked artifact stream")
	}
}

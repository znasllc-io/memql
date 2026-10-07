package release

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type candidateEvidenceFixture struct {
	modes     []string
	calls     int
	failAfter int
	skips     []candidateSkippedStep
}

func (e *candidateEvidenceFixture) verify(context.Context, pl.ReleaseCandidate) error {
	e.calls++
	if e.failAfter > 0 && e.calls >= e.failAfter {
		return errors.New("receipt changed")
	}
	return nil
}
func (e *candidateEvidenceFixture) coverage(context.Context, pl.ReleaseCandidate) (candidateCoverage, error) {
	return candidateCoverage{Modes: slices.Clone(e.modes), Skipped: slices.Clone(e.skips)}, nil
}

type candidateLibraryFixture struct {
	body          []byte
	t             *testing.T
	mu            sync.Mutex
	receipt       pipelinesteps.StoredFileReceipt
	pins          map[string]bool
	released      map[string]bool
	opens, closes int
	readError     error
	alterReceipt  bool
	onOpen        func()
}

func (l *candidateLibraryFixture) check(ctx context.Context, scope pipelinesteps.RunFileReceiptScope) {
	l.t.Helper()
	owner, err := candidateOwner(ctx)
	if err != nil || owner != scope.OwnerUserID || !auth.OriginFromContext(ctx).IsInternal() {
		l.t.Fatal("artifact authority changed", err)
	}
}
func (l *candidateLibraryFixture) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	l.check(ctx, ref.Scope)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released[ref.ReferenceID] {
		return nil, errors.New("released reference cannot be pinned")
	}
	if ref.Scope != receiptScope(l.receipt) || !slices.Equal(ref.IntentIDs, []string{l.receipt.IntentID}) {
		return nil, errors.New("unexpected pin identity")
	}
	l.pins[ref.ReferenceID] = true
	return []pipelinesteps.StoredFileReceipt{l.receipt}, nil
}
func (l *candidateLibraryFixture) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	l.check(ctx, ref.Scope)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released[ref.ReferenceID] = true
	delete(l.pins, ref.ReferenceID)
	return nil
}
func (l *candidateLibraryFixture) RetireRunFile(context.Context, pipelinesteps.RunFileReceiptScope, string) (pipelinesteps.RetiredFileReceipt, error) {
	l.t.Fatal("candidate retirement must not retire shared artifact bytes")
	return pipelinesteps.RetiredFileReceipt{}, nil
}

type candidateReadFixture struct {
	l *candidateLibraryFixture
	r io.Reader
}

func (r *candidateReadFixture) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if errors.Is(err, io.EOF) && r.l.readError != nil {
		return n, r.l.readError
	}
	return n, err
}
func (r *candidateReadFixture) Close() error {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	r.l.closes++
	return nil
}
func (l *candidateLibraryFixture) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	l.check(ctx, scope)
	l.mu.Lock()
	if len(l.pins) == 0 || scope != receiptScope(l.receipt) || id != l.receipt.IntentID {
		l.mu.Unlock()
		return pipelinesteps.StoredFileReceipt{}, nil, errors.New("read without exact pin")
	}
	l.opens++
	got := l.receipt
	if l.alterReceipt {
		got.ETag = "changed-version"
	}
	l.mu.Unlock()
	if l.onOpen != nil {
		l.onOpen()
	}
	return got, &candidateReadFixture{l: l, r: bytes.NewReader(l.body)}, nil
}

func candidatePreparationFixture(t *testing.T, db *sql.DB) (*candidatePreparer, pl.ReleaseCandidate, *candidateLibraryFixture, *candidateEvidenceFixture) {
	t.Helper()
	c := candidateTestManifest()
	a := &c.Components[0].Artifacts[0]
	body, archiveDigest, imageDigest := candidateOCIBytes(t)
	a.Kind, a.ImageDigest, a.Size, a.Digest = "oci", imageDigest, int64(len(body)), archiveDigest
	r := a.Receipt
	c.Evidence[0].WorkRunID, c.Evidence[0].StepKey, c.Evidence[0].Attempt = r.WorkRunID, r.StepKey, r.Attempt
	c.Evidence[0].ArtifactIntentIDs = []string{r.IntentID}
	library := &candidateLibraryFixture{t: t, body: body, pins: map[string]bool{}, released: map[string]bool{}, receipt: pipelinesteps.StoredFileReceipt{
		IntentID: r.IntentID, OwnerUserID: c.OwnerUserID, WorkRunID: r.WorkRunID, StepKey: r.StepKey, Attempt: r.Attempt,
		ETag: "fixture-v1", Size: a.Size, SHA256: strings.TrimPrefix(a.Digest, "sha256:"),
	}}
	evidence := &candidateEvidenceFixture{modes: []string{"full"}}
	p := &candidatePreparer{ledger: &candidateLedger{db: func() *sql.DB { return db }}, library: library, evidence: evidence,
		engineRevision: func() string { return strings.Repeat("d", 40) },
		versionReader:  &candidateVersionReader{client: NewClient().WithBaseURL(newFakeGitHub(t, nil, c.Components[0].Commit).withVersionFile("0.25.0\n").server.URL), sources: map[string]candidateVersionSource{"engine": {Component: "engine", Repository: "acme/engine", Path: "VERSION"}}},
		targetReader:   &candidateTargetReader{targets: map[string]candidateRegistryTarget{"registry": {ID: "registry", Component: "engine", Artifact: "image", Origin: "https://registry.example.com", Repository: "acme/engine"}}},
	}
	_, _, digest, err := p.targetReader.targets["registry"].snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Destinations[0].TargetDigest = digest
	return p, c, library, evidence
}

func candidatePreparedID(t *testing.T, db *sql.DB, run string) (string, string) {
	t.Helper()
	var key, state string
	err := db.QueryRow(`SELECT candidate_id,state FROM release_candidates WHERE manifest->'components'->0->'artifacts'->0->'receipt'->>'workRunId'=$1`, run).Scan(&key, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return key, state
}

func TestCandidatePreparationAcrossHostsRequiresCompleteVerification(t *testing.T) {
	db := candidateTestDB(t)
	p, candidate, library, evidence := candidatePreparationFixture(t, db)
	record, err := p.prepare(ownerCtx(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, record.ID)
	if record.State != "ready" || record.ApprovalID != "" || record.Manifest.WorkflowDigest == candidate.WorkflowDigest || evidence.calls != 2 || library.opens != 1 || library.closes != 1 {
		t.Fatalf("preparation: %+v, evidence=%d, stream=%d/%d", record, evidence.calls, library.opens, library.closes)
	}
	// Another host has no scope flags, snapshots or preparer's local state.
	other, _, _, _ := candidatePreparationFixture(t, db)
	other.library, other.evidence = library, &candidateEvidenceFixture{modes: []string{"full"}}
	other.versionReader, other.targetReader = p.versionReader, p.targetReader
	repeated, err := other.prepare(ownerCtx(), candidate)
	if err != nil || repeated.ID != record.ID || repeated.State != "ready" {
		t.Fatal(repeated, err)
	}
	approved, err := other.ledger.approve(ownerCtx(), repeated.ID)
	if err != nil || approved.ApprovalID == "" {
		t.Fatal(approved, err)
	}
	retired, err := other.retire(ownerCtx(), repeated.ID)
	if err != nil || retired.State != "retired" || len(library.pins) != 0 || !library.released[record.ID] {
		t.Fatal(retired, err)
	}
	if _, err := p.prepare(ownerCtx(), candidate); err == nil {
		t.Fatal("old preparer resurrected retired candidate")
	}
}

func TestCandidatePreparationFailureNeverReportsReady(t *testing.T) {
	db := candidateTestDB(t)
	for _, failure := range []string{"partial mode", "no modes", "version", "target", "read", "receipt changed", "late evidence", "invalid OCI", "retired while reading"} {
		t.Run(failure, func(t *testing.T) {
			p, c, library, evidence := candidatePreparationFixture(t, db)
			switch failure {
			case "partial mode":
				evidence.modes = []string{"full", "affected"}
			case "no modes":
				evidence.modes = nil
			case "version":
				c.Components[0].Version = "0.25.1"
			case "target":
				target := p.targetReader.targets["registry"]
				target.Repository = "acme/changed"
				p.targetReader.targets["registry"] = target
			case "read":
				library.readError = errors.New("provider checksum failed at EOF")
			case "receipt changed":
				library.alterReceipt = true
			case "late evidence":
				evidence.failAfter = 2
			case "invalid OCI":
				c.Components[0].Artifacts[0].Kind, c.Components[0].Artifacts[0].ImageDigest = "oci", c.Components[0].Artifacts[0].Digest
			case "retired while reading":
				library.onOpen = func() {
					key, _ := candidatePreparedID(t, db, c.Components[0].Artifacts[0].Receipt.WorkRunID)
					if _, err := p.retire(ownerCtx(), key); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := p.prepare(ownerCtx(), c); err == nil {
				t.Fatal("invalid candidate reported ready")
			}
			key, state := candidatePreparedID(t, db, c.Components[0].Artifacts[0].Receipt.WorkRunID)
			if key != "" {
				cleanupCandidate(t, db, key)
				if state != "preparing" && state != "retired" {
					t.Fatal("failed preparation became approvable", state)
				}
				if _, err := p.ledger.approve(ownerCtx(), key); err == nil {
					t.Fatal("failed candidate approved")
				}
			}
			if library.opens != library.closes {
				t.Fatal("artifact stream leaked", library.opens, library.closes)
			}
		})
	}
}

func TestCandidateWorkflowCannotSkipNativeInvariants(t *testing.T) {
	db := candidateTestDB(t)
	base, err := workflowhost.Load(candidatePrepareWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"Evidence", "CheckVersions", "CheckTargets", "RecordPreparing", "PinArtifacts", "VerifyArtifacts"} {
		t.Run(omitted, func(t *testing.T) {
			p, c, library, _ := candidatePreparationFixture(t, db)
			var lines []string
			for _, op := range []string{"Evidence", "CheckVersions", "CheckTargets", "RecordPreparing", "PinArtifacts", "VerifyArtifacts", "MarkReady"} {
				if op != omitted {
					lines = append(lines, "builtin releaseCandidate"+op+"()")
				}
			}
			a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation incompleteCandidate {\n"+strings.Join(lines, "\n")+"\n}", "candidate-test.memql")
			if err != nil {
				t.Fatal(err)
			}
			a.Trusted = base.Trusted // models a different installed recipe, never a client flag
			if _, err := p.prepareWithWorkflow(ownerCtx(), c, a); err == nil {
				t.Fatal("workflow omitted mandatory verification", omitted)
			}
			key, state := candidatePreparedID(t, db, c.Components[0].Artifacts[0].Receipt.WorkRunID)
			if key != "" {
				cleanupCandidate(t, db, key)
				if state != "preparing" {
					t.Fatal(state)
				}
			}
			if library.opens != library.closes {
				t.Fatal("stream leaked")
			}
		})
	}
}

func TestCandidatePreparationOwnerGateBeforePorts(t *testing.T) {
	var p *candidatePreparer
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleDeveloper), actorContext(auth.RoleAdmin)} {
		if _, err := p.prepare(ctx, candidateTestManifest()); err == nil {
			t.Fatal("nonowner reached preparation")
		}
		if _, err := p.retire(ctx, "candidate"); err == nil {
			t.Fatal("nonowner reached retirement")
		}
	}
	for _, capability := range workflowhost.ScopedCapabilities((&candidatePrepareScope{}).operations()) {
		if _, err := capability.Handler(ownerCtx(), map[string]any{"approved": true, "verified": true}, 0); err == nil {
			t.Fatal("serialized authority reached private operation")
		}
	}
}

func TestCandidateWorkflowIdentityBindsPolicySourceRulesAndEngine(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _, _ := candidatePreparationFixture(t, db)
	first, err := p.prepare(ownerCtx(), c)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, first.ID)
	// A second installed recipe composes the same operations in a different
	// valid order. Its change requires a separate review even with equal bytes.
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@template
automation otherCandidateRecipe {
 builtin releaseCandidateCheckTargets()
 builtin releaseCandidateCheckVersions()
 facts := builtin releaseCandidateEvidence()
 if facts.modes.count() == 0 || facts.modes.any(mode => mode != "full") { builtin releaseCandidateRefuse() }
 builtin releaseCandidateRecordPreparing()
 builtin releaseCandidatePinArtifacts()
 builtin releaseCandidateVerifyArtifacts()
 builtin releaseCandidateMarkReady()
}`, "candidate-test.memql")
	if err != nil {
		t.Fatal(err)
	}
	a.Trusted = true
	other, err := p.prepareWithWorkflow(ownerCtx(), c, a)
	if err != nil || other.ID == first.ID || other.Manifest.WorkflowDigest == first.Manifest.WorkflowDigest {
		t.Fatal("changed policy reused approval identity", err)
	}
	cleanupCandidate(t, db, other.ID)
	p.engineRevision = func() string { return strings.Repeat("e", 40) }
	revised, err := p.prepare(ownerCtx(), c)
	if err != nil || revised.ID == first.ID {
		t.Fatal("changed native contract reused approval identity", err)
	}
	cleanupCandidate(t, db, revised.ID)
	p.engineRevision = func() string { return "unknown-dirty" }
	if _, err := p.prepare(ownerCtx(), c); err == nil {
		t.Fatal("mutable engine source produced approvable evidence")
	}
}

func TestCandidateWorkflowChoosesAllowedPlannedSkips(t *testing.T) {
	db := candidateTestDB(t)
	for _, code := range []string{pl.CodeNotAffected, pl.CodePassedEarlier, "pipeline_stage_blocked", "unknown_skip"} {
		t.Run(code, func(t *testing.T) {
			p, c, library, evidence := candidatePreparationFixture(t, db)
			evidence.skips = []candidateSkippedStep{{Component: "engine", WorkRunID: c.Evidence[0].WorkRunID, StepKey: "test.empty", Code: code}}
			record, err := p.prepare(ownerCtx(), c)
			if code == pl.CodeNotAffected {
				if err != nil || record.State != "ready" {
					t.Fatal("declared empty selection did not qualify", err)
				}
				cleanupCandidate(t, db, record.ID)
			} else {
				if err == nil {
					t.Fatal("unproven skip satisfied release policy", code)
				}
				if len(library.pins) != 0 || library.opens != 0 {
					t.Fatal("excluded evidence reached artifacts")
				}
			}
		})
	}
}

package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/buildinfo"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

const candidatePrepareWorkflow = "releasePrepareCandidateWorkflow"
const candidatePublishWorkflow = "releasePublishCandidateWorkflow"

type candidateEvidence interface {
	verify(context.Context, pl.ReleaseCandidate) error
	coverage(context.Context, pl.ReleaseCandidate) (candidateCoverage, error)
}

// Private native ports. No client or DSL argument supplies validators, storage
// handles or authority flags. The configured source/target adapters must prove
// exact values; absence refuses before any durable preparation or artifact pin.
type candidatePreparer struct {
	ledger   *candidateLedger
	evidence candidateEvidence
	library  interface {
		pipelinesteps.LibraryArtifactLifecycle
		pipelinesteps.LibraryReceiptOpener
	}
	versionReader  *candidateVersionReader
	targetReader   *candidateTargetReader
	engineRevision func() string
	logger         *slog.Logger
}

// prepare is intentionally not a public capability yet. The installed recipe
// orders separate effects; the native scope makes readiness impossible until
// this exact immutable candidate passed every integrity gate. Approval remains
// a separate owner request and publication is not available in this scope.
func (p *candidatePreparer) prepare(ctx context.Context, input pl.ReleaseCandidate) (candidateRecord, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateRecord{}, err
	}
	a, err := workflowhost.Load(candidatePrepareWorkflow)
	if err != nil {
		return candidateRecord{}, err
	}
	return p.prepareWithWorkflow(ctx, input, a)
}

func (p *candidatePreparer) prepareWithWorkflow(ctx context.Context, input pl.ReleaseCandidate, definition *automations.Automation) (candidateRecord, error) {
	return p.prepareWithExpectedIdentity(ctx, input, definition, "", nil)
}

func (p *candidatePreparer) prepareWithExpectedIdentity(ctx context.Context, input pl.ReleaseCandidate, definition *automations.Automation, expectedID string, publication *automations.Automation) (candidateRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateRecord{}, err
	}
	if input.OwnerUserID != owner {
		return candidateRecord{}, errors.New("release candidate belongs to another owner")
	}
	if p == nil || p.ledger == nil || p.evidence == nil || p.library == nil || p.versionReader == nil || p.targetReader == nil {
		return candidateRecord{}, errors.New("release candidate preparation is not configured")
	}
	if definition == nil || !definition.Trusted {
		return candidateRecord{}, errors.New("release candidate workflow must come from the installed DSL")
	}
	owned, err := automations.NewLoader(automations.LoaderOptions{Logger: p.logger}).Snapshot(definition)
	if err != nil {
		return candidateRecord{}, err
	}
	if publication == nil {
		publication, err = workflowhost.Load(candidatePublishWorkflow)
		if err != nil {
			return candidateRecord{}, err
		}
	}
	publication, err = automations.NewLoader(automations.LoaderOptions{Logger: p.logger}).Snapshot(publication)
	if err != nil || !publication.Trusted {
		return candidateRecord{}, errors.New("candidate publication recipe must be installed and immutable")
	}
	versions, sources, err := p.versionReader.freezeFor(input.Components)
	if err != nil {
		return candidateRecord{}, err
	}
	revision := buildinfo.Commit()
	if p.engineRevision != nil {
		revision = p.engineRevision()
	}
	if !candidateSourceCommit(revision) {
		return candidateRecord{}, errors.New("release preparation requires an immutable engine revision")
	}
	// The fingerprint binds the exact owned DSL body and the native contract.
	// Child templates/logics are refused below until their snapshots also enter
	// this identity; a reload cannot change execution under the same digest.
	fingerprint, err := workjournal.DefinitionFingerprint("release-candidate-preparation-v1", []workjournal.StepDecl{{
		Key: "prepare", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"workflow": owned, "publicationWorkflow": publication, "versionSources": sources, "engineRevision": revision},
	}})
	if err != nil {
		return candidateRecord{}, err
	}
	input.WorkflowDigest = "sha256:" + strings.TrimPrefix(fingerprint, "work-definition-v2:")
	body, key, err := pl.CanonicalReleaseCandidate(input)
	if err != nil {
		return candidateRecord{}, err
	}
	if expectedID != "" && key != expectedID {
		return candidateRecord{}, errors.New("candidate policy, engine or source configuration changed; prepare a new candidate for review")
	}
	var candidate pl.ReleaseCandidate
	if err := json.Unmarshal(body, &candidate); err != nil {
		return candidateRecord{}, err
	}
	scope := &candidatePrepareScope{p: p, candidate: candidate, key: key, versionReader: versions}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	_, err = workflowhost.Run(ctx, owned.Name, nil, workflowhost.Options{
		Logger: p.logger, Operations: scope.operations(),
		Load: func(name string) (*automations.Automation, error) {
			if name != owned.Name {
				return nil, errors.New("candidate workflow cannot load an unbound child")
			}
			return owned, nil
		},
		LoadLogic: func(string) (*memql.Function, error) {
			return nil, errors.New("candidate workflow cannot load unbound logic")
		},
	})
	if err != nil {
		return candidateRecord{}, err
	}
	if scope.out.ID != key || (scope.out.State != "ready" && scope.out.State != "approved") {
		return candidateRecord{}, errors.New("release workflow did not complete candidate verification")
	}
	return scope.out, nil
}

type candidatePrepareScope struct {
	versionReader                                                candidateVersionReader
	p                                                            *candidatePreparer
	candidate                                                    pl.ReleaseCandidate
	key                                                          string
	out                                                          candidateRecord
	evidenceOK, versionsOK, targetsOK, recorded, pinned, bytesOK bool
	receipts                                                     map[string]pipelinesteps.StoredFileReceipt
}

func (s *candidatePrepareScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"releaseCandidateEvidence": s.inspectEvidence,
		"releaseCandidateRefuse": func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("candidate does not satisfy the installed release policy")
		},
		"releaseCandidateCheckVersions":   s.versions,
		"releaseCandidateCheckTargets":    s.targets,
		"releaseCandidateRecordPreparing": s.record,
		"releaseCandidatePinArtifacts":    s.pin,
		"releaseCandidateVerifyArtifacts": s.verifyBytes,
		"releaseCandidateMarkReady":       s.ready,
	}
}

func (s *candidatePrepareScope) inspectEvidence(ctx context.Context, _ map[string]any) (any, error) {
	s.evidenceOK = false
	if err := s.p.evidence.verify(ctx, s.candidate); err != nil {
		return nil, err
	}
	facts, err := s.p.evidence.coverage(ctx, s.candidate)
	if err != nil {
		return nil, err
	}
	s.evidenceOK = true
	// Convert to an ordinary value object for the DSL evaluator.
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *candidatePrepareScope) versions(ctx context.Context, _ map[string]any) (any, error) {
	s.versionsOK = false
	for _, component := range s.candidate.Components {
		if err := s.versionReader.check(ctx, component); err != nil {
			return nil, fmt.Errorf("component %s: %w", component.Name, err)
		}
	}
	s.versionsOK = true
	return nil, nil
}

func (s *candidatePrepareScope) targets(ctx context.Context, _ map[string]any) (any, error) {
	s.targetsOK = false
	for _, target := range s.candidate.Destinations {
		for _, component := range s.candidate.Components {
			if component.Name != target.Component {
				continue
			}
			for _, artifact := range component.Artifacts {
				if artifact.Name == target.Artifact && artifact.Kind != "oci" {
					return nil, errors.New("registry destination requires an OCI image artifact")
				}
			}
		}
		if err := s.p.targetReader.check(ctx, target); err != nil {
			return nil, fmt.Errorf("target %s: %w", target.TargetID, err)
		}
	}
	s.targetsOK = true
	return nil, nil
}

func (s *candidatePrepareScope) record(ctx context.Context, _ map[string]any) (any, error) {
	if !s.evidenceOK || !s.versionsOK || !s.targetsOK {
		return nil, errors.New("candidate intent requires verified evidence, versions and destinations")
	}
	rec, err := s.p.ledger.prepare(ctx, s.candidate)
	if err != nil {
		return nil, err
	}
	if rec.ID != s.key {
		return nil, errors.New("candidate identity changed while preparing")
	}
	s.recorded = true
	return nil, nil
}

func (s *candidatePrepareScope) ready(ctx context.Context, _ map[string]any) (any, error) {
	if !s.evidenceOK || !s.versionsOK || !s.targetsOK || !s.recorded || !s.pinned || !s.bytesOK {
		return nil, errors.New("candidate is missing verified and pinned release inputs")
	}
	// Close the preparation interval with fresh journal and target reads. The
	// ledger then atomically refuses any concurrent retirement. Publication will
	// repeat these checks, since later edits cannot be locked by a read here.
	if _, err := s.inspectEvidence(ctx, nil); err != nil {
		return nil, err
	}
	if _, err := s.targets(ctx, nil); err != nil {
		return nil, err
	}
	out, err := s.p.ledger.ready(ctx, s.key)
	if err != nil {
		return nil, err
	}
	s.out = out
	return nil, nil
}

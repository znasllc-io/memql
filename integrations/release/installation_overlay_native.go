package release

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

type overlayCandidateIdentity struct {
	ID             string `json:"id"`
	ApprovalID     string `json:"approvalId"`
	WorkflowDigest string `json:"workflowDigest"`
	CatalogDigest  string `json:"catalogDigest"`
}

type overlayRunEvidence struct {
	PipelineID    string `json:"pipelineId"`
	PipelineRunID string `json:"pipelineRunId"`
	WorkRunID     string `json:"workRunId"`
	Mode          string `json:"mode"`
	StepsPassed   int    `json:"stepsPassed"`
	StepsSkipped  int    `json:"stepsSkipped"`
	ReceiptDigest string `json:"receiptDigest"`
}

type overlayPublicationEvidence struct {
	EffectID     string `json:"effectId"`
	State        string `json:"state"`
	TargetID     string `json:"targetId"`
	TargetDigest string `json:"targetDigest"`
	Origin       string `json:"origin"`
	Repository   string `json:"repository"`
	Tag          string `json:"tag"`
}

type overlayImageEvidence struct {
	Digest           string                     `json:"digest"`
	InstanceRegistry string                     `json:"instanceRegistry"`
	ArchiveDigest    string                     `json:"archiveDigest"`
	ArchiveSize      int64                      `json:"archiveSize"`
	ArtifactReceipt  pl.ReleaseReceiptReference `json:"artifactReceipt"`
	Publication      overlayPublicationEvidence `json:"publication"`
}

// This is private same-owner evidence, not a widening of the public catalog.
// It is held in the same proposal record as its replacement bytes. A replay
// revalidates these exact identities rather than choosing a newer companion.
type overlayCandidateEvidence struct {
	Candidate  overlayCandidateIdentity        `json:"candidate"`
	Component  string                          `json:"component"`
	Version    string                          `json:"version"`
	Repository string                          `json:"repository"`
	Commit     string                          `json:"commit"`
	Platform   string                          `json:"platform,omitempty"`
	Runs       []overlayRunEvidence            `json:"runs"`
	Images     map[string]overlayImageEvidence `json:"images,omitempty"`
	File       *pl.PublishedArtifact           `json:"file,omitempty"`
}

type overlayNativeEvidence struct {
	Primary   overlayCandidateEvidence `json:"primary"`
	Companion overlayCandidateEvidence `json:"companion"`
}

func (s *overlayScope) nativeCandidate(ctx context.Context, key, digest, component string, companion bool) (overlayCandidateEvidence, error) {
	var out overlayCandidateEvidence
	if err := s.gate(ctx, nil); err != nil {
		return out, err
	}
	owner, err := candidateOwner(ctx)
	if err != nil || owner != s.owner || s.integration.store == nil || s.integration.store.engine == nil {
		return out, errors.New("native overlay requires same-owner private journal access")
	}
	if s.configuration.FormatVersion != 3 || s.configuration.NativeEvidence == nil {
		return out, errors.New("native overlay evidence requires an explicit v3 scope")
	}
	_, cfg, trust, err := s.integration.catalog(ctx)
	if err != nil {
		return out, err
	}
	_, verified, err := readCatalog(ctx, s.db, key, cfg.Publisher, trust)
	if err != nil || verified.Digest() != digest {
		return out, errors.New("native overlay requires the exact signed catalog")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, s.owner, key)
	if err != nil {
		return out, errors.New("native overlay private evidence is unavailable to this owner")
	}
	rec, err = readCandidateApproval(ctx, tx, s.owner, rec)
	if err != nil || rec.ApprovalID == "" {
		return out, errors.New("native overlay requires the original owner approval")
	}
	projection, err := publicCandidate(ctx, tx, s.owner, rec, cfg.Publisher)
	if err != nil {
		return out, err
	}
	_, actual, err := pl.CanonicalPublishedRelease(projection)
	if err != nil || actual != digest {
		return out, errors.New("native publication history differs from its signed catalog")
	}
	selected, err := overlayPublishedComponent(verified, component)
	if err != nil {
		return out, err
	}
	out = overlayCandidateEvidence{Candidate: overlayCandidateIdentity{key, rec.ApprovalID, rec.Manifest.WorkflowDigest, digest}, Component: component, Version: selected.Version, Repository: selected.Repository, Commit: selected.Commit}
	if companion {
		for _, artifact := range selected.Artifacts {
			if artifact.Name == s.configuration.NativeEvidence.CompanionArtifact {
				if artifact.Kind != "file" || len(artifact.Locations) != 1 || artifact.Locations[0].Kind != "github-release" || artifact.Locations[0].Repository != selected.Repository || artifact.Locations[0].Tag != "v"+selected.Version {
					return out, errors.New("overlay companion must be one published file at the selected source tag")
				}
				copy := artifact
				out.File = &copy
			}
		}
		if out.File == nil {
			return out, errors.New("overlay companion artifact is absent")
		}
	} else {
		out.Images = map[string]overlayImageEvidence{}
		for _, binding := range s.configuration.NativeEvidence.Images {
			var found bool
			for _, destination := range rec.Manifest.Destinations {
				if destination.Component != component || destination.Artifact != binding.Artifact {
					continue
				}
				target, proof, err := readPublicationTarget(ctx, tx, rec.ID, rec.ApprovalID, destination)
				if err != nil {
					return out, err
				}
				if target.Registry == nil || strings.TrimPrefix(target.Registry.Origin, "https://")+"/"+target.Registry.Repository != binding.Repository {
					continue
				}
				if found {
					return out, errors.New("native image has ambiguous publication destinations")
				}
				pub, err := candidatePublicationFor(rec, destination.TargetID, component, binding.Artifact)
				if err != nil || pub.Artifact.Kind != "oci" {
					return out, errors.New("native image artifact is unavailable")
				}
				var effectID, state string
				err = tx.QueryRowContext(ctx, `SELECT effect_id,state FROM release_publication_intents WHERE candidate_id=$1 AND approval_id=$2 AND target_id=$3 AND component_name=$4 AND artifact_name=$5`, key, rec.ApprovalID, destination.TargetID, component, binding.Artifact).Scan(&effectID, &state)
				if err != nil || effectID == "" || state != "complete" {
					return out, errors.New("native image has no completed effect identity")
				}
				if out.Platform != "" && out.Platform != pub.Artifact.Platform {
					return out, errors.New("native manifest requires one explicitly selected image platform")
				}
				out.Platform = pub.Artifact.Platform
				out.Images[binding.Name] = overlayImageEvidence{Digest: proof.ImageDigest, InstanceRegistry: binding.Repository + "@" + proof.ImageDigest, ArchiveDigest: proof.ArchiveDigest, ArchiveSize: pub.Artifact.Size, ArtifactReceipt: pub.Artifact.Receipt, Publication: overlayPublicationEvidence{effectID, state, destination.TargetID, destination.TargetDigest, target.Registry.Origin, target.Registry.Repository, target.Registry.Tag}}
				found = true
			}
			if !found {
				return out, errors.New("native manifest image is outside the exact published destination")
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	reader := candidateEvidenceReader{engine: s.integration.store.engine}
	if err := reader.verify(ctx, rec.Manifest); err != nil {
		return out, err
	}
	if _, err := reader.coverage(ctx, rec.Manifest); err != nil {
		return out, err
	}
	runs := []string{}
	for _, evidence := range rec.Manifest.Evidence {
		if evidence.Component == component && !slices.Contains(runs, evidence.WorkRunID) {
			runs = append(runs, evidence.WorkRunID)
		}
	}
	slices.Sort(runs)
	for _, runID := range runs {
		plan, err := reader.completeRun(ctx, runID)
		if err != nil {
			return out, err
		}
		rows, err := reader.rows(ctx, "workRunForOwner", map[string]any{"runId": runID}, 1)
		if err != nil || len(rows) != 1 {
			return out, errors.New("native overlay execution is unavailable")
		}
		var run struct {
			ID, OwnerUserID, Status string
			Input                   struct{ PipelineID, PipelineRunID, Repository, SHA, Mode string }
		}
		body, err := json.Marshal(rows[0])
		if err != nil || json.Unmarshal(body, &run) != nil || run.ID != runID || run.OwnerUserID != s.owner || run.Status != "succeeded" || run.Input.Repository != selected.Repository || run.Input.SHA != selected.Commit || run.Input.Mode != plan.Mode || run.Input.PipelineID == "" || run.Input.PipelineRunID == "" {
			return out, errors.New("native overlay execution source changed")
		}
		item := overlayRunEvidence{PipelineID: memql.BareShortId(run.Input.PipelineID), PipelineRunID: memql.BareShortId(run.Input.PipelineRunID), WorkRunID: runID, Mode: plan.Mode, ReceiptDigest: "sha256:" + overlayHash(plan.Receipts)}
		for _, proof := range plan.Receipts {
			if proof.Status == "done" {
				item.StepsPassed++
			} else {
				item.StepsSkipped++
			}
		}
		out.Runs = append(out.Runs, item)
	}
	if len(out.Runs) == 0 {
		return out, errors.New("native overlay has no complete execution evidence")
	}
	return out, nil
}

func overlayCompanionMatches(primary, companion overlayCandidateEvidence) bool {
	// A sealed mixed release may supply both roles. The same candidate must
	// retain one exact approval/workflow/catalog identity across both reads.
	identity := primary.Candidate.ID != companion.Candidate.ID || primary.Candidate == companion.Candidate
	return identity && primary.Repository == companion.Repository && primary.Version == companion.Version && primary.Commit == companion.Commit && companion.File != nil
}

func (s *overlayScope) nativeEvidence(ctx context.Context, args map[string]any) (any, error) {
	if err := s.gate(ctx, args); err != nil {
		return nil, err
	}
	if s.configuration.FormatVersion != 3 || s.source == nil {
		return nil, errors.New("native overlay facts require the verified receiving publication")
	}
	binding := s.configuration.NativeEvidence
	primary, err := s.nativeCandidate(ctx, s.request.CandidateID, s.request.CatalogDigest, binding.Component, false)
	if err != nil {
		return nil, err
	}
	if s.record != nil && (s.record.NativeEvidence == nil || overlayHash(primary) != overlayHash(s.record.NativeEvidence.Primary)) {
		return nil, errors.New("native overlay primary evidence changed")
	}
	s.nativePrimary = &primary
	s.nativeCompanions = map[string]overlayCandidateEvidence{}
	type choice struct {
		CandidateID   string               `json:"candidateId"`
		CatalogDigest string               `json:"catalogDigest"`
		Runs          []overlayRunEvidence `json:"runs"`
	}
	choices := []choice{}
	keys := [][2]string{}
	if s.record != nil {
		bound := s.record.NativeEvidence.Companion.Candidate
		keys = append(keys, [2]string{bound.ID, bound.CatalogDigest})
	} else {
		// Narrow owner/source/artifact predicates precede projection. This is a
		// bounded inventory, never "latest wins"; the installed DSL chooses.
		rows, err := s.db.QueryContext(ctx, `SELECT c.candidate_id,k.catalog_digest FROM release_candidates c JOIN release_catalog k ON k.candidate_id=c.candidate_id WHERE c.owner_user_id=$1 AND EXISTS (SELECT 1 FROM jsonb_array_elements(c.manifest->'components') AS component WHERE component->>'name'=$2 AND component->>'repository'=$3 AND component->>'version'=$4 AND component->>'commit'=$5 AND EXISTS (SELECT 1 FROM jsonb_array_elements(component->'artifacts') AS artifact WHERE artifact->>'name'=$6 AND artifact->>'kind'='file')) ORDER BY c.candidate_id LIMIT 21`, s.owner, binding.CompanionComponent, primary.Repository, primary.Version, primary.Commit, binding.CompanionArtifact)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, digest string
			if err := rows.Scan(&key, &digest); err != nil {
				rows.Close()
				return nil, err
			}
			keys = append(keys, [2]string{key, digest})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(keys) > 20 {
			return nil, errors.New("native overlay companion inventory exceeds its bound")
		}
	}
	for _, key := range keys {
		companion, err := s.nativeCandidate(ctx, key[0], key[1], binding.CompanionComponent, true)
		if err != nil {
			return nil, err
		}
		if !overlayCompanionMatches(primary, companion) {
			return nil, errors.New("native overlay companion source differs")
		}
		if s.record != nil && overlayHash(companion) != overlayHash(s.record.NativeEvidence.Companion) {
			return nil, errors.New("native overlay companion evidence changed")
		}
		s.nativeCompanions[key[0]] = companion
		choices = append(choices, choice{key[0], key[1], companion.Runs})
	}
	return map[string]any{"runs": primary.Runs, "companions": choices}, nil
}

func (s *overlayScope) bindNativePublication(ctx context.Context, args map[string]any) (any, error) {
	if err := s.gate(ctx, nil); err != nil {
		return nil, err
	}
	if len(args) != 2 || s.nativePrimary == nil {
		return nil, errors.New("native overlay binding requires prior verified facts")
	}
	key, digest := asString(args["candidateId"]), asString(args["catalogDigest"])
	companion, ok := s.nativeCompanions[key]
	if !ok || companion.Candidate.CatalogDigest != digest || !overlayCompanionMatches(*s.nativePrimary, companion) {
		return nil, errors.New("native overlay selection is outside the verified companion inventory")
	}
	bound := overlayNativeEvidence{*s.nativePrimary, companion}
	if s.nativeBound != nil && overlayHash(*s.nativeBound) != overlayHash(bound) {
		return nil, errors.New("native overlay companion was already selected")
	}
	if s.record != nil && overlayHash(s.record.NativeEvidence) != overlayHash(&bound) {
		return nil, errors.New("native overlay selection differs from its durable intent")
	}
	s.nativeBound = &bound
	return nil, nil
}

package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

const candidateAssembleWorkflow = "releaseAssembleCandidateWorkflow"

func (i *Integration) handleCandidateAssemble(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.configuredCandidate(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(args["runs"])
	if err != nil {
		return nil, errors.New("assembly runs must be an object of run identities")
	}
	var runs map[string]string
	if err := decodeCandidateObject(body, &runs); err != nil {
		return nil, err
	}
	record, err := p.assemble(ctx, asString(args["planName"]), runs)
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}

func (p *candidatePreparer) assemble(ctx context.Context, name string, runs map[string]string) (candidateRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateRecord{}, err
	}
	if p == nil || p.versionReader == nil || p.targetReader == nil {
		return candidateRecord{}, errors.New("release assembly is unavailable")
	}
	reader, ok := p.evidence.(candidateEvidenceReader)
	library, supportsReceipts := p.library.(pipelinesteps.LibraryReceiptReader)
	if !ok || !supportsReceipts {
		return candidateRecord{}, errors.New("release assembly requires native journal and artifact receipt readers")
	}
	if err := validateAssemblyPlans(p.assemblyPlans, p.versionReader, p.targetReader); err != nil {
		return candidateRecord{}, err
	}
	var plan candidateAssemblyPlan
	for _, configured := range p.assemblyPlans {
		if configured.Name == name {
			plan = configured
		}
	}
	if plan.Name == "" {
		return candidateRecord{}, errors.New("release assembly plan is not configured")
	}
	ownedRuns := map[string]string{}
	for _, component := range plan.Components {
		seen := map[string]bool{}
		for _, alias := range component.Runs {
			id := memql.BareShortId(runs[alias])
			if id == "" || len(id) > 512 || strings.IndexFunc(id, func(r rune) bool { return r < 33 || r > 126 }) >= 0 || seen[id] {
				return candidateRecord{}, errors.New("assembly requires one distinct completed run for each declared component input")
			}
			ownedRuns[alias], seen[id] = id, true
		}
	}
	if len(ownedRuns) != len(runs) {
		return candidateRecord{}, errors.New("assembly contains undeclared run inputs")
	}
	definition, err := workflowhost.Load(candidateAssembleWorkflow)
	if err != nil {
		return candidateRecord{}, err
	}
	definition, err = automations.NewLoader(automations.LoaderOptions{Logger: p.logger}).Snapshot(definition)
	if err != nil || definition == nil || !definition.Trusted {
		return candidateRecord{}, errors.New("release assembly requires an immutable installed workflow")
	}
	bound := *p
	bound.assemblyWorkflow = definition
	s := &candidateAssemblyScope{p: &bound, owner: owner, plan: plan, runs: ownedRuns, reader: reader, library: library,
		resolvedRuns: map[string]candidateRunPlan{}, versions: map[string]pl.ReleaseComponent{}, artifacts: map[string]pl.ReleaseArtifact{}, targets: map[string]pl.ReleaseDestination{}}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	_, err = workflowhost.Run(ctx, definition.Name, nil, workflowhost.Options{Logger: p.logger, Operations: s.operations(),
		Load: func(name string) (*automations.Automation, error) {
			if name != definition.Name {
				return nil, errors.New("assembly cannot load an unbound child")
			}
			return definition, nil
		}, LoadLogic: func(string) (*memql.Function, error) { return nil, errors.New("assembly cannot load unbound logic") },
	})
	if err != nil {
		return candidateRecord{}, err
	}
	if s.out.ID == "" || (s.out.State != "ready" && s.out.State != "approved") {
		return candidateRecord{}, errors.New("assembly did not finish candidate preparation")
	}
	return s.out, nil
}

type candidateAssemblyScope struct {
	p            *candidatePreparer
	owner        string
	plan         candidateAssemblyPlan
	runs         map[string]string
	reader       candidateEvidenceReader
	library      pipelinesteps.LibraryReceiptReader
	resolvedRuns map[string]candidateRunPlan
	versions     map[string]pl.ReleaseComponent
	artifacts    map[string]pl.ReleaseArtifact
	targets      map[string]pl.ReleaseDestination
	out          candidateRecord
}

func assemblyValue(value any) (any, error) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("assembly facts exceed their encoding bound")
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *candidateAssemblyScope) operations() map[string]workflowhost.Operation {
	ops := map[string]workflowhost.Operation{
		"releaseAssemblyPlan":         func(context.Context, map[string]any) (any, error) { return assemblyValue(s.plan) },
		"releaseAssemblyReadRun":      s.readRun,
		"releaseAssemblyReadVersion":  s.readVersion,
		"releaseAssemblyReadArtifact": s.readArtifact,
		"releaseAssemblyReadTarget":   s.readTarget,
		"releaseAssemblyFacts":        s.facts,
		"releaseAssemblyPrepare":      s.prepare,
	}
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			owner, err := candidateOwner(ctx)
			if err != nil || owner != s.owner {
				return nil, errors.New("assembly operation requires its original owner")
			}
			return operation(ctx, args)
		}
	}
	return ops
}

func (s *candidateAssemblyScope) component(name string) (candidateAssemblyComponent, error) {
	for _, component := range s.plan.Components {
		if component.Name == name {
			return component, nil
		}
	}
	return candidateAssemblyComponent{}, errors.New("component is outside the declared assembly")
}

func (s *candidateAssemblyScope) readRun(ctx context.Context, args map[string]any) (any, error) {
	component, err := s.component(asString(args["componentName"]))
	if err != nil {
		return nil, err
	}
	alias := asString(args["runName"])
	if !slices.Contains(component.Runs, alias) {
		return nil, errors.New("run input is outside its declared component")
	}
	if _, ok := s.resolvedRuns[alias]; ok {
		return nil, nil
	}
	plan, err := s.reader.completeRun(ctx, s.runs[alias])
	if err != nil {
		return nil, err
	}
	if len(plan.Receipts) == 0 {
		return nil, errors.New("assembly run has no complete evidence")
	}
	first := plan.Receipts[0]
	if first.Repository != s.p.versionReader.sources[component.Name].Repository {
		return nil, errors.New("assembly run belongs to a different source repository")
	}
	count := len(plan.Receipts)
	for otherAlias, other := range s.resolvedRuns {
		count += len(other.Receipts)
		if slices.Contains(component.Runs, otherAlias) && other.Receipts[0].Commit != first.Commit {
			return nil, errors.New("component run inputs must name exactly the same source commit")
		}
	}
	if count > 1024 {
		return nil, errors.New("assembly complete evidence exceeds 1024 steps")
	}
	s.resolvedRuns[alias] = plan
	return nil, nil
}

func (s *candidateAssemblyScope) readVersion(ctx context.Context, args map[string]any) (any, error) {
	component, err := s.component(asString(args["componentName"]))
	if err != nil {
		return nil, err
	}
	for _, alias := range component.Runs {
		if _, ok := s.resolvedRuns[alias]; !ok {
			return nil, errors.New("component version requires every declared run")
		}
	}
	proof := s.resolvedRuns[component.Runs[0]].Receipts[0]
	value := pl.ReleaseComponent{Name: component.Name, Repository: proof.Repository, Commit: proof.Commit}
	value.Version, err = s.p.versionReader.readVersion(ctx, value)
	if err != nil {
		return nil, err
	}
	s.versions[component.Name] = value
	return nil, nil
}

func (s *candidateAssemblyScope) readArtifact(ctx context.Context, args map[string]any) (any, error) {
	component, err := s.component(asString(args["componentName"]))
	if err != nil {
		return nil, err
	}
	var selected candidateAssemblyArtifact
	for _, artifact := range component.Artifacts {
		if artifact.Name == asString(args["artifactName"]) {
			selected = artifact
		}
	}
	if selected.Name == "" {
		return nil, errors.New("artifact is outside the declared component")
	}
	var proof pl.ReleaseWorkReceipt
	for _, receipt := range s.resolvedRuns[selected.Run].Receipts {
		if receipt.StepKey == selected.StepKey {
			proof = receipt
		}
	}
	if proof.StepKind != string(pl.StepCommand) || proof.Status != "done" {
		return nil, errors.New("assembly artifact requires its declared successful producer")
	}
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: s.owner, WorkRunID: proof.WorkRunID, StepKey: proof.StepKey, Attempt: proof.Attempt}
	rows, err := s.library.ReadRunFileReceipts(auth.ContextWithInternalOrigin(ctx), scope, proof.ArtifactIntentIDs)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(proof.ArtifactIntentIDs) {
		return nil, errors.New("artifact discovery returned incomplete receipts")
	}
	byPath, ids := map[string]pipelinesteps.StoredFileReceipt{}, map[string]bool{}
	for _, row := range rows {
		if receiptScope(row) != scope || !slices.Contains(proof.ArtifactIntentIDs, row.IntentID) || ids[row.IntentID] || byPath[row.Path].IntentID != "" ||
			!assemblyArtifactPath(row.Path) || row.ETag == "" || row.Size < 0 || row.Size > 2<<30 || !candidateArtifactDigest(row.SHA256) {
			return nil, errors.New("artifact discovery returned ambiguous or inconsistent receipts")
		}
		byPath[row.Path], ids[row.IntentID] = row, true
	}
	row := byPath[selected.Path]
	if row.IntentID == "" {
		return nil, errors.New("declared artifact path is absent from its producing execution")
	}
	artifact := pl.ReleaseArtifact{Name: selected.Name, Kind: selected.Kind, Platform: selected.Platform, Size: row.Size, Digest: "sha256:" + row.SHA256,
		Receipt: pl.ReleaseReceiptReference{WorkRunID: proof.WorkRunID, StepKey: proof.StepKey, Attempt: proof.Attempt, IntentID: row.IntentID, ReceiptDigest: proof.ReceiptDigest, DefinitionDigest: proof.DefinitionDigest}}
	if selected.Kind == "oci" {
		metadata := byPath[selected.ImageMetadataPath]
		if metadata.IntentID == "" {
			return nil, errors.New("OCI metadata is absent from the same producing execution")
		}
		artifact.ImageDigest, err = s.imageDigest(ctx, metadata)
		if err != nil {
			return nil, err
		}
	}
	s.artifacts[component.Name+"/"+selected.Name] = artifact
	return nil, nil
}

func (s *candidateAssemblyScope) readTarget(_ context.Context, args map[string]any) (any, error) {
	name := asString(args["targetId"])
	if !slices.Contains(s.plan.Targets, name) {
		return nil, errors.New("target is outside the declared assembly")
	}
	var target pl.ReleaseDestination
	if registry, ok := s.p.targetReader.targets[name]; ok {
		_, _, digest, err := registry.snapshot()
		if err != nil {
			return nil, err
		}
		target = pl.ReleaseDestination{TargetID: name, TargetDigest: digest, Component: registry.Component, Artifact: registry.Artifact, Operation: "publish"}
	} else if file, ok := s.p.targetReader.files[name]; ok {
		_, _, digest, err := file.snapshot()
		if err != nil {
			return nil, err
		}
		target = pl.ReleaseDestination{TargetID: name, TargetDigest: digest, Component: file.Component, Artifact: file.Artifact, Operation: "publish"}
	} else {
		return nil, errors.New("assembly target is unavailable")
	}
	s.targets[name] = target
	return nil, nil
}

func assemblyEvidenceName(component, alias, step string) string {
	identity := component + "/" + alias + "/" + step
	fingerprint := id.NewUntracked().FromString("release-assembly-evidence-v1:" + identity)
	prefix := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.ToLower(identity))
	if len(prefix) > 63 {
		prefix = prefix[:63]
	}
	return prefix + "-" + string(fingerprint)
}

// Complete scoped facts are the authority against which the DSL's projection
// is checked. Reordering is legal; dropping/substituting a declared input is not.
func (s *candidateAssemblyScope) resolved() (pl.ReleaseCandidate, error) {
	out := pl.ReleaseCandidate{FormatVersion: 1, OwnerUserID: s.owner, WorkflowDigest: "sha256:" + strings.Repeat("0", 64), Compatibility: append([]pl.ReleaseCompatibility{}, s.plan.Compatibility...)}
	for _, declaration := range s.plan.Components {
		component, ok := s.versions[declaration.Name]
		if !ok {
			return out, errors.New("assembly component version has not been resolved")
		}
		for _, selected := range declaration.Artifacts {
			artifact, ok := s.artifacts[declaration.Name+"/"+selected.Name]
			if !ok {
				return out, errors.New("assembly artifact has not been resolved")
			}
			component.Artifacts = append(component.Artifacts, artifact)
		}
		out.Components = append(out.Components, component)
		for _, alias := range declaration.Runs {
			plan, ok := s.resolvedRuns[alias]
			if !ok {
				return out, errors.New("assembly run has not been resolved")
			}
			for _, receipt := range plan.Receipts {
				out.Evidence = append(out.Evidence, pl.ReleaseEvidence{Name: assemblyEvidenceName(declaration.Name, alias, receipt.StepKey), Component: declaration.Name,
					WorkRunID: receipt.WorkRunID, StepKey: receipt.StepKey, Attempt: receipt.Attempt, ReceiptID: receipt.ReceiptID, ReceiptDigest: receipt.ReceiptDigest,
					DefinitionDigest: receipt.DefinitionDigest, ArtifactIntentIDs: slices.Clone(receipt.ArtifactIntentIDs)})
			}
		}
	}
	for _, name := range s.plan.Targets {
		target, ok := s.targets[name]
		if !ok {
			return out, errors.New("assembly target has not been resolved")
		}
		out.Destinations = append(out.Destinations, target)
	}
	return out, nil
}

func (s *candidateAssemblyScope) facts(_ context.Context, _ map[string]any) (any, error) {
	resolved, err := s.resolved()
	if err != nil {
		return nil, err
	}
	versions := []map[string]any{}
	for _, component := range resolved.Components {
		versions = append(versions, map[string]any{"name": component.Name, "version": component.Version, "repository": component.Repository, "commit": component.Commit, "artifacts": component.Artifacts})
	}
	return assemblyValue(map[string]any{"ownerUserId": s.owner, "versions": versions, "evidence": resolved.Evidence, "destinations": resolved.Destinations})
}

func (s *candidateAssemblyScope) prepare(ctx context.Context, args map[string]any) (any, error) {
	if s.out.ID != "" {
		return assemblyValue(s.out)
	}
	body, err := json.Marshal(args["candidate"])
	if err != nil {
		return nil, err
	}
	var input pl.ReleaseCandidate
	if err := decodeCandidateObject(body, &input); err != nil {
		return nil, err
	}
	want, err := s.resolved()
	if err != nil {
		return nil, err
	}
	input.WorkflowDigest = want.WorkflowDigest // assigned only by preparation
	wantBody, _, err := pl.CanonicalReleaseCandidate(want)
	if err != nil {
		return nil, err
	}
	gotBody, _, err := pl.CanonicalReleaseCandidate(input)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(gotBody, wantBody) {
		return nil, errors.New("assembly projection changed or omitted declared source/evidence/artifact/target facts")
	}
	s.out, err = s.p.prepare(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("prepare assembled candidate: %w", err)
	}
	return assemblyValue(s.out)
}

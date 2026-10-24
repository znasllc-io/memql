package release

import (
	"context"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

type releaseBuildScope struct {
	i     *Integration
	lease *releaseBuildLease
}

func buildCaptureOperations(publication bool) map[string]workflowhost.Operation {
	names := []string{"releaseBuildFacts", "releaseBuildEnsureTag", "releaseBuildRun", "releaseBuildObserve", "releaseBuildAssemble"}
	if publication {
		names = []string{"releaseGetCandidate", "releaseCandidateTargets", "releaseCandidateDrafts", "releaseCandidatePublications", "releaseCreateCandidateDraft", "releasePublishCandidate", "releasePromoteCandidateDraft", "releaseSealPublishedCandidate"}
	}
	ops := make(map[string]workflowhost.Operation, len(names))
	for _, name := range names {
		ops[name] = func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("release recipe capture cannot execute operations")
		}
	}
	return ops
}

func (s *releaseBuildScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{"releaseBuildFacts": s.facts, "releaseBuildEnsureTag": s.ensureTag, "releaseBuildRun": s.run, "releaseBuildObserve": s.observe, "releaseBuildAssemble": s.assemble}
}

// Fresh original-owner/profile authority and the live native lease are required
// before every bounded operation, including effects after a long remote read.
func (s *releaseBuildScope) gate(ctx context.Context) (candidateProfile, error) {
	if err := s.lease.check(ctx); err != nil {
		return candidateProfile{}, err
	}
	p, _, digest, err := s.i.candidateProfile(ctx, s.lease.record.Selection.ProfileName)
	if err != nil || digest != s.lease.record.Selection.Configuration || p.Build == nil {
		return candidateProfile{}, errors.New("release profile authority changed; this saved release cannot perform new effects")
	}
	return p, nil
}

func (s *releaseBuildScope) facts(ctx context.Context, _ map[string]any) (any, error) {
	if _, err := s.gate(ctx); err != nil {
		return nil, err
	}
	r := s.lease.record
	return assemblyValue(map[string]any{"intentId": r.ID, "phase": r.Phase, "repository": r.Selection.Request.Repository, "sha": r.Selection.Request.ExpectedSHA, "version": r.Selection.Version, "tag": r.Selection.Tag, "tagConfirmed": r.TagConfirmed, "runAttempted": r.RunAttempted, "candidateId": r.CandidateID})
}

func (s *releaseBuildScope) inspect(ctx context.Context) (bool, error) {
	if _, err := s.gate(ctx); err != nil {
		return false, err
	}
	pipeline, err := s.i.buildPipeline()
	if err != nil {
		return false, err
	}
	r := s.lease.record.Selection.Request
	fresh, err := pipeline.InspectReleaseBuild(ctx, r)
	if err != nil {
		return false, err
	}
	if fresh.OwnerUserID != s.lease.record.Selection.Owner || fresh.PipelineID != r.PipelineID || fresh.SourceID != r.SourceID || fresh.CredentialID != r.CredentialID || fresh.RepositoryID != r.RepositoryID || fresh.InstallationID != r.InstallationID || fresh.Repository != r.Repository || fresh.Branch != r.Branch || fresh.SHA != r.ExpectedSHA {
		return false, errors.New("release source authority changed")
	}
	if fresh.TagExists && fresh.TagSHA != r.ExpectedSHA {
		return false, errors.New("selected release tag now names a different source")
	}
	if fresh.ExistingRun != nil && !matchingBuildRun(s.lease.record.Selection, *fresh.ExistingRun) {
		return false, errors.New("selected source has a different native release run")
	}
	return fresh.TagExists, nil
}

func (s *releaseBuildScope) ensureTag(ctx context.Context, _ map[string]any) (any, error) {
	p, err := s.gate(ctx)
	if err != nil {
		return nil, err
	}
	present, err := s.inspect(ctx)
	if err != nil {
		return nil, err
	}
	if present {
		s.lease.record.TagConfirmed = true
		if err := s.lease.save(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"confirmed": true}, nil
	}
	if s.lease.record.TagAttempted {
		s.lease.record.Phase, s.lease.record.ReasonCode, s.lease.record.Reason = "uncertain", "tag_unconfirmed", "The tag request is not yet confirmed. Its saved intent prevents a replacement request."
		return map[string]any{"confirmed": false}, s.lease.save(ctx)
	}
	if s.lease.record.Selection.Source.TagExists {
		return nil, errors.New("the previously observed immutable tag is no longer present; do not recreate it")
	}
	// Resolve only the fixed publication authority, never a caller URL/token.
	var lifecycle *githubrelease.Lifecycle
	for _, a := range p.ReleaseAssets {
		if a.Component != p.Build.Component || a.ID != p.Build.TagTargetID {
			continue
		}
		t := a.target(s.lease.record.Selection.Tag, s.lease.record.Selection.Request.ExpectedSHA, "release-source-tag", &githubrelease.Metadata{Name: "Release source"})
		_, target, _, err := t.snapshot()
		if err != nil {
			return nil, err
		}
		reader := candidateTargetReader{secret: s.i.resolver.systemSecret, appToken: s.i.resolver.appToken}
		target.Token, err = reader.fileToken(ctx, t)
		if err != nil {
			return nil, err
		}
		lifecycle, err = githubrelease.NewLifecycle(target)
		if err != nil {
			return nil, err
		}
		break
	}
	if lifecycle == nil {
		return nil, errors.New("release tag authority is unavailable")
	}
	if _, err := s.gate(ctx); err != nil {
		return nil, err
	}
	s.lease.record.TagAttempted, s.lease.record.Phase = true, "tagging"
	if err := s.lease.save(ctx); err != nil {
		return nil, err
	}
	// EnsureTag is called at most once for this durable selection. All recovery
	// uses Inspect above, including absent/ambiguous provider readback.
	if err := lifecycle.EnsureTag(ctx); err != nil {
		return nil, err
	}
	s.lease.record.TagConfirmed = true
	if err := s.lease.save(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"confirmed": true}, nil
}

func (s *releaseBuildScope) run(ctx context.Context, _ map[string]any) (any, error) {
	if _, err := s.gate(ctx); err != nil {
		return nil, err
	}
	if !s.lease.record.TagConfirmed {
		return nil, errors.New("release build requires its confirmed immutable tag")
	}
	present, err := s.inspect(ctx)
	if err != nil || !present {
		return nil, errors.New("release tag could not be freshly verified")
	}
	if s.lease.record.RunAttempted {
		return s.observe(ctx, nil)
	}
	pipeline, err := s.i.buildPipeline()
	if err != nil {
		return nil, err
	}
	if _, err := s.gate(ctx); err != nil {
		return nil, err
	}
	s.lease.record.RunAttempted, s.lease.record.Phase = true, "building"
	if err := s.lease.save(ctx); err != nil {
		return nil, err
	}
	run, err := pipeline.StartReleaseBuild(ctx, s.lease.record.Selection.Request, func(c context.Context) error { _, err := s.gate(c); return err })
	if err != nil {
		return nil, err
	}
	if !matchingBuildRun(s.lease.record.Selection, run) {
		return nil, errors.New("native release returned a different execution")
	}
	s.lease.record.Run = &run
	return s.recordRun(ctx)
}

func (s *releaseBuildScope) observe(ctx context.Context, _ map[string]any) (any, error) {
	if _, err := s.gate(ctx); err != nil {
		return nil, err
	}
	if s.lease.record.TagAttempted && !s.lease.record.TagConfirmed {
		present, err := s.inspect(ctx)
		if err != nil {
			return nil, err
		}
		s.lease.record.TagConfirmed = present
	}
	if !s.lease.record.RunAttempted {
		return s.factsAfterSave(ctx)
	}
	pipeline, err := s.i.buildPipeline()
	if err != nil {
		return nil, err
	}
	run, found, err := pipeline.ObserveReleaseBuild(ctx, s.lease.record.Selection.Request)
	if err != nil {
		return nil, err
	}
	if !found {
		s.lease.record.Phase, s.lease.record.ReasonCode, s.lease.record.Reason = "uncertain", "build_unconfirmed", "The build request is not yet confirmed. Its saved intent prevents a replacement request."
		return s.factsAfterSave(ctx)
	}
	if !matchingBuildRun(s.lease.record.Selection, run) || (s.lease.record.Run != nil && s.lease.record.Run.ID != run.ID) {
		return nil, errors.New("native release observation changed its execution identity")
	}
	s.lease.record.Run = &run
	return s.recordRun(ctx)
}

func (s *releaseBuildScope) recordRun(ctx context.Context) (any, error) {
	r := s.lease.record.Run
	if r == nil {
		return nil, errors.New("release run is unavailable")
	}
	s.lease.record.ReasonCode, s.lease.record.Reason = "", ""
	if s.lease.record.CandidateID != "" {
		return s.factsAfterSave(ctx)
	}
	s.lease.record.Phase = "building"
	if r.Status == "completed" {
		if r.Conclusion == "success" && !r.CancelRequested && r.WorkRunID != "" {
			s.lease.record.Phase = "built"
		} else {
			s.lease.record.Phase, s.lease.record.ReasonCode, s.lease.record.Reason = "blocked", "build_not_successful", "The selected release build did not succeed. Review its run; this release will not replace it automatically."
		}
	}
	return s.factsAfterSave(ctx)
}

func (s *releaseBuildScope) factsAfterSave(ctx context.Context) (any, error) {
	if err := s.lease.save(ctx); err != nil {
		return nil, err
	}
	return s.facts(ctx, nil)
}

func (s *releaseBuildScope) assemble(ctx context.Context, _ map[string]any) (any, error) {
	if _, err := s.gate(ctx); err != nil {
		return nil, err
	}
	if s.lease.record.CandidateID != "" {
		return s.facts(ctx, nil)
	}
	if _, err := s.observe(ctx, nil); err != nil {
		return nil, err
	}
	r := s.lease.record.Run
	if s.lease.record.Phase != "built" || r == nil || !matchingBuildRun(s.lease.record.Selection, *r) {
		return nil, errors.New("candidate preparation requires the exact successful full release build")
	}
	selection := s.lease.record.Selection
	p, binding, err := s.i.materializeProfile(ctx, selection.ProfileName, selection.Configuration, map[string]string{selection.RunAlias: r.WorkRunID})
	if err != nil {
		return nil, err
	}
	if _, err := s.gate(ctx); err != nil {
		return nil, err
	}
	record, err := p.assemble(ctx, binding.ProfileName, binding.Runs)
	if err != nil {
		return nil, err
	}
	if len(record.Manifest.Components) != 1 || record.Manifest.Components[0].Repository != selection.Request.Repository || record.Manifest.Components[0].Commit != selection.Request.ExpectedSHA || record.Manifest.Components[0].Version != selection.Version {
		return nil, fmt.Errorf("candidate does not match the saved release source")
	}
	s.lease.record.CandidateID, s.lease.record.Phase = record.ID, "ready"
	return s.factsAfterSave(ctx)
}

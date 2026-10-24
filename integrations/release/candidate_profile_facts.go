package release

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Facts are derived from the original owner's complete journal and its native
// pipeline association. A caller selects run IDs, never a tag or source SHA.
type candidateProfileFacts struct {
	ProfileName string                           `json:"profileName"`
	Components  []candidateProfileComponentFacts `json:"components"`
	Versions    map[string]string                `json:"versions"`
	Targets     []candidateProfileTargetFacts    `json:"targets"`
}
type candidateProfileComponentFacts struct {
	Name       string                     `json:"name"`
	Repository string                     `json:"repository"`
	Commit     string                     `json:"commit"`
	Version    string                     `json:"version"`
	ReleaseTag string                     `json:"releaseTag"`
	Runs       []candidateProfileRunFacts `json:"runs"`
}
type candidateProfileRunFacts struct {
	Alias            string `json:"alias"`
	WorkRunID        string `json:"workRunId"`
	PipelineID       string `json:"pipelineId"`
	PipelineRunID    string `json:"pipelineRunId"`
	Event            string `json:"event"`
	Mode             string `json:"mode"`
	Status           string `json:"status"`
	ReleaseTag       string `json:"releaseTag"`
	DefinitionDigest string `json:"definitionDigest"`
}
type candidateProfileTargetFacts struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Component string `json:"component"`
	Artifact  string `json:"artifact"`
}

func (r candidateEvidenceReader) profileRun(ctx context.Context, alias, runID, repository string) (candidateProfileRunFacts, string, error) {
	var out candidateProfileRunFacts
	plan, err := r.completeRun(ctx, runID)
	if err != nil {
		return out, "", err
	}
	if len(plan.Receipts) == 0 {
		return out, "", errors.New("profile requires complete native run evidence")
	}
	proof := plan.Receipts[0]
	rows, err := r.rows(ctx, "workRunForOwner", map[string]any{"runId": runID}, 1)
	if err != nil || len(rows) != 1 {
		return out, "", errors.New("profile work association is unavailable")
	}
	var work struct {
		Input struct {
			PipelineRunID, PipelineID string
			Attempt                   int
		}
	}
	body, _ := json.Marshal(rows[0])
	if json.Unmarshal(body, &work) != nil {
		return out, "", errors.New("invalid work association")
	}
	pid, rid := memql.BareShortId(work.Input.PipelineID), memql.BareShortId(work.Input.PipelineRunID)
	if pid == "" || rid == "" {
		return out, "", errors.New("profile work is not associated with a native pipeline run")
	}
	rows, err = r.rows(ctx, "pipelineRunForOwner", map[string]any{"runId": rid}, 1)
	if err != nil || len(rows) != 1 {
		return out, "", errors.New("profile pipeline run is unavailable")
	}
	var run struct {
		ID, OwnerUserID, PipelineID, WorkRunID, Repository, SHA, Mode, Event, ReleaseTag, Version, Status, Conclusion string
		Attempt                                                                                                       int
		CancelRequested                                                                                               bool
	}
	body, _ = json.Marshal(rows[0])
	if json.Unmarshal(body, &run) != nil {
		return out, "", errors.New("invalid pipeline run")
	}
	owner, err := candidateOwner(ctx)
	if err != nil {
		return out, "", err
	}
	if run.ID != rid || run.OwnerUserID != owner || memql.BareShortId(run.PipelineID) != pid || memql.BareShortId(run.WorkRunID) != runID ||
		run.Repository != repository || run.Repository != proof.Repository || run.SHA != proof.Commit || run.Mode != proof.Mode || run.Event != proof.Event || run.Attempt != work.Input.Attempt || work.Input.Attempt < 1 ||
		run.Status != "completed" || run.Conclusion != "success" || run.CancelRequested || !candidateSourceCommit(run.SHA) || run.ReleaseTag == "" || run.Version != run.ReleaseTag || !pl.ValidBranchName(run.ReleaseTag) {
		return out, "", errors.New("profile requires an exact completed successful tagged pipeline/work association")
	}
	rows, err = r.rows(ctx, "pipelineForOwner", map[string]any{"pipelineId": pid}, 1)
	if err != nil || len(rows) != 1 || asString(rows[0]["id"]) != pid || asString(rows[0]["ownerUserId"]) != owner || asString(rows[0]["repository"]) != repository {
		return out, "", errors.New("profile pipeline source or owner changed")
	}
	return candidateProfileRunFacts{Alias: alias, WorkRunID: runID, PipelineID: pid, PipelineRunID: rid, Event: run.Event, Mode: run.Mode, Status: run.Status, ReleaseTag: run.ReleaseTag, DefinitionDigest: proof.DefinitionDigest}, run.SHA, nil
}

func (i *Integration) profileFacts(ctx context.Context, profile candidateProfile, runs map[string]string) (candidateProfileFacts, error) {
	out := candidateProfileFacts{ProfileName: profile.Name, Versions: map[string]string{}, Components: []candidateProfileComponentFacts{}, Targets: []candidateProfileTargetFacts{}}
	if i.store == nil || i.store.engine == nil || i.candidateDeps.Load() == nil {
		return out, errors.New("profile requires native source and journal readers")
	}
	client := i.candidateDeps.Load().SourceClient
	if client == nil {
		client = i.github
	}
	reader := candidateEvidenceReader{engine: i.store.engine}
	versions := candidateVersionReader{client: client, resolver: i.resolver, sources: map[string]candidateVersionSource{}}
	used := map[string]bool{}
	for _, component := range profile.Components {
		fact := candidateProfileComponentFacts{Name: component.Component, Repository: component.Repository, Runs: []candidateProfileRunFacts{}}
		seen := map[string]bool{}
		for _, alias := range component.Runs {
			runID := memql.BareShortId(runs[alias])
			if runID == "" || runID != runs[alias] || len(runID) > 512 || seen[runID] {
				return out, errors.New("profile requires distinct bare run IDs for every declared input")
			}
			seen[runID], used[alias] = true, true
			run, commit, err := reader.profileRun(ctx, alias, runID, component.Repository)
			if err != nil {
				return out, err
			}
			if fact.Commit != "" && (fact.Commit != commit || fact.ReleaseTag != run.ReleaseTag) {
				return out, errors.New("profile component inputs disagree on source or tag")
			}
			fact.Commit, fact.ReleaseTag = commit, run.ReleaseTag
			fact.Runs = append(fact.Runs, run)
		}
		versions.sources[component.Component] = component.candidateVersionSource
		version, err := versions.readVersion(ctx, pl.ReleaseComponent{Name: fact.Name, Repository: fact.Repository, Commit: fact.Commit})
		if err != nil {
			return out, err
		}
		fact.Version = version
		out.Components = append(out.Components, fact)
	}
	if len(used) != len(runs) {
		return out, errors.New("profile contains undeclared run inputs")
	}
	for _, extra := range profile.VersionFacts {
		for _, c := range out.Components {
			if c.Name != extra.Component {
				continue
			}
			reader := versions
			source := versions.sources[c.Name]
			source.Path, source.JSONField = extra.Path, extra.JSONField
			reader.sources = map[string]candidateVersionSource{c.Name: source}
			value, err := reader.readVersion(ctx, pl.ReleaseComponent{Name: c.Name, Repository: c.Repository, Commit: c.Commit})
			if err != nil {
				return out, err
			}
			out.Versions[extra.Name] = value
		}
	}
	for _, t := range profile.Registries {
		out.Targets = append(out.Targets, candidateProfileTargetFacts{t.ID, "oci", t.Component, t.Artifact})
	}
	for _, t := range profile.ReleaseAssets {
		out.Targets = append(out.Targets, candidateProfileTargetFacts{t.ID, "file", t.Component, t.Artifact})
	}
	return out, nil
}

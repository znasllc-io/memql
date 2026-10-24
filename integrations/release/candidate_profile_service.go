package release

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

func (i *Integration) handleCandidateProfiles(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	profiles, err := i.candidateProfiles(ctx)
	if err != nil {
		return nil, err
	}
	type componentView struct {
		Name       string   `json:"name"`
		Repository string   `json:"repository"`
		Runs       []string `json:"runs"`
	}
	type profileView struct {
		Name          string          `json:"name"`
		Configuration string          `json:"configuration"`
		Components    []componentView `json:"components"`
	}
	out := []profileView{}
	for _, profile := range profiles {
		_, _, digest, err := i.candidateProfile(ctx, profile.Name)
		if err != nil {
			return nil, err
		}
		view := profileView{Name: profile.Name, Configuration: digest, Components: []componentView{}}
		for _, c := range profile.Components {
			view.Components = append(view.Components, componentView{c.Component, c.Repository, c.Runs})
		}
		out = append(out, view)
	}
	return resultNode("candidate-profiles", "", map[string]any{"profiles": out})
}

func (i *Integration) handleProfileAssemble(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	var runs map[string]string
	if err := profileDecode(args["runs"], &runs); err != nil {
		return nil, err
	}
	p, binding, err := i.materializeProfile(ctx, asString(args["profileName"]), asString(args["expectedConfiguration"]), runs)
	if err != nil {
		return nil, err
	}
	record, err := p.assemble(ctx, binding.ProfileName, binding.Runs)
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}

func (i *Integration) materializeProfile(ctx context.Context, name, expected string, runs map[string]string) (*candidatePreparer, candidateProfileBinding, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var binding candidateProfileBinding
	owner, err := candidateOwner(ctx)
	if err != nil {
		return nil, binding, err
	}
	profile, snapshot, configuration, err := i.candidateProfile(ctx, name)
	if err != nil {
		return nil, binding, err
	}
	if expected == "" || expected != configuration {
		return nil, binding, errors.New("release profile changed since selection; review it again")
	}
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, binding, err
	}
	facts, err := i.profileFacts(ctx, profile, runs)
	if err != nil {
		return nil, binding, err
	}
	key, err := profileRequestID(owner, name, configuration, runs)
	if err != nil {
		return nil, binding, err
	}
	binding, err = p.ledger.profileBinding(ctx, key, false)
	if errors.Is(err, sql.ErrNoRows) {
		value, err := assemblyValue(facts)
		if err != nil {
			return nil, binding, err
		}
		resolved, err := workflowhost.RunSnapshot(ctx, snapshot, candidateProfileContract, map[string]any{"facts": value}, workflowhost.Options{Logger: i.logger})
		if err != nil {
			return nil, binding, err
		}
		binding = candidateProfileBinding{ID: key, Owner: owner, ProfileName: name, Configuration: configuration, Runs: runs, Facts: facts, Snapshot: snapshot}
		if err := profileDecode(resolved, &binding.Selection); err != nil {
			return nil, binding, err
		}
		// Validate the complete result before writing native materialization.
		if _, err := i.profilePreparer(ctx, p, profile, binding); err != nil {
			return nil, binding, err
		}
		binding, err = p.ledger.saveProfileBinding(ctx, binding)
		if err != nil {
			return nil, binding, err
		}
	} else if err != nil {
		return nil, binding, err
	}
	if !reflect.DeepEqual(binding.Facts, facts) || binding.Snapshot.Version != snapshot.Version {
		return nil, binding, errors.New("release profile evidence changed during recovery")
	}
	p, err = i.profilePreparer(ctx, p, profile, binding)
	return p, binding, err
}

func (i *Integration) profilePreparer(ctx context.Context, p *candidatePreparer, profile candidateProfile, binding candidateProfileBinding) (*candidatePreparer, error) {
	if p.library == nil || i.candidateDeps.Load() == nil || i.store == nil || i.store.engine == nil {
		return nil, errors.New("profile assembly native dependencies are unavailable")
	}
	client := i.candidateDeps.Load().SourceClient
	if client == nil {
		client = i.github
	}
	versions := &candidateVersionReader{client: client, resolver: i.resolver, sources: map[string]candidateVersionSource{}}
	targets := &candidateTargetReader{targets: map[string]candidateRegistryTarget{}, files: map[string]candidateFileTarget{}, npm: map[string]candidateNPMTarget{}, secret: i.resolver.systemSecret, appToken: i.resolver.appToken}
	components := map[string]candidateProfileComponent{}
	facts := map[string]candidateProfileComponentFacts{}
	for _, c := range profile.Components {
		versions.sources[c.Component], components[c.Component] = c.candidateVersionSource, c
	}
	for _, c := range binding.Facts.Components {
		facts[c.Name] = c
	}
	plan := candidateAssemblyPlan{Name: profile.Name, Compatibility: binding.Selection.Compatibility, Components: []candidateAssemblyComponent{}, Targets: []string{}}
	selected := map[string]bool{}
	for _, c := range binding.Selection.Components {
		authority, ok := components[c.Name]
		if !ok || selected[c.Name] {
			return nil, errors.New("profile selected an unknown or repeated component")
		}
		selected[c.Name] = true
		plan.Components = append(plan.Components, candidateAssemblyComponent{Name: c.Name, Runs: authority.Runs, Artifacts: c.Artifacts})
	}
	if len(selected) != len(components) {
		return nil, errors.New("profile must resolve every declared source component")
	}
	for _, selection := range binding.Selection.Publications {
		found := false
		for _, authority := range profile.Registries {
			if authority.ID != selection.TargetID {
				continue
			}
			if selection.AssetName != "" || selection.Draft != nil || (selection.Tag != "" && selection.Tag != facts[authority.Component].Version && selection.Tag != "v"+facts[authority.Component].Version) {
				return nil, errors.New("profile registry tag must name its exact component version")
			}
			authority.Tag = selection.Tag
			owned, _, _, err := authority.snapshot()
			if err != nil {
				return nil, err
			}
			targets.targets[authority.ID], found = owned, true
		}
		for _, authority := range profile.ReleaseAssets {
			if authority.ID != selection.TargetID {
				continue
			}
			fact := facts[authority.Component]
			if selection.Tag != "" || selection.AssetName == "" || selection.Draft == nil || fact.ReleaseTag != authority.TagPrefix+fact.Version {
				return nil, errors.New("profile file publication requires exact native tag/version and explicit draft metadata")
			}
			owned, _, _, err := authority.target(fact.ReleaseTag, fact.Commit, selection.AssetName, selection.Draft).snapshot()
			if err != nil {
				return nil, err
			}
			targets.files[authority.ID], found = owned, true
		}
		if !found {
			return nil, errors.New("profile publication is outside its fixed destination authority")
		}
		plan.Targets = append(plan.Targets, selection.TargetID)
	}
	if err := validateAssemblyPlans([]candidateAssemblyPlan{plan}, versions, targets); err != nil {
		return nil, err
	}
	bound := *p
	bound.engineRevision = i.profileRevision
	bound.versionReader, bound.targetReader, bound.evidence = versions, targets, candidateEvidenceReader{engine: i.store.engine}
	bound.assemblyPlans, bound.profileBinding = []candidateAssemblyPlan{plan}, &binding
	bound.authority = func(ctx context.Context) error {
		owner, err := candidateOwner(ctx)
		if err != nil || owner != binding.Owner {
			return errors.New("profile requires its original owner")
		}
		_, _, current, err := i.candidateProfile(ctx, profile.Name)
		if err != nil || current != binding.Configuration {
			return errors.New("release profile authority or installed resolver was changed or revoked")
		}
		return nil
	}
	return &bound, nil
}

func (p *candidatePreparer) checkProfileAuthority(ctx context.Context) error {
	if p != nil && p.authority != nil {
		return p.authority(ctx)
	}
	return nil
}

func (i *Integration) handleCandidateTargets(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	key := asString(args["candidateId"])
	p, err := i.configuredCandidateFor(ctx, key)
	if err != nil {
		return nil, err
	}
	record, err := p.ledger.get(ctx, key)
	if err != nil {
		return nil, err
	}
	views, err := candidateTargetViews(p.targetReader)
	if err != nil {
		return nil, err
	}
	out := []candidateTargetView{}
	for _, d := range record.Manifest.Destinations {
		found := false
		for _, view := range views {
			if view.TargetID == d.TargetID && view.TargetDigest == d.TargetDigest && view.Component == d.Component && view.Artifact == d.Artifact {
				out = append(out, view)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("candidate target is no longer authorized")
		}
	}
	return resultNode("candidate-targets", key, map[string]any{"candidateId": key, "targets": out})
}

// Keep the profile materialization in the immutable candidate identity without
// changing any format-one fingerprint or published candidate wire fields.
func (p *candidatePreparer) bindProfileCandidate(ctx context.Context, candidate pl.ReleaseCandidate, key string) error {
	if p.profileBinding == nil {
		return nil
	}
	if err := p.checkProfileAuthority(ctx); err != nil {
		return err
	}
	return p.ledger.attachProfileCandidate(ctx, *p.profileBinding, key)
}

func profileOperations(p *candidatePreparer, ops map[string]workflowhost.Operation) map[string]workflowhost.Operation {
	for name, operation := range ops {
		ops[name] = func(ctx context.Context, args map[string]any) (any, error) {
			if err := p.checkProfileAuthority(ctx); err != nil {
				return nil, err
			}
			return operation(ctx, args)
		}
	}
	return ops
}

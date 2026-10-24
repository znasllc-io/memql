package release

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/buildinfo"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

const CandidateProfilesVariable = "MEMQL_RELEASE_PROFILES"
const candidateProfileContract = "release-profile-v1"

// Profiles are opt-in authority, independent of the legacy format-one candidate
// configuration. A product recipe selects artifacts and names; it cannot choose
// a repository, connection, credential or version source.
type candidateProfileConfiguration struct {
	FormatVersion int                `json:"formatVersion"`
	Profiles      []candidateProfile `json:"profiles"`
}
type candidateProfile struct {
	Name          string                          `json:"name"`
	Resolver      string                          `json:"resolver"`
	Components    []candidateProfileComponent     `json:"components"`
	VersionFacts  []candidateProfileVersionFact   `json:"versionFacts,omitempty"`
	Registries    []candidateRegistryTarget       `json:"registries,omitempty"`
	ReleaseAssets []candidateProfileFileAuthority `json:"releaseAssets,omitempty"`
	Build         *candidateBuildConfiguration    `json:"build,omitempty"`
}
type candidateProfileComponent struct {
	candidateVersionSource
	Runs []string `json:"runs"`
}
type candidateProfileVersionFact struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Path      string `json:"path"`
	JSONField string `json:"jsonField,omitempty"`
}
type candidateProfileFileAuthority struct {
	ID                string   `json:"id"`
	Component         string   `json:"component"`
	Artifact          string   `json:"artifact"`
	APIOrigin         string   `json:"apiOrigin"`
	UploadOrigin      string   `json:"uploadOrigin"`
	DownloadOrigins   []string `json:"downloadOrigins,omitempty"`
	Repository        string   `json:"repository"`
	TagPrefix         string   `json:"tagPrefix"`
	CredentialSecret  string   `json:"credentialSecret,omitempty"`
	UseGitHubApp      bool     `json:"useGitHubApp,omitempty"`
	RootCAPEM         string   `json:"rootCaPem,omitempty"`
	AllowLoopbackHTTP bool     `json:"allowLoopbackHttp,omitempty"`
}

func (a candidateProfileFileAuthority) target(tag, commit, asset string, draft *githubrelease.Metadata) candidateFileTarget {
	return candidateFileTarget{ID: a.ID, Component: a.Component, Artifact: a.Artifact, APIOrigin: a.APIOrigin,
		UploadOrigin: a.UploadOrigin, DownloadOrigins: a.DownloadOrigins, Repository: a.Repository,
		Tag: tag, SourceCommit: commit, AssetName: asset, Draft: draft, CredentialSecret: a.CredentialSecret,
		UseGitHubApp: a.UseGitHubApp, RootCAPEM: a.RootCAPEM, AllowLoopbackHTTP: a.AllowLoopbackHTTP}
}

func profileDigest(value any) (string, error) {
	fp, err := workjournal.DefinitionFingerprint(candidateProfileContract, []workjournal.StepDecl{{Key: "binding", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"value": value}}})
	return "sha256:" + strings.TrimPrefix(fp, "work-definition-v2:"), err
}

func (p candidateProfile) validate() error {
	if !candidateTargetName.MatchString(p.Name) || !candidateJSONField.MatchString(p.Resolver) || len(p.Components) == 0 || len(p.Components) > 16 ||
		len(p.VersionFacts) > 32 || len(p.Registries)+len(p.ReleaseAssets) == 0 || len(p.Registries)+len(p.ReleaseAssets) > 128 {
		return errors.New("release profile requires bounded sources, targets and an installed resolver")
	}
	sources, aliases := map[string]candidateVersionSource{}, map[string]bool{}
	for _, c := range p.Components {
		if c.validate() != nil || !candidateTargetName.MatchString(c.Component) || sources[c.Component].Component != "" || len(c.Runs) == 0 || len(c.Runs) > 16 {
			return errors.New("release profile has invalid or repeated source components")
		}
		sources[c.Component] = c.candidateVersionSource
		for _, alias := range c.Runs {
			key := strings.ToLower(alias)
			if !candidateJSONField.MatchString(alias) || aliases[key] {
				return errors.New("release profile run aliases must be unique")
			}
			aliases[key] = true
		}
	}
	versions := map[string]bool{}
	for _, v := range p.VersionFacts {
		source := sources[v.Component]
		if !candidateJSONField.MatchString(v.Name) || versions[v.Name] || source.Component == "" {
			return errors.New("release profile version facts require unique names and a declared component")
		}
		versions[v.Name] = true
		source.Path, source.JSONField = v.Path, v.JSONField
		if source.validate() != nil {
			return errors.New("release profile version fact has an invalid source path")
		}
	}
	targets := map[string]bool{}
	for _, target := range p.Registries {
		if target.Tag != "" || targets[target.ID] || sources[target.Component].Component == "" {
			return errors.New("profile registries require unique fixed authority without a version tag")
		}
		if _, _, _, err := target.snapshot(); err != nil {
			return err
		}
		targets[target.ID] = true
	}
	for _, target := range p.ReleaseAssets {
		if targets[target.ID] || sources[target.Component].Repository != target.Repository || target.TagPrefix == "" || len(target.TagPrefix) > 96 ||
			!pl.ValidBranchName(target.TagPrefix+"1.2.3") {
			return errors.New("profile files require unique matching source authority and a fixed tag prefix")
		}
		// Validate protocol/trust/credential fields using the existing native
		// constructor. These fixture values are never persisted or published.
		probe := target.target(target.TagPrefix+"1.2.3", strings.Repeat("a", 40), "profile-validation", &githubrelease.Metadata{Name: "Profile validation"})
		if _, _, _, err := probe.snapshot(); err != nil {
			return err
		}
		targets[target.ID] = true
	}
	return p.validateBuild()
}

func (i *Integration) candidateProfiles(ctx context.Context) ([]candidateProfile, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return nil, err
	}
	if i == nil || i.resolver.systemVariable == nil {
		return nil, errors.New("release profile configuration is unavailable")
	}
	raw, err := i.resolver.systemVariable(memql.ContextWithFreshRead(ctx), CandidateProfilesVariable)
	if err != nil && !memql.IsVariableNotFound(err) {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return []candidateProfile{}, nil
	}
	var cfg candidateProfileConfiguration
	if err := decodeCandidateObject([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	if cfg.FormatVersion != 1 || len(cfg.Profiles) > 64 {
		return nil, errors.New("unsupported or oversized release profiles")
	}
	seen := map[string]bool{}
	for _, p := range cfg.Profiles {
		if seen[p.Name] {
			return nil, errors.New("duplicate release profile")
		}
		seen[p.Name] = true
		if err := p.validate(); err != nil {
			return nil, err
		}
	}
	return cfg.Profiles, nil
}

func (i *Integration) candidateProfile(ctx context.Context, name string) (candidateProfile, *workflowhost.Snapshot, string, error) {
	profiles, err := i.candidateProfiles(ctx)
	if err != nil {
		return candidateProfile{}, nil, "", err
	}
	for _, p := range profiles {
		if p.Name != name {
			continue
		}
		snapshot, err := workflowhost.Capture(p.Resolver, candidateProfileContract, i.profileSource, nil)
		if err != nil {
			return candidateProfile{}, nil, "", err
		}
		revision := buildinfo.Commit()
		if i.profileRevision != nil {
			revision = i.profileRevision()
		}
		build, err := i.captureBuildRecipes(p)
		if err != nil {
			return candidateProfile{}, nil, "", err
		}
		digest, err := profileDigest(struct {
			Profile  candidateProfile
			Resolver string
			Engine   string
			Build    *releaseBuildRecipeIdentity `json:",omitempty"`
		}{p, snapshot.Version, revision, build.identity()})
		return p, snapshot, digest, err
	}
	return candidateProfile{}, nil, "", errors.New("release profile is absent or no longer authorized")
}

type candidateProfileSelection struct {
	Components []struct {
		Name      string                      `json:"name"`
		Artifacts []candidateAssemblyArtifact `json:"artifacts"`
	} `json:"components"`
	Compatibility []pl.ReleaseCompatibility `json:"compatibility,omitempty"`
	Publications  []struct {
		TargetID  string                  `json:"targetId"`
		Tag       string                  `json:"tag,omitempty"`
		AssetName string                  `json:"assetName,omitempty"`
		Draft     *githubrelease.Metadata `json:"draft,omitempty"`
	} `json:"publications"`
}

// JSON round trips use the same strict decoder as external candidate input.
// Unknown nested output cannot become a new connection or authority field.
func profileDecode(value any, into any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return errors.New("release profile result cannot be encoded")
	}
	return decodeCandidateObject(body, into)
}

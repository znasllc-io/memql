package release

import (
	"errors"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// An assembly declares selection, not evidence. Run IDs are supplied separately;
// versions, execution verdicts, byte digests and receipt IDs are always resolved.
type candidateAssemblyPlan struct {
	Name          string                       `json:"name"`
	Components    []candidateAssemblyComponent `json:"components"`
	Compatibility []pl.ReleaseCompatibility    `json:"compatibility"`
	Targets       []string                     `json:"targets"`
}

type candidateAssemblyComponent struct {
	Name      string                      `json:"name"`
	Runs      []string                    `json:"runs"`
	Artifacts []candidateAssemblyArtifact `json:"artifacts"`
}

type candidateAssemblyArtifact struct {
	Name              string `json:"name"`
	Run               string `json:"run"`
	StepKey           string `json:"stepKey"`
	Path              string `json:"path"`
	Kind              string `json:"kind"`
	Platform          string `json:"platform"`
	ImageMetadataPath string `json:"imageMetadataPath,omitempty"`
}

func assemblyArtifactPath(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) &&
		!path.IsAbs(value) && path.Clean(value) == value && value != "." && value != ".." &&
		!strings.HasPrefix(value, "../") && !strings.Contains(value, "\\") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validateAssemblyPlans(plans []candidateAssemblyPlan, versions *candidateVersionReader, targets *candidateTargetReader) error {
	if len(plans) > 64 {
		return errors.New("release assembly configuration exceeds 64 plans")
	}
	names := map[string]bool{}
	for _, plan := range plans {
		if !candidateTargetName.MatchString(plan.Name) || names[plan.Name] || len(plan.Components) == 0 || len(plan.Components) > 64 ||
			len(plan.Targets) == 0 || len(plan.Targets) > 1024 || len(plan.Compatibility) > 256 {
			return errors.New("release assembly requires a unique name and bounded components, targets and compatibility")
		}
		names[plan.Name] = true
		components, aliases, artifacts := map[string]bool{}, map[string]bool{}, map[string]string{}
		for _, component := range plan.Components {
			if !candidateTargetName.MatchString(component.Name) || components[component.Name] || versions.sources[component.Name].Component == "" ||
				len(component.Runs) == 0 || len(component.Runs) > 64 || len(component.Artifacts) == 0 || len(component.Artifacts) > 128 {
				return errors.New("assembly component requires a unique configured source, run inputs and artifacts")
			}
			components[component.Name] = true
			for _, alias := range component.Runs {
				key := strings.ToLower(alias) // input JSON rejects case-folded duplicate keys
				if !candidateJSONField.MatchString(alias) || aliases[key] {
					return errors.New("assembly run inputs must have unique bounded names")
				}
				aliases[key] = true
			}
			for _, artifact := range component.Artifacts {
				key := component.Name + "/" + artifact.Name
				if !candidateTargetName.MatchString(artifact.Name) || artifacts[key] != "" || !slices.Contains(component.Runs, artifact.Run) ||
					artifact.StepKey == "" || len(artifact.StepKey) > 512 || strings.IndexFunc(artifact.StepKey, func(r rune) bool { return r < 33 || r > 126 }) >= 0 ||
					!assemblyArtifactPath(artifact.Path) {
					return errors.New("assembly artifact requires a unique name and exact declared producer/path")
				}
				switch artifact.Kind {
				case "oci":
					if (artifact.Platform != "linux/amd64" && artifact.Platform != "linux/arm64") || !assemblyArtifactPath(artifact.ImageMetadataPath) || artifact.ImageMetadataPath == artifact.Path {
						return errors.New("assembly OCI artifact requires a supported platform and separate producer metadata path")
					}
				case "file":
					if artifact.ImageMetadataPath != "" || artifact.Platform == "" || len(artifact.Platform) > 512 || strings.IndexFunc(artifact.Platform, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
						return errors.New("assembly file artifact requires a platform and no image metadata")
					}
				default:
					return errors.New("assembly artifact kind must be oci or file")
				}
				artifacts[key] = artifact.Kind
			}
		}
		if len(aliases) > 256 {
			return errors.New("release assembly exceeds 256 run inputs")
		}
		seenTargets := map[string]bool{}
		for _, name := range plan.Targets {
			if seenTargets[name] {
				return errors.New("assembly target is repeated")
			}
			seenTargets[name] = true
			registry, file := targets.targets[name], targets.files[name]
			if registry.ID != "" {
				if artifacts[registry.Component+"/"+registry.Artifact] != "oci" {
					return errors.New("assembly registry target does not match a declared OCI artifact")
				}
			} else if file.ID != "" {
				if artifacts[file.Component+"/"+file.Artifact] != "file" {
					return errors.New("assembly file target does not match a declared file artifact")
				}
			} else {
				return errors.New("assembly target is not configured")
			}
		}
		seenRules := map[string]bool{}
		for _, rule := range plan.Compatibility {
			key := rule.Component + "/" + rule.Requires
			if !components[rule.Component] || !components[rule.Requires] || rule.Component == rule.Requires || seenRules[key] || rule.MinVersion == "" || rule.MaxExclusive == "" {
				return errors.New("assembly compatibility requires unique declared components and explicit version bounds")
			}
			seenRules[key] = true
		}
	}
	return nil
}

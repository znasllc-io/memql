package pipelines

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"strings"
)

// ImageBuild describes one bounded, single-platform Dockerfile build. Source
// selection, stage ordering and publication remain the caller's workflow.
// No registry push, daemon arguments, mounts or credential channels are exposed.
type ImageBuild struct {
	Context    string            `yaml:"context" json:"context"`
	Dockerfile string            `yaml:"dockerfile" json:"dockerfile"`
	Target     string            `yaml:"target,omitempty" json:"target,omitempty"`
	Args       map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
}

const ImageBuildDirectory = ".memql-image-build"

func ImageBuildArtifacts() []string {
	return []string{ImageBuildDirectory + "/image.oci.tar", ImageBuildDirectory + "/metadata.json"}
}

var imageBuildName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

func CheckImageBuild(build *ImageBuild, platform string) error {
	if build == nil {
		return nil
	}
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return fmt.Errorf("imageBuild requires an explicit Linux amd64 or arm64 platform")
	}
	for name, value := range map[string]string{"context": build.Context, "dockerfile": build.Dockerfile} {
		if value == "" || len(value) > 512 || path.IsAbs(value) || path.Clean(value) != value ||
			value == ".." || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "-") ||
			strings.ContainsAny(value, "\\\x00\r\n") || (name == "dockerfile" && value == ".") {
			return fmt.Errorf("imageBuild %s must be a clean relative source path", name)
		}
	}
	if build.Target != "" && !imageBuildName.MatchString(build.Target) {
		return fmt.Errorf("imageBuild target must be a Dockerfile stage name")
	}
	if len(build.Args) > 64 {
		return fmt.Errorf("imageBuild supports at most 64 public build arguments")
	}
	for key, value := range build.Args {
		if !imageBuildName.MatchString(key) || len(value) > 4096 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("imageBuild argument names and values must be bounded text without NUL")
		}
	}
	return nil
}

func CloneImageBuild(build *ImageBuild) *ImageBuild {
	if build == nil {
		return nil
	}
	out := *build
	out.Args = maps.Clone(build.Args)
	return &out
}

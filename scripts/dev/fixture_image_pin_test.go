package dev

// Service fixtures remain mirrored and immutable. Native pull authority is
// bound by the executor; an Actions GITHUB_TOKEN is no longer involved.
import (
	"regexp"
	"strings"
	"testing"
)

const mirrorPrefix = "ghcr.io/znasllc-io/"

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

func TestCIServiceImagesAreMirroredAndDigestPinned(t *testing.T) {
	spec := nativePipeline(t)
	used := map[string]bool{}
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			for _, name := range step.Services {
				used[name] = true
			}
		}
	}
	if len(used) == 0 {
		t.Fatal("native CI declares no service consumers")
	}
	for name := range used {
		image := spec.Services[name].Image
		if !strings.HasPrefix(image, mirrorPrefix) || !digestPinned.MatchString(image) {
			t.Errorf("service %s is not an immutable organization mirror: %s", name, image)
		}
	}
}

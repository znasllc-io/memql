package dev

// Native caches are attempt/trust scoped by the executor. Actions' restore/
// save-writer rules do not apply; keep the cache attached to the actual steps.
import (
	"slices"
	"strings"
	"testing"
)

func TestNativeGoStepsUseTheDeclaredCache(t *testing.T) {
	spec := nativePipeline(t)
	checked := 0
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Execution == "native" {
				continue
			} // native-env owns and removes host caches
			usesGo := false
			for _, line := range commandLines(step.Run) {
				if strings.HasPrefix(line, "go test ") || strings.HasPrefix(line, "go build ") || strings.HasPrefix(line, "go vet ") {
					usesGo = true
				}
			}
			if !usesGo {
				continue
			}
			checked++
			caches := spec.Caches
			if step.Caches != nil {
				caches = *step.Caches
			}
			if !slices.Contains(caches, "go") {
				t.Errorf("%s.%s compiles without the native Go cache", stage.Name, step.Name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Go command steps were checked")
	}
}

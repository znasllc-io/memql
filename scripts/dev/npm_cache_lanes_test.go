package dev

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestNativeNodeStepsUseTheDeclaredCache(t *testing.T) {
	spec := nativePipeline(t)
	checked := 0
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Execution == "native" || !(strings.Contains(step.Run, "npm ") || strings.Contains(step.Run, "make os-install") || strings.Contains(step.Run, "make sdk-ts-install") || strings.Contains(step.Run, "make viewkit-install")) {
				continue
			}
			checked++
			caches := spec.Caches
			if step.Caches != nil {
				caches = *step.Caches
			}
			if !slices.Contains(caches, "npm") {
				t.Errorf("%s.%s installs without the native npm cache", stage.Name, step.Name)
			}
		}
	}
	if checked < 3 {
		t.Fatalf("checked only %d native Node steps", checked)
	}
}

var retryWraps = regexp.MustCompile(`scripts/ci/retry\.sh\s+--\s+(.*)$`)
var testCommand = regexp.MustCompile(`(^|\s)(go test|npm test|npm run test)(\s|$)|(^|\s)make\s+\S*-test(\s|$)`)

func TestNoRetryWrapsATest(t *testing.T) {
	for _, stage := range nativePipeline(t).Stages {
		for _, step := range stage.Steps {
			for _, line := range commandLines(step.Run) {
				match := retryWraps.FindStringSubmatch(line)
				if match != nil && testCommand.MatchString(match[1]) {
					t.Errorf("%s.%s hides a flaky test by retrying %s", stage.Name, step.Name, line)
				}
			}
		}
	}
}

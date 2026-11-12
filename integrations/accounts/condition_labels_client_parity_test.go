package accounts

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// exportTabLabelsPath is the Export tab's copy of the condition labels,
// relative to the repository root. The tab names each condition while it
// renders and the not-ready refusal names the same conditions in a sentence
// the wizard shows under it, so the two copies say one thing in two places.
const exportTabLabelsPath = "clients/os/src/apps/accounts/exports.ts"

var (
	conditionLabelBlock = regexp.MustCompile(`(?s)const\s+CONDITION_LABEL\s*(?::[^=]+)?=\s*\{(.*?)\};`)
	conditionLabelEntry = regexp.MustCompile(`(\w+)\s*:\s*"([^"]*)"`)
)

// repositoryRoot walks up from this package's directory to the one holding
// go.work.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot read the test's directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work above the test's directory, so %s cannot be found", exportTabLabelsPath)
		}
		dir = parent
	}
}

// TestConditionLabelsMatchTheExportTab keeps conditionLabels and the Export
// tab's CONDITION_LABEL in step. A missing file or a map this cannot read is a
// failure, not a skip: a skip would pass forever once the file moved, which is
// when the two copies are most likely to differ.
func TestConditionLabelsMatchTheExportTab(t *testing.T) {
	path := filepath.Join(repositoryRoot(t), exportTabLabelsPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the Export tab's condition labels are unreadable at %s: %v\n"+
			"If the file moved, update exportTabLabelsPath; the labels must still be held in one place with conditionLabels.", path, err)
	}
	block := conditionLabelBlock.FindSubmatch(raw)
	if block == nil {
		t.Fatalf("%s declares no CONDITION_LABEL object literal this test can read", path)
	}
	client := map[string]string{}
	for _, m := range conditionLabelEntry.FindAllSubmatch(block[1], -1) {
		client[string(m[1])] = string(m[2])
	}
	mismatch := len(client) != len(conditionLabels)
	for name, want := range conditionLabels {
		if client[name] != want {
			mismatch = true
		}
	}
	if mismatch {
		t.Fatalf("the condition labels disagree:\n  conditionLabels (export_readiness.go) = %v\n  CONDITION_LABEL (%s) = %v\n"+
			"Both must change together: the not-ready refusal and the Export tab name the same conditions.",
			conditionLabels, exportTabLabelsPath, client)
	}
}

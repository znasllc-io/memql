package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// TestRunWritesAndChecksThePage drives both modes against a scratch copy: the
// check refuses a missing or stale page naming the command that fixes it, the
// write mode repairs it, and a second write reports nothing to do.
func TestRunWritesAndChecksThePage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attribute-matrix.md")

	if _, err := run(path, true); err == nil || !strings.Contains(err.Error(), "make docs-matrix") {
		t.Fatalf("-check on a missing page: got %v, want a refusal naming `make docs-matrix`", err)
	}

	msg, err := run(path, false)
	if err != nil || !strings.HasPrefix(msg, "wrote ") {
		t.Fatalf("write: got %q, %v", msg, err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != dslspec.AttributeMatrix() {
		t.Fatal("the written page is not what the registry renders")
	}
	if msg, err := run(path, true); err != nil || !strings.Contains(msg, "current") {
		t.Fatalf("-check on a fresh page: got %q, %v", msg, err)
	}
	if msg, err := run(path, false); err != nil || !strings.Contains(msg, "unchanged") {
		t.Fatalf("a second write: got %q, %v", msg, err)
	}

	// A hand edit is stale: the check refuses it without touching the file,
	// and a write restores the rendered page.
	edited := strings.Replace(string(written), "| flag |", "| string |", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(path, true); err == nil || !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "make docs-matrix") {
		t.Fatalf("-check on a hand-edited page: got %v, want a stale refusal naming `make docs-matrix`", err)
	}
	if after, _ := os.ReadFile(path); string(after) != edited {
		t.Fatal("-check wrote to the page")
	}
	if _, err := run(path, false); err != nil {
		t.Fatal(err)
	}
	if repaired, _ := os.ReadFile(path); string(repaired) != dslspec.AttributeMatrix() {
		t.Fatal("a write did not restore the rendered page")
	}
}

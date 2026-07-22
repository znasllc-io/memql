package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// constructInternalRe matches a standalone leading-annotation @internal line
// -- the construct-level form hard-retired under the 2026.08 epoch (#2620
// ruling / #2708). Field-level @internal on concept PROPERTIES (inline after
// a field type, the @secret/@pii sensitivity family) is a different, live
// surface and deliberately does not match: property annotations never appear
// alone on a line.
var constructInternalRe = regexp.MustCompile(`^\s*@internal\s*$`)

// TestNoRetiredConstructAnnotations is the #2708 corpus lock-in, the
// TestNoClockCallForms shape: the load gate already rejects construct-level
// @internal with a migration hint, but this gives a clear tree-wide
// file:line report and documents the contract at the source.
func TestNoRetiredConstructAnnotations(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			if constructInternalRe.MatchString(line) {
				t.Errorf("%s:%d: construct-level @internal is retired (2026.08 epoch, #2620 ruling / #2708) -- delete the annotation", p, i+1)
			}
		}
	}
}

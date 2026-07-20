package dsl

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// TestNoRedundantVersion keeps the tree stripped of @version("1.0.0")
// (#2613): absent @version means 1.0.0 (the loader default), so the literal
// annotation is pure ceremony -- ~18 chars per line teaching every reader
// that the default needs stating. An explicit @version stays fully valid
// for genuine non-defaults; only the redundant literal is gated.
//
// _reference/ is structurally outside this gate (underscore walker skip +
// embed omission), matching the no_redundant_enabled gate.
func TestNoRedundantVersion(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	redundantRe := regexp.MustCompile(`(?m)^[ \t]*@version\("1\.0\.0"\)[ \t]*$`)

	var hits []string
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
			if redundantRe.MatchString(stripLineComment(line)) {
				hits = append(hits, fmt.Sprintf("%s:%d", p, i+1))
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("%d redundant @version(\"1.0.0\") annotation(s); absent means 1.0.0 (#2613) -- delete the line(s):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

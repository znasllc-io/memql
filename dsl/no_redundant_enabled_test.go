package dsl

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

// TestNoRedundantEnabled keeps the tree stripped of construct-attached
// @enabled (#2610): enabled is the default on every kind (#2604-#2608), so
// the annotation is an accepted no-op that teaches every reader the wrong
// lesson. It stays legal to PARSE forever (legacy-DSL compatibility, and
// Sense surfaces a removal hint), but the shipped tree does not carry it.
//
// _reference/ files are structurally outside this gate: the walker skips
// underscore-prefixed paths and the embed directive omits them entirely,
// so no code-level exclusion exists here. Their construct examples are
// stripped too -- a sheet whose prose calls @enabled an accepted no-op
// must not model it.
func TestNoRedundantEnabled(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	// Match the annotation form only (start of line / after whitespace),
	// not the English word in comments or @description prose.
	annotationRe := regexp.MustCompile(`(?m)^[ \t]*@enabled\b`)

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
			if annotationRe.MatchString(stripLineComment(line)) {
				hits = append(hits, fmt.Sprintf("%s:%d", p, i+1))
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("%d construct-attached @enabled annotation(s) in the tree; enabled is the default (#2604-#2608) -- delete the line(s):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

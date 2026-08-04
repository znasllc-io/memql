package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// preamble_attachment_test.go -- memql#2965, the wiring half.
//
// component/language/parser's own tests pin the detector. These pin that it is
// REACHED: the lane reads raw source off the tree and the diagnostic reaches
// the lint report. Without this the rule could be correct and never run, which
// is the shape memql#2965 is itself an instance of.

const orphanedQuerySource = `use probe.concepts.{ thing }

@public
@description("intentionally caller-scope-free")
/*
query thing zzParked {
  filter  label=="x"
}
*/
query thing zzLive {
  filter  label=="y"
}
`

const cleanQuerySource = `use probe.concepts.{ thing }

@public
@description("intentionally caller-scope-free")
query thing zzLive {
  filter  label=="y"
}
`

const probeConceptSource = `@version("1.0.0")
@namespace("probe")
@description("d")
concept thing {
  label string @required @description("l")
}
`

func treeFor(t *testing.T, querySrc string) *Tree {
	t.Helper()
	root := fstest.MapFS{
		"probe/concepts.memql": {Data: []byte(probeConceptSource)},
		"probe/queries.memql":  {Data: []byte(querySrc)},
	}
	tree, err := Load(root)
	if err != nil {
		t.Fatalf("fixture tree did not load, so this measures nothing: %v", err)
	}
	return tree
}

// The case with teeth. A `@public` query is one whose authorization is DECLARED
// rather than filtered, so losing the annotation is not cosmetic -- and unlike
// the builtin case, the loader raises nothing at all here. Measured: without
// this lane the fixture lints completely clean.
func TestPreambleAttachmentReportsAnOrphanedPublicQuery(t *testing.T) {
	errs := treeFor(t, orphanedQuerySource).VerifyPreambleAttachment()
	if len(errs) != 1 {
		t.Fatalf("expected exactly one diagnostic, got %d: %v", len(errs), errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{
		"probe/queries.memql:3", // the first @ line, where the author edits
		"line 5",                // the `/*` that broke the run
		"@public",               // what was orphaned, named
		"attached to nothing",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the diagnostic must contain %q so the author can act on it without "+
				"re-deriving the rule.\n  got: %s", want, msg)
		}
	}
}

// The other direction: an ordinary tree must be silent, or the lane cannot be
// wired in as an error.
func TestPreambleAttachmentIsQuietOnACleanTree(t *testing.T) {
	if errs := treeFor(t, cleanQuerySource).VerifyPreambleAttachment(); len(errs) != 0 {
		t.Errorf("a clean tree produced %d diagnostic(s), which would make the lane unusable: %v",
			len(errs), errs)
	}
}

// A nil tree and a tree with no Root must not panic -- the lane runs on every
// lint invocation, including ones where the tree failed to load.
func TestPreambleAttachmentToleratesAnEmptyTree(t *testing.T) {
	var nilTree *Tree
	if errs := nilTree.VerifyPreambleAttachment(); errs != nil {
		t.Errorf("nil tree returned %v", errs)
	}
	if errs := (&Tree{}).VerifyPreambleAttachment(); errs != nil {
		t.Errorf("rootless tree returned %v", errs)
	}
}

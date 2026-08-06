package dsl

import (
	"io"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// TestNoRedundantDescription keeps the shipped tree on the /// doc-comment
// form (#2636, epic #2601): the gate RUNS the exact #2635 codemod
// (parser.RewriteDocCommentDescriptions, self-verifying), so gate and
// codemod cannot drift -- a file the codemod would still change carries a
// construct-level @description where /// suffices, or a restating
// "// Arguments:" / "// Returns:" block. Seeds, builtins, and shapes the
// codemod's verification refuses are untouched by construction, and
// @description itself stays valid as the compatibility fallback (the gate
// targets the REDUNDANT long form, not the annotation's existence). Fix a
// failure with:
//
//	go run ./cmd/memqlmigrate --rewrite=doc-comment-descriptions -w dsl/<domain>
func TestNoRedundantDescription(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	var stale []string
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
		rewritten, rerr := languageParser.RewriteDocCommentDescriptions(raw)
		if rerr != nil {
			t.Fatalf("rewrite %s: %v", p, rerr)
		}
		if string(rewritten) != string(raw) {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d file(s) carry @description where /// suffices (or a restating Arguments/Returns block); run `go run ./cmd/memqlmigrate --rewrite=doc-comment-descriptions -w` on: %v", len(stale), stale)
	}
}

// The gate is live: a deliberately regressed fixture (the redundant long
// form) must be flagged, and the codemod-converged form must pass.
func TestNoRedundantDescription_GateIsLive(t *testing.T) {
	regressed := []byte(`// Lists active spaces for the calling user, newest first.
@description("Lists active spaces for the calling user, newest first.")
query space queryGateProbe {
  filter ownerUserId == actor.userId
}
`)
	rewritten, err := languageParser.RewriteDocCommentDescriptions(regressed)
	if err != nil {
		t.Fatal(err)
	}
	if string(rewritten) == string(regressed) {
		t.Fatal("the gate's detector must flag the redundant long form")
	}
	converged, err := languageParser.RewriteDocCommentDescriptions(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if string(converged) != string(rewritten) {
		t.Fatal("the converged form must pass the gate")
	}
}

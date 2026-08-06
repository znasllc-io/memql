package dsl

import (
	"io"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// TestNoLonghandSingleStepAutomation keeps single-step pass-through
// automations in the terse arrow form (#2619): the gate RUNS the exact
// codemod (parser.RewriteLonghandSingleStepAutomation, which verifies
// every rewrite through the engine's own two-stage lowering), so gate
// and codemod cannot drift. Multi-step automations, bodies with extra
// payload keys, and constructs carrying comments are untouched by
// construction. Fix a failure with:
//
//	go run ./cmd/memqlmigrate --rewrite=terse-automation -w dsl/
func TestNoLonghandSingleStepAutomation(t *testing.T) {
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
		rewritten, rerr := languageParser.RewriteLonghandSingleStepAutomation(raw)
		if rerr != nil {
			t.Fatalf("rewrite %s: %v", p, rerr)
		}
		if string(rewritten) != string(raw) {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d file(s) carry longhand single-step automations (#2619) -- run `go run ./cmd/memqlmigrate --rewrite=terse-automation -w dsl/`:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

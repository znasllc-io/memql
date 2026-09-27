package workbench

import (
	"context"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/dsl"
)

// TestTheRunReadTheWorkbenchUsesIsDeclared is the guard the planById era
// lacked. The workspace owner is resolved by ONE read (planRow), and every
// fake in this package answers whatever query string it is handed -- which is
// how planById went on being "answered" in tests for months after
// v1:planner:plan and its reads were retired, while every real dispatch was
// refused as workspace_owner_unresolved (found by epic memql#5408).
//
// So the query the store renders is checked against the SHIPPED DSL tree: the
// construct must be declared, over v1:work:run, with a `runId` argument.
func TestTheRunReadTheWorkbenchUsesIsDeclared(t *testing.T) {
	var captured string
	store := &workspaceStore{exec: func(_ context.Context, q string) ([]map[string]any, error) {
		captured = q
		return nil, nil
	}}
	if _, err := store.planRow(context.Background(), "v1:work:run:r1"); err != nil {
		t.Fatalf("planRow: %v", err)
	}
	m := regexp.MustCompile(`^query (\w+)\((\w+):`).FindStringSubmatch(captured)
	if m == nil {
		t.Fatalf("planRow rendered %q, which is not a named query call", captured)
	}
	name, arg := m[1], m[2]

	src, err := fs.ReadFile(dsl.Tree(), "work/queries.memql")
	if err != nil {
		t.Fatalf("reading the shipped work queries: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^query run ` + regexp.QuoteMeta(name) + ` \{`).FindIndex(src)
	if decl == nil {
		t.Fatalf("the workbench reads the run through %q, which dsl/work/queries.memql does not declare over v1:work:run", name)
	}
	body := string(src[decl[1]:])
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}
	if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(arg) + `\s+string!`).MatchString(body) {
		t.Fatalf("%s declares no required %q argument; the workbench passes one", name, arg)
	}
}

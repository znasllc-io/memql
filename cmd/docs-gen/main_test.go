package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// TestRenderConceptCatalog loads the embedded DSL (DB-free) and renders the
// catalog, asserting it is non-empty, carries public front-matter, and
// includes a known concept with a fields table.
func TestRenderConceptCatalog(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.List()
	if len(concepts) == 0 {
		t.Fatal("no concepts loaded from the embedded DSL")
	}

	md := renderConceptCatalog(concepts)

	for _, want := range []string{
		"audience: public",
		"area: reference",
		"# Concept Catalog",
		"| Field | Type | Required | Description |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("catalog missing %q", want)
		}
	}
	// At least one concept heading with the canonical id shape v1:<ns>:<entity>.
	if !strings.Contains(md, "## `v1:") {
		t.Errorf("catalog has no v1: concept headings; got first 200 chars:\n%s", md[:min(200, len(md))])
	}
}

func TestTypeString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"string", "string"},
		{[]any{"string", "null"}, "string \\| null"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := typeString(c.in); got != c.want {
			t.Errorf("typeString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRunDispatchesSubcommands: an unknown or missing subcommand is a usage
// error (exit 2), never a silent fall-through to the catalog.
func TestRunDispatchesSubcommands(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"-out", "x"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != exitUsage {
			t.Errorf("run(%q) = %d, want %d", args, code, exitUsage)
		}
		if !strings.Contains(errOut.String(), "usage: docs-gen") {
			t.Errorf("run(%q) printed no usage: %s", args, errOut.String())
		}
	}
}

// TestBundleRefusesBeforeLoadingAnything: a missing or malformed version is
// refused as a usage error, with the one JSON line the wrapper reads absent.
func TestBundleRefusesBeforeLoadingAnything(t *testing.T) {
	for _, args := range [][]string{{}, {"-version", "1.2.3", "extra"}, {"-bogus"}, {"-version", "v1.2.3"}} {
		var out, errOut bytes.Buffer
		if code := run(append([]string{"bundle"}, args...), &out, &errOut); code != exitUsage {
			t.Errorf("bundle %q = %d, want %d (%s)", args, code, exitUsage, errOut.String())
		}
		if out.Len() != 0 {
			t.Errorf("bundle %q wrote to stdout: %s", args, out.String())
		}
	}
}

// TestBundleCheckOverTheRealTree runs the subcommand as the release lane does
// in its check step: against this repository, allowlist and all, writing
// nothing, and printing one JSON summary line.
func TestBundleCheckOverTheRealTree(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"bundle", "-version", "1.2.3", "-root", "../..", "-check"}, &out, &errOut)
	if code != exitOK {
		t.Fatalf("bundle -check = %d\n%s", code, errOut.String())
	}
	var summary struct {
		Check      bool  `json:"check"`
		Pages      int   `json:"pages"`
		Violations []any `json:"violations"`
	}
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("stdout is not one JSON line: %v\n%s", err, out.String())
	}
	if !summary.Check || summary.Pages == 0 || len(summary.Violations) != 0 {
		t.Errorf("summary = %+v", summary)
	}
}

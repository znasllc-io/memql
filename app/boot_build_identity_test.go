package app

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewAppLogsTheBuildIdentity is the memql#4486 regression.
//
// The value was already in the binary (core/buildinfo, memql#3998) and nothing
// printed it, so "what version is running?" was answered by mapping a live
// image digest back to a registry tag. This asserts the line exists and names a
// version.
func TestNewAppLogsTheBuildIdentity(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	t.Setenv("MEMQL_NODE_TYPE", "bff")
	t.Setenv("MEMQL_NODE_ID", "bff-test-0")

	_ = newApp(log, "ignored", Overrides{})

	var line map[string]any
	var found bool
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		if msg, _ := m["msg"].(string); strings.Contains(msg, "build identity") {
			line, found = m, true
			break
		}
	}
	if !found {
		t.Fatalf("newApp logged no build-identity line. Without it a running cluster cannot state what it is running, "+
			"which is memql#4486 exactly. Got:\n%s", buf.String())
	}

	if v, _ := line["version"].(string); v == "" {
		t.Errorf("build-identity line names no version: %v", line)
	}
	if nt, _ := line["nodeType"].(string); nt != "bff" {
		t.Errorf("build-identity nodeType = %q, want %q", nt, "bff")
	}
	if id, _ := line["nodeId"].(string); id != "bff-test-0" {
		t.Errorf("build-identity nodeId = %q, want %q", id, "bff-test-0")
	}
}

// TestNewAppSurvivesANilLogger pins the guard. newApp is called from ten
// build-tagged constructors and from tests; a nil logger must not panic the
// process on the line whose whole job is to be present.
func TestNewAppSurvivesANilLogger(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("newApp(nil logger) panicked: %v", r)
		}
	}()
	_ = newApp(nil, "v", Overrides{})
}

// TestEveryNodeTypeBuildRoutesThroughNewApp is the gate that matters more than
// the one above, and it is a SOURCE scan on purpose.
//
// Each app/build_<type>.go sits behind its own build tag, so a test binary
// compiled for any one node type cannot even see the other nine. A runtime
// assertion would therefore verify one node type and silently vouch for ten --
// and "a helper the calling node cannot see" is a failure mode this repository
// has shipped before, where a feature ships complete and simply never fires.
//
// Reading the files as TEXT is what makes the claim cover all ten at once. A
// new node type that constructs an App by hand, or a refactor that inlines
// newApp into one constructor, loses its boot line silently -- the node starts,
// serves traffic, and says nothing about itself, which is discovered during an
// incident.
func TestEveryNodeTypeBuildRoutesThroughNewApp(t *testing.T) {
	matches, err := filepath.Glob("build_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("found no app/build_*.go files; this gate is scanning the wrong directory")
	}

	var checked int
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		var build *ast.FuncDecl
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if ok && fn.Name.Name == "Build" && fn.Recv == nil {
				build = fn
			}
			return true
		})
		if build == nil {
			continue
		}
		checked++

		var callsNewApp bool
		ast.Inspect(build, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "newApp" {
				callsNewApp = true
			}
			return true
		})

		if !callsNewApp {
			t.Errorf("%s: Build() does not call newApp.\n"+
				"newApp is where the build-identity boot line is written (memql#4486), and it is there "+
				"BECAUSE it is the one constructor every node type shares. A Build() that assembles an App "+
				"another way starts a node that never states its version or commit -- silently, and green in "+
				"every test. Route this Build() through newApp, or move the boot line somewhere all ten "+
				"constructors provably reach.", path)
		}
	}

	// A count assertion, because the loop above passes vacuously if the glob
	// stops matching -- an empty scan reads exactly like a clean one.
	if checked < 8 {
		t.Errorf("scanned only %d Build() constructors across app/build_*.go, expected the ten node types "+
			"(identity, bff, cognition, agent, planner, voice, workbench, mcp, edge, default). "+
			"A gate that examines nothing reports success about nothing.", checked)
	}
}

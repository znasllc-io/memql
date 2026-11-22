package vendorkeys

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/logger"
)

// log_lines_test.go -- a log line about a key must say WHICH key.
//
// The production log handler replaces the value of every attribute whose NAME
// contains "key" (also "auth", "token", "secret", "pass", "credential"), as a
// backstop against logging a secret by accident. This package is about keys,
// so the natural attribute name, keyId, is one it redacts: every warning here
// printed `keyId=<redacted>`, and "an API key could not be unsealed" did not
// say which. A key's id is not a secret; it is on every call record.

// TestALogLineNamesTheKeyItIsAbout pins the helper through the handler a node
// really logs through.
func TestALogLineNamesTheKeyItIsAbout(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(logger.NewRedactingHandler(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.Warn("vendorkeys: an API key could not be unsealed", keySubject("v1:router:vendorKey:abc123"), "reason", "the request timed out")

	line := out.String()
	if !strings.Contains(line, `"id":"abc123"`) {
		t.Errorf("the line does not name the key by its bare id: %s", line)
	}
	if !strings.Contains(line, keyConcept) {
		t.Errorf("the line does not say what kind of row its subject is: %s", line)
	}
	if strings.Contains(line, logger.RedactedPlaceholder) {
		t.Errorf("part of the line was redacted: %s", line)
	}
}

// TestNoLogLineUsesAnAttributeTheHandlerRedacts reads every log call in this
// package and refuses an attribute name the production handler would blank.
func TestNoLogLineUsesAnAttributeTheHandlerRedacts(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	levels := map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true}
	fset := token.NewFileSet()
	var redacted []string
	calls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !levels[sel.Sel.Name] || len(call.Args) == 0 {
				return true
			}
			// A log call: its receiver ends in a logger.
			receiver := fmt.Sprint(sel.X)
			if inner, isSel := sel.X.(*ast.SelectorExpr); isSel {
				receiver = inner.Sel.Name
			}
			if !strings.Contains(strings.ToLower(receiver), "log") {
				return true
			}
			calls++
			// slog's own reading of its arguments: a string is an attribute's
			// name and takes the next argument as its value; anything else is
			// one attribute by itself.
			for i := 1; i < len(call.Args); {
				lit, isLit := call.Args[i].(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					i++
					continue
				}
				if key, err := strconv.Unquote(lit.Value); err == nil && logger.ShouldRedact(key) {
					redacted = append(redacted, fmt.Sprintf("%s: attribute %q", fset.Position(lit.Pos()), key))
				}
				i += 2
			}
			return true
		})
	}
	if calls < 10 {
		t.Fatalf("found only %d log calls in this package, so the scan is not reading what it should", calls)
	}
	if len(redacted) > 0 {
		t.Errorf("%d log attribute(s) here are blanked by the production log handler, so the line would not say what it is about:\n  %s\n"+
			"Name a key with keySubject(id) (store.go), and give any other attribute a name without key, auth, token, secret, pass or credential in it.",
			len(redacted), strings.Join(redacted, "\n  "))
	}
}

package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

var (
	namedWriteRe    = regexp.MustCompile(`\b(insert|update)\s+[A-Za-z_][A-Za-z0-9_]*\s*\{`)
	cidStringRe     = regexp.MustCompile(`canonicalId\([^,]+,\s*"v1:`)
	concatForeignRe = regexp.MustCompile(`concat\("v1:[A-Za-z0-9:]+:",`)
)

// TestNoRetiredBindingForms is the #988 lock-in for the binding epic (#982):
// the authored tree must use the bare write form + typed foreign-concept refs.
//   - `insert <concept> {` / `update <concept> {`  -> bare `insert {` / `update {`
//   - `canonicalId(x, "v1:ns:name")`               -> `canonicalId(x, <name>)`
//   - `concat("v1:ns:concept:", id)`               -> `canonicalId(id, <name>)`
func TestNoRetiredBindingForms(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
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
			trim := strings.TrimSpace(line)
			// Skip annotation/doc prose so a string literal that mentions a
			// retired form is not flagged.
			if strings.HasPrefix(trim, "@description") || strings.HasPrefix(trim, "@") {
				continue
			}
			ref := func(msg string) {
				t.Errorf("%s:%d retired binding form (%s):\n  %s", p, i+1, msg, trim)
			}
			if namedWriteRe.MatchString(line) {
				ref("`insert <concept> {` / `update <concept> {` is retired -- write the bare `insert {` / `update {` (target comes from the signature)")
			}
			if cidStringRe.MatchString(line) {
				ref("canonicalId concept-string literal is retired -- use the imported short-name `canonicalId(x, <name>)`")
			}
			if concatForeignRe.MatchString(line) {
				ref("`concat(\"v1:ns:concept:\", id)` foreign-id construction is retired -- use `canonicalId(id, <name>)`")
			}
		}
	}
}

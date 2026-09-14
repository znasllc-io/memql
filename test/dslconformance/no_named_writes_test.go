package dslconformance

import (
	"regexp"
	"strings"
	"testing"
)

var (
	namedWriteRe = regexp.MustCompile(`\b(insert|update)\s+[A-Za-z_][A-Za-z0-9_]*\s*\{`)
	bareWriteRe  = regexp.MustCompile(`\b(insert|update)\s*\{`)
	cidStringRe  = regexp.MustCompile(`canonicalId\([^,]+,\s*"v1:`)
	// foreignIdLiteralRe is the literal a retired foreign-id construction
	// starts with: `"v1:ns:concept:" + id`.
	foreignIdLiteralRe = regexp.MustCompile(`^v1:[A-Za-z0-9:]+:$`)
)

// TestNoRetiredBindingForms is the #988 lock-in for the binding epic (#982):
// the authored tree must use the bare write form + typed foreign-concept refs.
//   - `insert <concept> {` / `update <concept> {`  -> bare `insert {` / `update {`
//   - `canonicalId(x, "v1:ns:name")`               -> `canonicalId(x, <name>)`
//   - `"v1:ns:concept:" + id`                      -> `canonicalId(id, <name>)`
//
// Edition 2026 writes the concept name of `canonicalId` as a quoted SHORT name
// (`canonicalId(x, "campaign")`), which cidStringRe does not match: the
// retired form is the full canonical `"v1:..."` string, and that is still the
// one refused.
func TestNoRetiredBindingForms(t *testing.T) {
	onTree(t, func(t *testing.T, c corpus) {
		writes, concats := 0, 0
		for _, p := range c.paths {
			for i, line := range strings.Split(c.files[p], "\n") {
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
				if bareWriteRe.MatchString(line) || namedWriteRe.MatchString(line) {
					writes++
				}
				if namedWriteRe.MatchString(line) {
					ref("`insert <concept> {` / `update <concept> {` is retired -- write the bare `insert {` / `update {` (target comes from the signature)")
				}
				if cidStringRe.MatchString(line) {
					ref("canonicalId concept-string literal is retired -- use the imported short-name `canonicalId(x, <name>)`")
				}
				for _, lit := range concatLiteralPrefixes(line) {
					concats++
					if foreignIdLiteralRe.MatchString(lit) {
						ref("`\"v1:ns:concept:\" + id` foreign-id construction is retired -- use `canonicalId(id, <name>)`")
					}
				}
			}
		}
		// Reachable positives: the write blocks and concatenations this read.
		// Measured when the floors were set: 418 write blocks and 17
		// literal-prefixed concatenations.
		if writes < 300 || concats < 8 {
			t.Errorf("examined %d write blocks and %d concatenations -- the reader has stopped finding what it checks", writes, concats)
		}
		t.Logf("examined %d write blocks and %d concatenations", writes, concats)
	})
}

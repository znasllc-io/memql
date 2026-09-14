package main

import (
	"fmt"
	"regexp"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/baseparser"
)

// attributes.go is the `--rewrite=attributes` codemod for epic memql#5375
// (D17): the retirements, and the losing spelling of every pair that wrote
// one value two ways.
//
// WHY IT IS CONSTRUCT-AWARE. Three of the names it touches are live on one
// receiver and retired on another, so a rewrite that matched on the
// annotation name alone would be wrong in both directions:
//
//   - `@default` on a CONCEPT field was published as the JSON-Schema
//     `default` keyword that nothing applies, and goes. On a TOOL, PROMPT or
//     BUILTIN field it IS the schema default the model reads, and stays. On a
//     PROVIDER it marks the default for a modality, and stays.
//   - `@type` on a PROVIDER becomes `@vendor`. On a CONCEPT it is the row
//     kind and stays `@type`.
//   - `@version` on a function was read by nothing and goes. On a CONCEPT or
//     SEED it is the "v1" of every canonical id and stays.
//
// So the rewrite resolves each line's OWNING CONSTRUCT first (ownerOfLine
// below) and decides per receiver.
//
// The @namespace half is NOT here: it is path-aware, because whether the
// annotation is redundant depends on the directory's namespace.pin. That is
// `--rewrite=attributes-namespace`, which delegates to the engine package's
// own RewriteRedundantNamespace -- the same rewrite the dsl/ conformance gate
// runs, reused rather than reimplemented.

// constructKeywords are the declaration keywords a line may open. `mutate` is
// included because the rewrite's own job is to migrate it, so it must be
// recognised as a construct opener while it still exists in the input.
var constructKeywords = map[string]bool{
	"concept": true, "query": true, "mutation": true, "mutate": true,
	"logic": true, "automation": true, "tool": true, "builtin": true,
	"prompt": true, "provider": true, "spec": true, "trait": true,
	"shape": true, "seed": true, "policy": true, "rule": true,
	"action": true, "capability": true,
}

var (
	declRe = regexp.MustCompile(`^\s*([a-z]+)\s`)
	// annNameRe grabs the annotation identifier from a line that starts one.
	annNameRe = regexp.MustCompile(`^\s*@([A-Za-z_][A-Za-z0-9_]*)`)
	// mutateDeclRe matches a `mutate <Concept> <name> {` header, which is the
	// only position the keyword is migrated in. A bare `mutate` inside a
	// string, a comment or an identifier is left alone.
	mutateDeclRe = regexp.MustCompile(`^(\s*)mutate(\s+[A-Za-z_][A-Za-z0-9_]*\s+[A-Za-z_][A-Za-z0-9_]*\s*\{?)`)
	cacheTTLRe   = regexp.MustCompile(`@cache\(\s*ttl\s*=\s*"?(\d+)"?\s*\)`)
	scheduleRe   = regexp.MustCompile(`@schedule\(\s*cron\s*=\s*("([^"]*)")\s*\)`)
	providerTyRe = regexp.MustCompile(`@type\(`)
	// fieldAnnRe strips a field annotation plus the single space before it,
	// so `email string @unique @description("x")` collapses cleanly rather
	// than leaving a double space for gofmt-less .memql files to keep.
	uniqueRe    = regexp.MustCompile(`\s*@unique\b(\([^)]*\))?`)
	immutableRe = regexp.MustCompile(`\s*@immutable\b(\([^)]*\))?`)
	defaultRe   = regexp.MustCompile(`\s*@default\([^)]*\)`)
)

// retiredWholeLine are annotations whose every occurrence is a header
// annotation on its own line, so the whole line goes. None takes a receiver
// on which it survives.
var retiredWholeLine = map[string]bool{
	"enabled":    true,
	"latestMode": true,
	"deprecated": true,
	"timeout":    true,
	"retry":      true,
	"idempotent": true,
	"audit":      true,
	"rateLimit":  true,
	"scopes":     true,
	"nocache":    false, // rewritten, not stripped
}

// ownerOfLine resolves, for every line, the construct that owns it: the
// enclosing construct for a body line, and the NEXT construct opened at
// depth zero for a header annotation. Header annotations precede their
// keyword, which is why this cannot be a single forward pass with a
// "current construct" variable -- that variable is still holding the
// PREVIOUS construct when the next one's annotations are read, and the
// previous construct is exactly the wrong answer (a provider's @type
// following a concept would be left alone).
func ownerOfLine(scanLines []string) []string {
	owners := make([]string, len(scanLines))
	depth := 0
	// First pass: body lines get their enclosing construct; record where each
	// depth-zero declaration starts.
	enclosing := ""
	declAt := make([]string, len(scanLines))
	for i, l := range scanLines {
		if depth == 0 {
			if m := declRe.FindStringSubmatch(l); m != nil && constructKeywords[m[1]] {
				enclosing = m[1]
				declAt[i] = m[1]
			}
		}
		owners[i] = enclosing
		depth += strings.Count(l, "{") - strings.Count(l, "}")
		if depth < 0 {
			depth = 0
		}
		if depth == 0 && declAt[i] == "" && enclosing != "" {
			// The construct just closed; a following header annotation
			// belongs to the NEXT one, resolved in the back pass.
			enclosing = ""
		}
	}
	// Back pass: a depth-zero header annotation with no enclosing construct
	// adopts the next declaration below it.
	next := ""
	for i := len(scanLines) - 1; i >= 0; i-- {
		if declAt[i] != "" {
			next = declAt[i]
		}
		if owners[i] == "" {
			owners[i] = next
		}
	}
	return owners
}

// rewriteAttributes applies the construct-local half of the attribute
// cleanup. Decisions are taken against a COMMENT-BLANKED view of the source
// and the edits are applied to the original bytes: BlankComments preserves
// byte offsets and newlines, so the two views are positionally identical and
// only comment CONTENT differs. Without that, a prose mention of `@enabled`
// in a doc comment -- of which this repo has plenty -- would be rewritten as
// if it were an annotation.
func rewriteAttributes(src []byte) ([]byte, error) {
	text := string(src)
	scan := baseparser.BlankComments(text)
	origLines := strings.Split(text, "\n")
	scanLines := strings.Split(scan, "\n")
	if len(origLines) != len(scanLines) {
		return nil, fmt.Errorf("comment-blanked view has %d lines against the source's %d -- the two must stay positionally identical for this rewrite to be safe", len(scanLines), len(origLines))
	}
	owners := ownerOfLine(scanLines)

	out := make([]string, 0, len(origLines))
	for i, orig := range origLines {
		s := scanLines[i]
		owner := owners[i]

		// --- whole-line header retirements ---
		if m := annNameRe.FindStringSubmatch(s); m != nil {
			name := m[1]
			if retiredWholeLine[name] {
				continue // drop the line
			}
			// @version is dead on a function and id-bearing on a concept or
			// a seed, so the receiver decides.
			if name == "version" && owner != "concept" && owner != "seed" && owner != "" {
				continue
			}
			// @nocache becomes the one cache spelling.
			if name == "nocache" {
				out = append(out, strings.Replace(orig, "@nocache", "@cache(0)", 1))
				continue
			}
			// @schedule(cron="X") becomes @trigger(schedule="X").
			if name == "schedule" {
				if sm := scheduleRe.FindStringSubmatch(s); sm != nil {
					out = append(out, scheduleRe.ReplaceAllString(orig, `@trigger(schedule=$1)`))
					continue
				}
			}
			// A provider's @type becomes @vendor; a concept's @type is the
			// row kind and is left exactly as written.
			if name == "type" && owner == "provider" {
				out = append(out, providerTyRe.ReplaceAllString(orig, "@vendor("))
				continue
			}
		}

		line := orig

		// --- @cache(ttl="N") becomes @cache(N), anywhere it appears ---
		if cacheTTLRe.MatchString(s) {
			line = cacheTTLRe.ReplaceAllString(line, `@cache($1)`)
		}

		// --- field annotations, inside a concept body only ---
		if owner == "concept" {
			line = uniqueRe.ReplaceAllString(line, "")
			line = immutableRe.ReplaceAllString(line, "")
			// @default on a concept FIELD goes; the annotation never reaches
			// a concept header, so no receiver check is needed beyond this.
			line = defaultRe.ReplaceAllString(line, "")
		}

		// --- the one mutation keyword ---
		if mutateDeclRe.MatchString(s) {
			line = mutateDeclRe.ReplaceAllString(line, "${1}mutation${2}")
		}

		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// rewriteAttributesNamespace is the path-aware half: whether a @namespace
// annotation is redundant depends on the directory it sits in (its
// namespace.pin, else the domain directory), so it cannot be decided from
// the file's own bytes. Delegates to the engine package's rewrite -- the
// same one `--rewrite=namespace-default` runs and the dsl/ conformance gate
// checks, reused rather than reimplemented, so the two can never disagree
// about which annotations are safe to drop.
func rewriteAttributesNamespace(path string, src []byte) ([]byte, error) {
	return langparser.RewriteRedundantNamespace(domainForDSLPath(path), src)
}

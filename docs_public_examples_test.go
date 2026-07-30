package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestPublicExamplesAreAnnotated is the recurrence guard for memql#2918.
//
// That issue was not "two doc bullets are wrong". It was that
// docs/public/operate/auth/per-row-authz-audit.md — the reference for
// this repo's authorization model — listed two constructs as examples
// of legitimate `@public` use, NEITHER of which carried the annotation,
// and nothing on our side noticed. One of the two named no construct at
// all. A reader auditing the public read surface, or reasoning about
// whether the role catalog is exposed, would have taken the list as
// ground truth.
//
// Correcting the list once does not stop that recurring; the list is
// hand-maintained prose about annotations that live somewhere else. So
// the list is now checked: every backticked construct named under
// "Examples of legitimate `@public` use:" must be declared in the DSL
// tree AND carry `@public` in its preamble.
//
// # WHY THIS IS NARROW, AND DELIBERATELY SO
//
// It polices exactly one bullet list. It does not try to verify the
// document's other claims — the bucket table's prescribed spellings,
// the per-domain snapshot counts, the Validator section's description
// of its own enforcement posture — all of which have also drifted and
// are tracked in memql#2983. A gate that tried to check prose in
// general would either be unmaintainable or would have to be weakened
// until it caught nothing.
//
// The trade is that this is thin over a two-item list today. That is
// the price of a recurrence guard: the alternative is re-filing #2918
// in six months, which is what happened to the "tracked as a doc
// follow-up" note in extension-points.md (memql#2922 filed it 74 days
// later).
//
// # THE BULLET FORM IT EXPECTS
//
// A bullet naming a construct writes it in backticks, optionally
// followed by a parenthesised file hint:
//
//   - `activeAgentRoles` (`dsl/agents/queries.memql`) — the agentRole catalog: ...
//
// A bullet that names a FILE rather than a construct — "the catalog
// reads in `dsl/rbac/queries.memql`" — is a collective reference and is
// checked differently: that file must contain at least one `@public`.
// Both forms appear in the list and both are load-bearing, so the gate
// understands both rather than forcing one shape on the prose.
func TestPublicExamplesAreAnnotated(t *testing.T) {
	const doc = "docs/public/operate/auth/per-row-authz-audit.md"

	data, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	content := string(data)

	// Anchored on the stable PREFIX, not the full line: the sentence
	// carries a parenthetical naming this very test, and matching the
	// whole line would make the gate fail the moment someone reworded
	// its own citation.
	const heading = "Examples of legitimate `@public` use"
	start := strings.Index(content, heading)
	if start < 0 {
		t.Fatalf("%s no longer contains %q -- either the section was renamed (re-aim this gate) "+
			"or the example list was deleted (then delete this gate, deliberately)", doc, heading)
	}
	// Skip to the end of the heading line so the parenthetical does
	// not get parsed as list content.
	rest := content[start+len(heading):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	list := publicExampleList(rest)
	if strings.TrimSpace(list) == "" {
		t.Fatalf("%s: the %q list is empty; a gate over nothing passes for the wrong reason", doc, heading)
	}

	declared := publicAnnotatedConstructs(t)

	// A construct reference: a backticked identifier. A file reference:
	// a backticked path ending in .memql.
	tickRe := regexp.MustCompile("`([^`]+)`")

	checkedConstruct, checkedFile := 0, 0
	for _, line := range strings.Split(list, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		var namedConstruct bool
		for _, m := range tickRe.FindAllStringSubmatch(trimmed, -1) {
			tok := strings.TrimSpace(m[1])
			switch {
			case strings.HasSuffix(tok, ".memql"):
				// Collective reference: the file must carry @public.
				checkedFile++
				if !fileHasPublic(t, tok) {
					t.Errorf("%s: the %q list names file %q, but that file contains no `@public` "+
						"construct. Either annotate one or drop the bullet -- a doc asserting an "+
						"annotation the tree does not carry is memql#2918 recurring.",
						doc, heading, tok)
				}
			case isConstructIdent(tok):
				namedConstruct = true
				checkedConstruct++
				origin, ok := declared[tok]
				if !ok {
					t.Errorf("%s: the %q list names `%s`, which is either not declared in dsl/ or "+
						"does not carry `@public`. That is exactly the defect memql#2918 fixed: the "+
						"list read as an inventory of annotated constructs while naming things that "+
						"were not annotated (and, in one case, did not exist).",
						doc, heading, tok)
					continue
				}
				_ = origin
			}
		}
		_ = namedConstruct
	}

	if checkedConstruct+checkedFile == 0 {
		t.Errorf("%s: parsed the %q list but recognised no construct or file reference in it. "+
			"The bullet form probably changed; re-aim this gate rather than deleting it.", doc, heading)
	}
}

// publicExampleList returns the bullet list following the heading.
//
// A bullet WRAPS: its continuation lines are indented and are neither
// blank nor bullet-led. Treating one of those as the end of the list
// stopped this gate after the first bullet's first line, which left it
// green while checking almost nothing -- it caught a file reference on
// line one and never reached the construct reference on the next
// bullet. So the list ends only at a heading, or at a non-indented line
// that is not a bullet.
func publicExampleList(after string) string {
	var b strings.Builder
	started := false
	for _, line := range strings.Split(after, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			break
		}
		isBullet := strings.HasPrefix(trimmed, "-")
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')
		switch {
		case isBullet:
			started = true
			b.WriteString(line)
			b.WriteByte('\n')
		case started && indented && trimmed != "":
			// Continuation of the current bullet.
			b.WriteString(line)
			b.WriteByte('\n')
		case started && trimmed == "":
			// A blank line may separate bullets; keep going and let a
			// following non-indented prose line end the list.
		case started:
			// Non-indented prose resumed: the list is over.
			return b.String()
		}
	}
	return b.String()
}

// isConstructIdent reports whether a backticked token looks like a
// construct name rather than a path, an annotation, or a code snippet.
func isConstructIdent(tok string) bool {
	if tok == "" || strings.ContainsAny(tok, " /.@(){}\"=") {
		return false
	}
	for i, r := range tok {
		if i == 0 && !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// publicAnnotatedConstructs maps every construct carrying `@public` in
// its preamble to the file:line declaring it.
//
// The preamble scan walks back over the contiguous run of annotation
// and doc-comment lines, and `@public` is matched LINE-ANCHORED: a
// comment merely mentioning the annotation is prose, and treating it as
// the annotation is how a gate gets silenced by a sentence
// (memql#2875).
func publicAnnotatedConstructs(t *testing.T) map[string]string {
	t.Helper()

	header := regexp.MustCompile(
		`(?m)^(query|mutate|logic)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`,
	)
	publicLine := regexp.MustCompile(`(?m)^[ \t]*@public\b`)

	out := map[string]string{}
	err := filepath.WalkDir("dsl", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".memql") ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(data)
		for _, m := range header.FindAllStringSubmatchIndex(src, -1) {
			preamble := src[preambleStartOf(src, m[0]):m[0]]
			if !publicLine.MatchString(preamble) {
				continue
			}
			construct := src[m[4]:m[5]]
			if _, seen := out[construct]; !seen {
				out[construct] = path + ":" + strconv.Itoa(1+strings.Count(src[:m[0]], "\n"))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk dsl: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no @public construct found under dsl/ -- the annotation sweep is broken, not the doc")
	}
	return out
}

// preambleStartOf walks back from a construct header over the
// contiguous run of annotation and comment lines belonging to it.
func preambleStartOf(src string, headerStart int) int {
	lineStart := headerStart
	for lineStart > 0 {
		prevEnd := lineStart - 1
		ps := strings.LastIndexByte(src[:prevEnd], '\n') + 1
		trimmed := strings.TrimSpace(src[ps:prevEnd])
		if trimmed == "" || (!strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "//")) {
			break
		}
		lineStart = ps
	}
	return lineStart
}

// fileHasPublic reports whether a .memql file contains at least one
// line-anchored @public.
func fileHasPublic(t *testing.T, rel string) bool {
	t.Helper()
	data, err := os.ReadFile(rel)
	if err != nil {
		return false
	}
	return regexp.MustCompile(`(?m)^[ \t]*@public\b`).Match(data)
}

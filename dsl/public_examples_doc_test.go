package dsl

import (
	"os"
	"regexp"
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// public_examples_doc_test.go -- memql#2918.
//
// docs/public/operate/auth/per-row-authz-audit.md lists constructs as
// examples of legitimate `@public` use. It listed two that did not
// carry the annotation, and one of those named no construct at all --
// and nothing on our side noticed. A reader auditing the public read
// surface would have taken a false list as ground truth.
//
// Correcting a list once does not stop it drifting: the list is
// hand-maintained prose about annotations that live somewhere else.
// (The "tracked as a doc follow-up" note in extension-points.md took 74
// days and memql#2922 to come back.) So the list is checked.
//
// # WHY THIS LIVES IN package dsl AND NOT AT THE REPO ROOT
//
// Because the verdict must come from the PARSED tree, which is
// memql#2875's ruling and the reason serverOnlyConstructs exists in
// this package. A preamble regex is fail-open in exactly the direction
// that matters here: `@public` inside a multi-line annotation string or
// a block comment opened on an `@`-line satisfies the regex while the
// loader sets no attribute, so the gate would report the doc's claim as
// verified when the annotation does not exist. This shares
// declNameAndAttributes with the @serverOnly gate for that reason.
//
// # WHAT IT CHECKS, AND THE BULLET SHAPE IT ASSUMES
//
// Each bullet's SUBJECT is its first backticked token. Everything after
// is supporting prose and is not treated as a claim -- naming a field
// (`predefined`) or a shape (`agentRoleFull`) mid-sentence must not
// accuse the author of citing an unannotated construct.
//
//   - `activeAgentRoles` (`dsl/agents/queries.memql`) -- the agentRole
//     catalog: no owner field, ...
//
// A subject ending in `.memql` is a COLLECTIVE reference ("the catalog
// reads in `dsl/rbac/queries.memql`"); that file must contain at least
// one genuinely-annotated `@public` construct. Otherwise the subject is
// a construct name: it must be declared with `@public`, and if the
// bullet also names a `.memql` path, the construct must be declared IN
// that file -- a bullet's real assertion is "this construct, in this
// file", and checking the halves independently let a mis-attributed
// pair pass.
//
// Bullets are joined across their wrapped continuation lines before the
// subject is taken. An earlier version scanned only each bullet's first
// physical line: the doc wraps at ~72 columns, so a few added words
// would have pushed the construct name onto line 2 and silently retired
// the only construct-level check in the gate.
func TestPublicExamplesAreAnnotated(t *testing.T) {
	const doc = "../docs/public/operate/auth/per-row-authz-audit.md"

	data, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	content := string(data)

	// Anchored on the stable prefix: the sentence carries a
	// parenthetical naming this very test, and matching the whole line
	// would break the gate the moment someone reworded its own citation.
	const heading = "Examples of legitimate `@public` use"
	start := strings.Index(content, heading)
	if start < 0 {
		t.Fatalf("%s no longer contains %q -- either the section was renamed (re-aim this gate) "+
			"or the example list was deleted (then delete this gate deliberately, in a commit "+
			"that says why)", doc, heading)
	}
	rest := content[start+len(heading):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}

	bullets := publicExampleBullets(rest)
	if len(bullets) == 0 {
		t.Fatalf("%s: parsed no bullets under %q. A gate over an empty list passes for the wrong "+
			"reason -- the bullet form probably changed; re-aim this gate rather than deleting it.",
			doc, heading)
	}

	annotated, byFile := publicAnnotatedConstructs(t)

	for _, b := range bullets {
		subject, file := bulletSubjectAndFile(b)
		if subject == "" {
			t.Errorf("%s: bullet %q names no backticked subject, so it asserts nothing checkable. "+
				"Name the construct (or the .memql file) in backticks, or drop the bullet.",
				doc, firstLine(b))
			continue
		}

		if strings.HasSuffix(subject, ".memql") {
			if !byFile[subject] {
				t.Errorf("%s: the %q list names file %q, but that file declares no `@public` "+
					"construct. Either annotate one or drop the bullet -- a doc asserting an "+
					"annotation the tree does not carry is memql#2918 recurring.",
					doc, heading, subject)
			}
			continue
		}

		origin, ok := annotated[subject]
		if !ok {
			t.Errorf("%s: the %q list names `%s`, which is either not declared in dsl/ or does "+
				"not carry `@public` as a real annotation. That is exactly the defect memql#2918 "+
				"fixed: the list read as an inventory of annotated constructs while naming things "+
				"that were not annotated (and, in one case, did not exist).",
				doc, heading, subject)
			continue
		}
		// The bullet's real assertion is "this construct, in this file".
		// Tree paths are dsl/-relative ("agents/queries.memql"); the doc
		// writes them repo-relative. Normalise before comparing.
		if file != "" && strings.TrimPrefix(file, "dsl/") != origin {
			t.Errorf("%s: the %q list attributes `%s` to %q, but it is declared in %q. "+
				"Mis-attribution is half the original defect -- fix the path or the name.",
				doc, heading, subject, file, origin)
		}
	}
}

// publicExampleBullets returns each bullet under the heading as ONE
// string with its wrapped continuation lines joined.
//
// The list ends at the next markdown heading, or at a non-indented line
// that is neither a bullet nor blank. A `---` horizontal rule ends it
// too: it is not a bullet, and treating it as one kept the list alive
// across unrelated prose.
func publicExampleBullets(after string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	started := false
	for _, line := range strings.Split(after, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || isHorizontalRule(trimmed) {
			break
		}
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')
		isBullet := strings.HasPrefix(trimmed, "- ") && !indented

		switch {
		case isBullet:
			flush()
			started = true
			cur.WriteString(trimmed)
		case started && indented && trimmed != "":
			cur.WriteByte(' ')
			cur.WriteString(trimmed)
		case started && trimmed == "":
			// May separate bullets; a following non-indented prose line ends it.
		case started:
			flush()
			return out
		}
	}
	flush()
	return out
}

// isHorizontalRule reports whether a line is a markdown `---` rule,
// which shares a leading `-` with a bullet but is not one.
func isHorizontalRule(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	for _, r := range trimmed {
		if r != '-' {
			return false
		}
	}
	return true
}

var docTickRe = regexp.MustCompile("`([^`]+)`")

// bulletSubjectAndFile returns a bullet's subject (its first backticked
// token) and the first `.memql` path it names, if any.
//
// Only the FIRST token is a claim. A bullet naturally goes on to name a
// field or a shape, and treating those as claims produced a false
// accusation of citing an unannotated construct.
func bulletSubjectAndFile(bullet string) (subject, file string) {
	for _, m := range docTickRe.FindAllStringSubmatch(bullet, -1) {
		tok := strings.TrimSpace(m[1])
		if tok == "" {
			continue
		}
		if subject == "" && (isDocConstructIdent(tok) || strings.HasSuffix(tok, ".memql")) {
			subject = tok
		}
		if file == "" && strings.HasSuffix(tok, ".memql") {
			file = tok
		}
		if subject != "" && file != "" {
			break
		}
	}
	return subject, file
}

// isDocConstructIdent reports whether a backticked token has the shape
// of a construct name.
func isDocConstructIdent(tok string) bool {
	if tok == "" || strings.ContainsAny(tok, " /.@(){}\"=`") {
		return false
	}
	for i, r := range tok {
		isAlpha := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 90 {
		return s[:90] + "..."
	}
	return s
}

// publicAnnotatedConstructs returns every construct carrying a REAL
// `@public` attribute, mapped to the tree path declaring it, plus the
// set of files declaring at least one.
//
// Derived from the PARSED tree via the same rule the loader applies
// (presence of the attribute NAME), so an `@public` inside a string
// literal or a comment never appears here. That is memql#2875's whole
// point, and the reason this is not a regex.
func publicAnnotatedConstructs(t *testing.T) (map[string]string, map[string]bool) {
	t.Helper()

	tree, err := dslimports.Load(Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v -- the tree did not parse, so the @public set cannot be "+
			"derived. Fix the parse failure; do not read this as 'no construct is annotated'.", err)
	}

	byName := map[string]string{}
	byFile := map[string]bool{}
	for path, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			name, attrs, ok := declNameAndAttributes(def)
			if !ok {
				continue
			}
			for _, a := range attrs {
				if a == nil || a.Name != languageAst.AttrPublic {
					continue
				}
				if _, seen := byName[name]; !seen {
					byName[name] = path
				}
				byFile["dsl/"+path] = true
				byFile[path] = true
			}
		}
	}
	if len(byName) == 0 {
		t.Fatal("no @public construct found in the parsed tree -- the annotation sweep is broken, " +
			"not the doc")
	}
	return byName, byFile
}

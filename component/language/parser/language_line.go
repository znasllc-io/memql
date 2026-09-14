package parser

// language_line.go -- which language each domain of a tree is written in
// (epic memql#5356, task memql#5357; D4 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A domain declares its line in <domain>/memql.toml (core/dslfs reads the
// file); this file decides what the declaration MEANS to this engine: which
// front end its files are read with, and whether the engine reads it at all.
// It is the language's rule, so it lives beside LanguageVersion, Edition and
// the front-end table rather than in any one loader -- every loader, in every
// module, resolves a tree through ResolveLanguageLines and reads a file
// through the result.
//
// WHY THE EMBEDDED TREE IS AN ARGUMENT. Which domains are compiled into the
// engine, and the one line they speak (dsl/memql.toml), are facts of package
// dsl, which sits beside this module rather than below it. CoreTree names
// exactly those two facts, and dsl.EmbeddedTree answers them without either
// module importing the other.

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/core/dslfs"
)

// The refusal codes. Every LanguageLineProblem.Message ends with " [<code>]".
const (
	// CodeLanguageLineMissing: a domain that is not compiled into the engine
	// carries no <domain>/memql.toml.
	CodeLanguageLineMissing = "language_line_missing"
	// CodeLanguageLineMalformed: the file does not parse, lacks one of its two
	// keys, or declares a version that is not <major>.<minor>.
	CodeLanguageLineMalformed = "language_line_malformed"
	// CodeLanguageVersionNewer: the tree was written for a newer engine.
	CodeLanguageVersionNewer = "language_version_newer"
	// CodeLanguageVersionUnsupported: the tree declares an older line, which
	// this engine does not read (it reads only LanguageVersion).
	CodeLanguageVersionUnsupported = "language_version_unsupported"
	// CodeEditionUnknown: no front end for the declared edition.
	CodeEditionUnknown = "edition_unknown"
)

// CoreTree is the tree compiled into the engine, as a resolver needs it.
// Package dsl implements it (dsl.EmbeddedTree); a test passes a fake.
type CoreTree interface {
	// IsCoreDomain reports whether domain is compiled into the engine. A core
	// domain speaks the embedded manifest and carries no file of its own.
	IsCoreDomain(domain string) bool
	// EmbeddedManifest is the one declaration every core domain speaks.
	EmbeddedManifest() (dslfs.Manifest, error)
	// EmbeddedManifestPath names that declaration in messages: dsl/memql.toml.
	EmbeddedManifestPath() string
}

// LanguageLine is the declaration governing one domain of a tree.
type LanguageLine struct {
	// Domain is the first path segment of the tree.
	Domain string
	// Source is the file the line is read from: the embedded dsl/memql.toml
	// for a core domain, <domain>/memql.toml otherwise.
	Source string
	// Language is the declared `memql` line; Edition the declared edition.
	// A domain the engine refuses carries the engine's own line and edition
	// instead, because that is what its files are read with.
	Language string
	Edition  string
	// Embedded is true for a domain compiled into the engine.
	Embedded bool
}

// LanguageLineProblem is one declaration the engine will not read.
type LanguageLineProblem struct {
	Domain, Source string
	// Code is one of the Code* constants.
	Code string
	// Message says what is wrong and what to write instead, and ends with
	// " [<Code>]".
	Message string
}

// LanguageLines is every resolved domain of one tree, by domain.
type LanguageLines map[string]LanguageLine

// languageVersionPattern is the one shape a language line has.
var languageVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// ResolveLanguageLines reads the declaration of every domain of tree that
// holds a .memql file to read (as dslfs.WalkMemqlFiles walks it).
//
// It never fails outright. A domain the engine refuses is returned in the
// lines too, carrying the engine's own line and edition, so its files still
// parse and one boot reports every problem the tree has rather than the first.
// A tree that cannot be walked resolves to nothing; every loader walking it
// reports the walk failure itself.
func ResolveLanguageLines(tree fs.FS, core CoreTree) (LanguageLines, []LanguageLineProblem) {
	lines := LanguageLines{}
	if tree == nil {
		return lines, nil
	}
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return lines, nil
	}
	seen := map[string]bool{}
	var domains []string
	for _, p := range paths {
		if d := lineDomain(p); d != "" && !seen[d] {
			seen[d] = true
			domains = append(domains, d)
		}
	}
	sort.Strings(domains)

	var problems []LanguageLineProblem
	for _, d := range domains {
		line, found := resolveDomainLine(tree, core, d)
		if len(found) > 0 {
			line.Language, line.Edition = LanguageVersion, Edition
		}
		lines[d] = line
		problems = append(problems, found...)
	}
	return lines, problems
}

// For returns the line governing a file of the tree the lines were resolved
// from. path is tree-relative ("<domain>/queries.memql"); a loader origin
// ("unified:<path>:<name>") is accepted too.
func (l LanguageLines) For(path string) (LanguageLine, bool) {
	d := lineDomain(path)
	if d == "" {
		return LanguageLine{}, false
	}
	line, ok := l[d]
	return line, ok
}

// Prepare reads one file of the tree through the front end of the edition its
// domain declares (task memql#5358), before the struct-form rewriter or any
// other reader sees it. This is the step that lets two editions load in one
// engine: every loader that reads a tree file hands it here first, so the rest
// of the pipeline only ever sees the core grammar.
//
// A file in no resolved domain -- one at the root of a tree, or a path of
// another tree -- is read with this engine's own edition, which is also what a
// domain the engine refused carries (ResolveLanguageLines).
//
// An error means the file must not be parsed at all: read under the core
// grammar, text written for another edition could parse into something else.
func (l LanguageLines) Prepare(path string, src []byte) ([]byte, error) {
	edition := Edition
	if line, ok := l.For(path); ok {
		edition = line.Edition
	}
	fe, err := FrontEndFor(edition)
	if err != nil {
		return nil, err
	}
	out, err := fe.Prepare(string(src))
	if err != nil {
		return nil, fmt.Errorf("the edition %s front end refused this file: %w", edition, err)
	}
	return []byte(out), nil
}

// lineDomain is the domain a tree path belongs to: its first segment, after
// any loader origin prefix. A file at the root belongs to no domain.
func lineDomain(p string) string {
	if i := strings.IndexByte(p, ':'); i >= 0 && !strings.Contains(p[:i], "/") && strings.Contains(p[i+1:], "/") {
		p = p[i+1:]
	}
	if i := strings.IndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return ""
}

// resolveDomainLine reads one domain's declaration and checks it.
func resolveDomainLine(tree fs.FS, core CoreTree, domain string) (LanguageLine, []LanguageLineProblem) {
	if core != nil && core.IsCoreDomain(domain) {
		line := LanguageLine{Domain: domain, Source: core.EmbeddedManifestPath(), Embedded: true}
		m, err := core.EmbeddedManifest()
		if err != nil {
			return line, []LanguageLineProblem{malformed(domain, line.Source, err)}
		}
		return checkLine(line, m)
	}

	line := LanguageLine{Domain: domain, Source: domain + "/" + dslfs.ManifestFile}
	data, err := fs.ReadFile(tree, line.Source)
	if errors.Is(err, fs.ErrNotExist) {
		current := dslfs.Manifest{Language: LanguageVersion, Edition: Edition}
		return line, []LanguageLineProblem{{
			Domain: domain, Source: line.Source, Code: CodeLanguageLineMissing,
			Message: fmt.Sprintf("domain %q declares no language line: add %s containing\n%s(or run: memqlmigrate --rewrite=language-line <tree>) [%s]",
				domain, line.Source, indentLines(current.Render(), "  "), CodeLanguageLineMissing),
		}}
	}
	if err != nil {
		return line, []LanguageLineProblem{malformed(domain, line.Source, err)}
	}
	m, err := dslfs.ParseManifest(data)
	if err != nil {
		return line, []LanguageLineProblem{malformed(domain, line.Source, err)}
	}
	return checkLine(line, m)
}

// checkLine holds a declaration against this engine. Both keys are checked,
// so a domain wrong in both says so once.
func checkLine(line LanguageLine, m dslfs.Manifest) (LanguageLine, []LanguageLineProblem) {
	var problems []LanguageLineProblem
	add := func(code, format string, args ...any) {
		problems = append(problems, LanguageLineProblem{
			Domain: line.Domain, Source: line.Source, Code: code,
			Message: fmt.Sprintf(format, args...) + " [" + code + "]",
		})
	}

	switch {
	case m.Language == "":
		add(CodeLanguageLineMalformed, "domain %q declares no memql version in %s: add the line memql = %q",
			line.Domain, line.Source, LanguageVersion)
	case !languageVersionPattern.MatchString(m.Language):
		add(CodeLanguageLineMalformed, "domain %q declares memql = %q in %s, which is not a language version: write <major>.<minor>, as in memql = %q",
			line.Domain, m.Language, line.Source, LanguageVersion)
	default:
		switch cmp, ok := compareLanguageVersions(m.Language, LanguageVersion); {
		case !ok:
			add(CodeLanguageLineMalformed, "domain %q declares memql = %q in %s, which is not a language version: write <major>.<minor>, as in memql = %q",
				line.Domain, m.Language, line.Source, LanguageVersion)
		case cmp > 0:
			add(CodeLanguageVersionNewer, "domain %q declares memql = %q, newer than the %s this engine speaks: run an engine that speaks %s, or declare memql = %q in %s",
				line.Domain, m.Language, LanguageVersion, m.Language, LanguageVersion, line.Source)
		case cmp < 0:
			add(CodeLanguageVersionUnsupported, "domain %q declares memql = %q, which this engine does not read (it reads only %s): declare memql = %q in %s",
				line.Domain, m.Language, LanguageVersion, LanguageVersion, line.Source)
		}
	}

	switch {
	case m.Edition == "":
		add(CodeLanguageLineMalformed, "domain %q declares no edition in %s: add the line edition = %q",
			line.Domain, line.Source, Edition)
	default:
		if _, err := FrontEndFor(m.Edition); err != nil {
			add(CodeEditionUnknown, "domain %q declares edition = %q, which this engine does not read (it reads: %s)",
				line.Domain, m.Edition, strings.Join(Editions(), ", "))
		}
	}

	line.Language, line.Edition = m.Language, m.Edition
	return line, problems
}

// malformed is the refusal for a declaration that cannot be read at all.
func malformed(domain, source string, err error) LanguageLineProblem {
	detail := err.Error()
	var me *dslfs.ManifestError
	if errors.As(err, &me) {
		detail = fmt.Sprintf("line %d: %s", me.Line, me.Msg)
	}
	return LanguageLineProblem{
		Domain: domain, Source: source, Code: CodeLanguageLineMalformed,
		Message: fmt.Sprintf("domain %q declares its language line in %s, which does not read: %s [%s]",
			domain, source, detail, CodeLanguageLineMalformed),
	}
}

// compareLanguageVersions compares two <major>.<minor> lines numerically, so
// 1.10 is newer than 1.9. ok is false when either does not parse.
func compareLanguageVersions(a, b string) (cmp int, ok bool) {
	am, an, aok := splitLanguageVersion(a)
	bm, bn, bok := splitLanguageVersion(b)
	if !aok || !bok {
		return 0, false
	}
	switch {
	case am != bm:
		if am > bm {
			return 1, true
		}
		return -1, true
	case an != bn:
		if an > bn {
			return 1, true
		}
		return -1, true
	}
	return 0, true
}

func splitLanguageVersion(v string) (major, minor int, ok bool) {
	head, tail, found := strings.Cut(v, ".")
	if !found {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(head)
	minor, err2 := strconv.Atoi(tail)
	return major, minor, err1 == nil && err2 == nil
}

// indentLines prefixes every line of text, which ends with a newline.
func indentLines(text, prefix string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(text, "\n") {
		if l != "" {
			b.WriteString(prefix + l)
		}
	}
	return b.String()
}

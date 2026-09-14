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
	// Either is empty when the declaration does not say, or cannot be read.
	Language string
	Edition  string
	// Embedded is true for a domain compiled into the engine.
	Embedded bool
	// Refused is true when the engine will not read this domain: its line is
	// missing, malformed, newer or older than the engine's, or names an
	// edition with no front end. No loader reads a file of a refused domain
	// (Prepare answers ErrLanguageLineRefused), so its author sees the one
	// refusal, and nothing that reading the domain under a line it did not
	// declare would have produced.
	Refused bool
}

// ErrLanguageLineRefused is Prepare's answer for a file of a Refused domain.
// A loader skips such a file WITHOUT a word of its own: the refusal is
// reported once per domain, from the resolver's problems, and a per-file echo
// of it is exactly the cascade a refused domain must not produce.
var ErrLanguageLineRefused = errors.New("the domain's language line is refused, so no file of it is read")

// LanguageLineDomainOf is the one rule for which files of a tree make a
// domain that must declare a language line: the first segment of a .memql
// file some loader reads, however deep beneath it the file sits. It answers ""
// for a file no loader reads -- one at the root, one under a `_`-prefixed
// directory or with a `_`-prefixed name at any depth (dslfs.WalkMemqlFiles
// skips them), one under a top-level `.`-prefixed directory (no mount mounts
// one) -- and for anything that is not a .memql file.
//
// It also takes a loader origin -- "<kind>:<path>", which the functions
// loader puts on every query, mutate and logic ("unified:shop/queries.memql"),
// or "<kind>:<path>:<name>" ("unified:shop/queries.memql:list") -- and
// answers for the path inside it, which is how LanguageLines.For, and so
// MemQLEngine.LanguageLineFor, is asked about a construct.
//
// The resolver, LanguageLines.For and memqlmigrate --rewrite=language-line
// all key on it, and the migrator skips a domain the embedded tree owns as
// the resolver does, so it writes exactly the lines the loader asks for.
func LanguageLineDomainOf(path string) string {
	if inner, ok := loaderOriginPath(path); ok {
		path = inner
	}
	if !strings.HasSuffix(path, ".memql") {
		return ""
	}
	segments := strings.Split(path, "/")
	if len(segments) < 2 || !LanguageLineDomainName(segments[0]) {
		return ""
	}
	for _, seg := range segments {
		if seg == "" || strings.HasPrefix(seg, "_") {
			return ""
		}
	}
	return segments[0]
}

// LanguageLineDomainName reports whether a directory named name is one a
// mount reads as a domain: not empty, not `_`-prefixed (soft-disabled) and
// not `.`-prefixed (hidden). It is LanguageLineDomainOf's rule for a path's
// first segment.
func LanguageLineDomainName(name string) bool {
	return name != "" && !strings.Contains(name, "/") &&
		!strings.HasPrefix(name, "_") && !strings.HasPrefix(name, ".")
}

// LanguageLineRootIsDomain reports whether the root of a tree is itself one
// domain directory: it directly holds a .memql file a loader reads. paths are
// the tree's files, root-relative.
//
// memqlmigrate and memqllint are handed such a root when an author points
// them at one domain (bundle/<domain>) rather than at a bundle, and both then
// read it the way a node receives it: as the domain its directory is named
// for, whose memql.toml sits at the root. Read as a bare tree instead, its
// files sit at depth 1, where LanguageLineDomainOf finds no domain at all.
func LanguageLineRootIsDomain(paths []string) bool {
	for _, p := range paths {
		if !strings.Contains(p, "/") && strings.HasSuffix(p, ".memql") && !strings.HasPrefix(p, "_") {
			return true
		}
	}
	return false
}

// loaderOriginPath returns the tree path inside a loader origin -- a
// "<kind>:" prefix whose left side has no `/`, then a .memql path, then
// optionally ":<name>" -- and false for anything else, so a plain tree path,
// which has no such prefix, is read exactly as the walkers read it.
func loaderOriginPath(s string) (string, bool) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || kind == "" || strings.Contains(kind, "/") {
		return "", false
	}
	if strings.HasSuffix(rest, ".memql") {
		return rest, true
	}
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return "", false
	}
	inner, name := rest[:i], rest[i+1:]
	if name == "" || strings.Contains(name, "/") || !strings.HasSuffix(inner, ".memql") {
		return "", false
	}
	return inner, true
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

// languageVersionShape is what a language line looks like; canonicalVersion
// is how one is spelled: no leading zeros, so one line has one spelling and a
// string written down is the string compared.
var (
	languageVersionShape = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	canonicalVersion     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// ResolveLanguageLines reads the declaration of every domain of tree
// (LanguageLineDomainOf over the files dslfs.WalkMemqlFiles walks).
//
// It never fails outright: every domain is returned, and one the engine will
// not read is returned Refused, beside the problems naming why. A tree that
// cannot be walked resolves to nothing; every loader walking it reports the
// walk failure itself.
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
		if d := LanguageLineDomainOf(p); d != "" && !seen[d] {
			seen[d] = true
			domains = append(domains, d)
		}
	}
	sort.Strings(domains)

	var problems []LanguageLineProblem
	for _, d := range domains {
		line, found := resolveDomainLine(tree, core, d)
		line.Refused = len(found) > 0
		lines[d] = line
		problems = append(problems, found...)
	}
	return lines, problems
}

// For returns the line governing a file of the tree the lines were resolved
// from. path is tree-relative ("<domain>/queries.memql"); a loader origin
// ("unified:<path>:<name>") is accepted too. The domain is the resolver's own
// answer (LanguageLineDomainOf), so a file the resolver counts in no domain
// -- one no loader reads -- has no line here either.
func (l LanguageLines) For(path string) (LanguageLine, bool) {
	d := LanguageLineDomainOf(path)
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
// another tree -- is read with this engine's own edition. A file of a Refused
// domain is not read at all: Prepare answers ErrLanguageLineRefused.
//
// An error means the file must not be parsed at all: read under the core
// grammar, text written for another edition could parse into something else.
func (l LanguageLines) Prepare(path string, src []byte) ([]byte, error) {
	edition := Edition
	if line, ok := l.For(path); ok {
		if line.Refused {
			return nil, ErrLanguageLineRefused
		}
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
		// The migrator writes a file only under -w, and its argument is the
		// directory that HOLDS the domain: named from the domain here, so the
		// author reads the path off the message instead of guessing whether
		// "the tree" meant the domain or the bundle.
		current := dslfs.Manifest{Language: LanguageVersion, Edition: Edition}
		return line, []LanguageLineProblem{{
			Domain: domain, Source: line.Source, Code: CodeLanguageLineMissing,
			Message: fmt.Sprintf("domain %q declares no language line: add %s containing\n%s(or run: memqlmigrate --rewrite=language-line -w <dir>, where <dir> is the directory that holds %s/) [%s]",
				domain, line.Source, indentLines(current.Render(), "  "), domain, CodeLanguageLineMissing),
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
	case !languageVersionShape.MatchString(m.Language):
		add(CodeLanguageLineMalformed, "domain %q declares memql = %q in %s, which is not a language version: write <major>.<minor>, as in memql = %q",
			line.Domain, m.Language, line.Source, LanguageVersion)
	case !canonicalVersion.MatchString(m.Language):
		major, minor, _ := splitLanguageVersion(m.Language)
		add(CodeLanguageLineMalformed, "domain %q declares memql = %q in %s, which spells the version with a leading zero: write memql = %q",
			line.Domain, m.Language, line.Source, fmt.Sprintf("%d.%d", major, minor))
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
			add(CodeEditionUnknown, "domain %q declares edition = %q in %s, which this engine does not read (it reads: %s): declare edition = %q",
				line.Domain, m.Edition, line.Source, strings.Join(Editions(), ", "), Edition)
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

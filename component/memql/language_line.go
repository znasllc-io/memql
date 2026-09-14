package memql

// language_line.go -- the language line in the loader (epic memql#5356, task
// memql#5357; D4 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// Every domain of the tree declares the language it is written in: a core
// domain through the embedded dsl/memql.toml, every other domain through its
// own <domain>/memql.toml. The engine refuses a domain that declares none, one
// newer than it speaks, or an edition it has no front end for -- at the two
// places a tree enters the engine:
//
//   - BuildUnifiedConcepts, which runs before Init and before any concept
//     reaches the database, drops a refused domain's concepts and records a
//     ConceptSkip (app/database.go refuses boot on it);
//   - Init, whose FIRST step records each refusal on the load report, so
//     strict boot refuses the tree and MEMQL_DSL_ALLOW_SKIPS is the
//     break-glass, exactly as for a construct that fails to parse.
//
// Both render one refusal identically, and the offline passes that collect
// both (LintUnifiedTree, AnalyzePackageDSL) print it once.
//
// EDITIONS AT EVERY PARSE SITE (task memql#5358). Every loader that reads a
// tree file -- baseloader.ReadAll and, for the loaders that walk the tree
// themselves, the concept build, the automation loader, the action loader and
// capability catalog, the capability-name loader, the dependency validator
// and dslimports -- first hands it to LanguageLines.Prepare, the front end of
// the edition that file's domain declares. A file a front end refuses is read
// by no loader. Init reports it, once, on the load report (editionRefusals
// below); the two readers with problem lists of their own, the automation
// loader and dslimports, name it there too.
//
// The rule itself -- where a declaration lives, what the engine reads, the
// messages -- is the parser's (component/language/parser/language_line.go):
// the loaders that read a tree live in four modules, and component/actions
// cannot import this one. This file is the engine's side of it.

import (
	"io/fs"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// LanguageLine is the declaration governing one domain of a tree.
type LanguageLine = langparser.LanguageLine

// LanguageLineProblem is one declaration the engine will not read.
type LanguageLineProblem = langparser.LanguageLineProblem

// LanguageLines is every resolved domain of one tree, by domain.
type LanguageLines = langparser.LanguageLines

// How a refused language line lands on the load report. The corpus and the
// tests match on these, so they are the contract.
const (
	languageLineComponent = "languageLine"
	languageLineKeyword   = "domain"
	languageLinePhase     = "language-line"
)

// ResolveLanguageLines reads every domain's declaration in tree, a core domain
// through the embedded manifest. It never fails outright: a problem domain is
// also returned in the lines, with the engine's own edition, so parsing
// proceeds and one boot reports every problem.
func ResolveLanguageLines(tree fs.FS) (LanguageLines, []LanguageLineProblem) {
	return langparser.ResolveLanguageLines(tree, memqldsl.EmbeddedTree{})
}

// LanguageLineFor returns the declaration governing a file path of the loaded
// tree ("<domain>/queries.memql", or a loader origin naming one). It is the
// hook later epics key version-dependent meaning on: a behaviour that changes
// between two language lines reads the file's declared line, not the engine's.
// False before Init, and for a path in no domain of the tree.
func (e *MemQLEngine) LanguageLineFor(path string) (LanguageLine, bool) {
	return e.languageLines.For(path)
}

// languageLineSkip is a refused line as a load-report entry, named by the
// domain and by the file that declares (or must declare) its line.
func languageLineSkip(p LanguageLineProblem) baseloader.Skip {
	return baseloader.Skip{
		Component: languageLineComponent,
		Keyword:   languageLineKeyword,
		Name:      p.Domain,
		File:      p.Source,
		Phase:     languageLinePhase,
		Err:       p.Message,
	}
}

// resolveLanguageLines is Init's first step: resolve the merged tree's lines
// for LanguageLineFor, and record every refusal before any loader runs -- a
// domain whose line the engine will not read, and a file its edition's front
// end refuses.
func (e *MemQLEngine) resolveLanguageLines(report *LoadReport) {
	tree := memqldsl.Tree()
	lines, problems := ResolveLanguageLines(tree)
	e.languageLines = lines
	for _, p := range problems {
		report.AddSkip(languageLineSkip(p))
		// Logger is PROMOTED through the embedded *component.Component, so a
		// Component-less engine panics on it (#2674).
		if e.Component != nil && e.Logger != nil {
			e.Logger.Error("DSL language line refused",
				"component", "memql.engine",
				"domain", p.Domain,
				"file", p.Source,
				"code", p.Code,
				"detail", p.Message)
		}
	}
	for _, s := range editionRefusals(tree, lines) {
		report.AddSkip(s)
		if e.Component != nil && e.Logger != nil {
			e.Logger.Error("DSL file refused by its edition's front end",
				"component", "memql.engine",
				"file", s.File,
				"detail", s.Err)
		}
	}
}

// editionRefusals is every file of tree its edition's front end refuses
// (memql#5358), one report entry each.
//
// This is the ONE place such a file is reported. Every loader reads a tree
// file through LanguageLines.Prepare and leaves out a file it refuses -- read
// under the core grammar, text written for another edition could parse into
// something else -- so without this the file would simply be missing. The
// set is the one baseloader.ReadAll reads: a disabled pack's behavioral file
// is not read, so it cannot refuse boot (module-registry design 4.2).
func editionRefusals(tree fs.FS, lines LanguageLines) []baseloader.Skip {
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return nil // every loader walking the tree reports the walk itself
	}
	var out []baseloader.Skip
	for _, p := range paths {
		if memqldsl.SkipsBehavioralLoad(p) {
			continue
		}
		raw, err := fs.ReadFile(tree, p)
		if err != nil {
			continue
		}
		if _, err := lines.Prepare(p, raw); err != nil {
			out = append(out, baseloader.Skip{
				Component: languageLineComponent,
				Keyword:   "file",
				Name:      p,
				File:      p,
				Phase:     "edition",
				Err:       err.Error(),
			})
		}
	}
	return out
}

package conformance

// corpus_gates_test.go -- the two completeness gates over the conformance
// corpus (D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md;
// epic memql#5356, task memql#5361).
//
// The runner (corpus_test.go) holds the engine to every case the corpus has.
// These two hold the CORPUS to the language: every place an annotation can be
// written, and every position an expression can be written in, has a case the
// engine loads and a case it refuses. Without them the corpus covers what its
// authors happened to write, and a rule nobody wrote a case for is a rule the
// engine may stop keeping without anything turning red.
//
// Both read the corpus through the runner's own reader (readCorpusDir), so a
// file the runner refuses is refused here, and a case the gate counts is a
// case the runner runs. Each reads ONLY its own subtree -- cells/ for one,
// expr/ for the other -- so a malformed file elsewhere in the edition (another
// case group's directory) fails the runner once rather than every gate too.

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/core/dslfs"
)

// The verdicts a completeness gate counts on each side. A case that lowers or
// evaluates is a form the engine accepts, so it counts as one that loads.
var (
	corpusAcceptingVerdicts = map[string]bool{verdictLoadOK: true, verdictLower: true, verdictEvaluate: true}
	corpusRefusingVerdicts  = map[string]bool{verdictRefuseParse: true, verdictRefuseLoad: true}
)

// corpusReceiverDir is a receiver's directory under cells/: its name with the
// first letter lower-cased (Query -> query, ConceptField -> conceptField).
func corpusReceiverDir(r annotations.Receiver) string {
	s := string(r)
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// corpusCellDir is a placement's cell directory, relative to the edition.
func corpusCellDir(p annotations.Placement) string {
	return "cells/" + corpusReceiverDir(p.Receiver) + "/" + p.Name
}

// corpusVerdictSides counts, per directory relative to the edition, whether a
// case that is accepted and a case that is refused exist.
type corpusVerdictSides struct{ accepted, refused int }

// corpusSidesUnder reads the case directories under one subtree of the
// edition (cells, expr) with the runner's reader, and counts each directory's
// sides. A subtree that does not exist reads as nothing.
func corpusSidesUnder(t *testing.T, edition, subtree string) map[string]*corpusVerdictSides {
	t.Helper()
	root := os.DirFS(".")
	start := edition + "/" + subtree
	if _, err := fs.Stat(root, start); err != nil {
		return nil
	}
	vm := readCorpusManifest(t, root, edition)
	line := dslfs.Manifest{Language: vm.Language, Edition: vm.Edition}
	domains := map[string]string{}
	out := map[string]*corpusVerdictSides{}
	err := fs.WalkDir(root, start, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || path.Base(p) != "expect.json" {
			return nil
		}
		dir := path.Dir(p)
		for _, r := range readCorpusDir(t, root, edition, dir, line, domains) {
			rel := strings.TrimPrefix(r.dir, edition+"/")
			s := out[rel]
			if s == nil {
				s = &corpusVerdictSides{}
				out[rel] = s
			}
			switch {
			case corpusAcceptingVerdicts[r.c.Verdict]:
				s.accepted++
			case corpusRefusingVerdicts[r.c.Verdict]:
				s.refused++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", start, err)
	}
	return out
}

// corpusMissingSides names what a directory lacks, "" when it has both sides.
func corpusMissingSides(s *corpusVerdictSides) string {
	switch {
	case s == nil || (s.accepted == 0 && s.refused == 0):
		return "no cases (want one the engine loads and one it refuses)"
	case s.accepted == 0:
		return "no case that loads (load_ok, lower or evaluate)"
	case s.refused == 0:
		return "no refused case (refuse_parse or refuse_load)"
	}
	return ""
}

// TestCorpusCoversEveryRegistryCell: every placement in the annotation
// registry (annotations.Placements) has a cell,
// cells/<receiver>/<annotation>/, holding at least one case the engine
// accepts and one it refuses; and every directory under cells/ is a cell some
// placement names. It fails once, listing every gap.
func TestCorpusCoversEveryRegistryCell(t *testing.T) {
	edition := langparser.Edition
	sides := corpusSidesUnder(t, edition, "cells")

	var problems []string
	want := map[string]bool{}
	receivers := map[string]bool{}
	for _, p := range annotations.Placements() {
		dir := corpusCellDir(p)
		want[dir] = true
		receivers["cells/"+corpusReceiverDir(p.Receiver)] = true
		if msg := corpusMissingSides(sides[dir]); msg != "" {
			problems = append(problems, dir+": "+msg)
		}
	}
	if len(want) == 0 {
		t.Fatal("annotations.Placements() is empty: this gate would pass over nothing")
	}
	problems = append(problems, corpusStrayCellEntries(t, edition, receivers, want)...)

	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	t.Fatalf("%d problem(s) with the corpus's registry cells (%s/cells/):\n  %s\n\n"+
		"Every place an annotation can be written has a cell holding a case the engine loads and one it "+
		"refuses (D23). Write the missing cases by hand, or start from a skeleton:\n"+
		"  go test ./test/conformance -run TestScaffoldCorpusCells -scaffold\n"+
		"then read every file it wrote. A directory no placement names is a typo or a retired annotation: "+
		"rename it, or delete it with the placement it tested.",
		len(problems), edition, strings.Join(problems, "\n  "))
}

// corpusStrayCellEntries names everything under cells/ that is not a cell: a
// receiver directory no receiver names, an annotation directory no placement
// on that receiver names, a directory nested inside a cell, or a file outside
// one.
func corpusStrayCellEntries(t *testing.T, edition string, receivers, cells map[string]bool) []string {
	t.Helper()
	root := os.DirFS(edition)
	if _, err := fs.Stat(root, "cells"); err != nil {
		return nil
	}
	var out []string
	err := fs.WalkDir(root, "cells", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "cells" {
			return nil
		}
		depth := strings.Count(p, "/")
		switch {
		case d.IsDir() && depth == 1 && !receivers[p]:
			out = append(out, p+": no receiver's directory is named "+path.Base(p)+
				" (receiver directories are the annotations.Receivers() names, lower-camel-cased)")
			return fs.SkipDir
		case d.IsDir() && depth == 2 && !cells[p]:
			out = append(out, p+": no placement on "+path.Base(path.Dir(p))+" is named "+path.Base(p)+
				" (a typo, or an annotation the registry no longer has on this receiver)")
			return fs.SkipDir
		case d.IsDir() && depth > 2:
			out = append(out, p+": a directory inside a cell -- a cell holds its cases directly")
			return fs.SkipDir
		case !d.IsDir() && depth < 3:
			out = append(out, p+": a file outside a cell -- cases live in cells/<receiver>/<annotation>/")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s/cells: %v", edition, err)
	}
	return out
}

// TestCorpusCoversEveryTierPosition: every expression position in the tier
// manifest (tiers.Positions) has a directory, expr/<position>/, holding at
// least one case the engine accepts and one it refuses at that position; and
// every directory under expr/ is a position. It fails once, listing every gap.
//
// Cases in a directory nested under expr/<position>/ count toward that
// position: the expression epic organises a position's cases by node kind and
// function, and each such case is still a case at the position.
func TestCorpusCoversEveryTierPosition(t *testing.T) {
	edition := langparser.Edition
	sides := corpusSidesUnder(t, edition, "expr")

	byPosition := map[string]*corpusVerdictSides{}
	for dir, s := range sides {
		parts := strings.Split(dir, "/")
		if len(parts) < 2 || parts[0] != "expr" {
			continue
		}
		agg := byPosition[parts[1]]
		if agg == nil {
			agg = &corpusVerdictSides{}
			byPosition[parts[1]] = agg
		}
		agg.accepted += s.accepted
		agg.refused += s.refused
	}

	var problems []string
	known := map[string]bool{}
	for _, pos := range tiers.Positions() {
		known[string(pos)] = true
		if msg := corpusMissingSides(byPosition[string(pos)]); msg != "" {
			problems = append(problems, fmt.Sprintf("expr/%s: %s", pos, msg))
		}
	}
	if len(known) == 0 {
		t.Fatal("tiers.Positions() is empty: this gate would pass over nothing")
	}
	if entries, err := fs.ReadDir(os.DirFS(edition), "expr"); err == nil {
		for _, e := range entries {
			switch {
			case !e.IsDir():
				problems = append(problems, "expr/"+e.Name()+": a file outside a position directory -- cases live in expr/<position>/")
			case !known[e.Name()]:
				problems = append(problems, "expr/"+e.Name()+": no tiers.Position is named "+e.Name())
			}
		}
	}

	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	t.Fatalf("%d problem(s) with the corpus's expression positions (%s/expr/):\n  %s\n\n"+
		"Every position an expression can be written in has a case the engine accepts there and one it "+
		"refuses there (D23). Measure what the engine does today at the position and pin it.",
		len(problems), edition, strings.Join(problems, "\n  "))
}

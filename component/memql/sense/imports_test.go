package sense

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func newImportsService() *Service { return New(nil) }

func paths(in []Import) []string {
	out := make([]string, 0, len(in))
	for _, imp := range in {
		out = append(out, imp.Path)
	}
	return out
}

func TestImports_ProjectsFormBDeclarations(t *testing.T) {
	const src = `use cognition.concepts.{ participant, space }
use common.traits.{ isActiveRecord }

@description("Get space participants")
query participant spaceParticipants {
  filter  isActiveRecord
  shape   participantFull
}
`
	got := newImportsService().Imports(src)
	want := []Import{
		{Path: "cognition.concepts", Names: []string{"participant", "space"}},
		{Path: "common.traits", Names: []string{"isActiveRecord"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %+v;\nwant %+v", got, want)
	}
}

// THE CASE THE REGEX GETS WRONG (memql#3335).
//
// The extension scanned for `use <dotted>.{` with a line-anchored regex, which
// has no idea it is inside a block comment. The lexer skips /* ... */
// outright, so a commented-out import produces no tokens at all -- and a
// commented-out import is not an import.
//
// The consequence of getting this wrong is the invisible kind: the bundle
// walks to a file the buffer does not actually import, and in the reverse
// direction the same blindness is what loses a genuinely dirty dependency.
func TestImports_IgnoresAUseLineInsideABlockComment(t *testing.T) {
	const src = `use cognition.concepts.{ participant }

/*
Retired -- the shapes moved into cognition.concepts:
use cognition.shapes.{ participantFull }
use legacy.module.{ thing }
*/

use common.traits.{ isActiveRecord }
`
	got := paths(newImportsService().Imports(src))
	want := []string{"cognition.concepts", "common.traits"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %v; want %v -- a `use` inside /* */ is not an import", got, want)
	}
}

func TestImports_IgnoresAUseLineInsideALineComment(t *testing.T) {
	const src = `use cognition.concepts.{ participant }
// use cognition.shapes.{ participantFull }
   //use common.traits.{ isActiveRecord }
`
	got := paths(newImportsService().Imports(src))
	want := []string{"cognition.concepts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %v; want %v", got, want)
	}
}

// The word `use` inside a string literal lexes as part of the string, never as
// the keyword -- so no amount of import-shaped text in a @description can
// invent a dependency.
func TestImports_IgnoresAUseInsideAStringLiteral(t *testing.T) {
	const src = `use cognition.concepts.{ participant }

@description("do not write use cognition.shapes.{ participantFull } here")
query participant spaceParticipants {
  filter  isActiveRecord
  shape   participantFull
}
`
	got := paths(newImportsService().Imports(src))
	want := []string{"cognition.concepts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %v; want %v", got, want)
	}
}

// The lexer does not care where the newlines fall, so a declaration split
// across lines reads identically to the compiler and to this. The line regex
// this replaces required the whole `use <path>.{` prefix on one line.
func TestImports_HandlesADeclarationSplitAcrossLines(t *testing.T) {
	const src = `use cognition.shapes.{
  participantFull,
  spaceCard
}
`
	got := newImportsService().Imports(src)
	want := []Import{{Path: "cognition.shapes", Names: []string{"participantFull", "spaceCard"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %+v; want %+v", got, want)
	}
}

// Reported as authored, duplicates included: two declarations naming one
// module with different brace lists are two declarations, and collapsing them
// here would throw away all but the first's Names. A client that wants the
// distinct module set collapses it in one line.
func TestImports_KeepsDuplicateModulePaths(t *testing.T) {
	const src = `use cognition.concepts.{ participant }
use cognition.concepts.{ space }
`
	got := newImportsService().Imports(src)
	want := []Import{
		{Path: "cognition.concepts", Names: []string{"participant"}},
		{Path: "cognition.concepts", Names: []string{"space"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %+v; want %+v", got, want)
	}
}

// Form A (`use <ns>.<concept>`, with or without `as`) is retired and rejected
// at parse time. Following one would walk to a file the compiler will not
// read, so it is not reported.
func TestImports_SkipsTheRetiredFormA(t *testing.T) {
	const src = `use cognition.participant
use cognition.participant as p
use cognition.concepts.{ participant }
`
	got := paths(newImportsService().Imports(src))
	want := []string{"cognition.concepts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %v; want %v (Form A is retired)", got, want)
	}
}

// The whole reason this is a token walk rather than a full parse: the editor
// asks during a run, on a buffer that is usually mid-edit. A construct that
// does not parse must not erase the imports above it -- "your imports vanished
// because line 200 is incomplete" is precisely the invisible failure the
// request exists to remove.
func TestImports_ABrokenConstructDoesNotEraseTheImports(t *testing.T) {
	const src = `use cognition.concepts.{ participant }
use common.traits.{ isActiveRecord }

query participant halfTyped {
  args {
    spaceId  string  @requi
`
	got := paths(newImportsService().Imports(src))
	want := []string{"cognition.concepts", "common.traits"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %v; want %v -- a mid-edit construct must not drop the imports", got, want)
	}
}

// An unterminated brace group is a line the developer is still typing, not a
// declaration. Reported as absent rather than as a partial import to a module
// nobody has finished naming.
func TestImports_SkipsAnUnclosedBraceGroup(t *testing.T) {
	const src = `use cognition.concepts.{ participant
`
	if got := newImportsService().Imports(src); len(got) != 0 {
		t.Errorf("imports = %+v; want none", got)
	}
}

// Always a non-nil empty slice: the TypeScript consumer iterates it directly.
func TestImports_EmptyAndUnlexableBuffersYieldEmptySlices(t *testing.T) {
	for _, src := range []string{"", "// nothing here\n", "query participant q {\n  filter  isActiveRecord\n}\n"} {
		got := newImportsService().Imports(src)
		if got == nil {
			t.Fatalf("Imports(%q) = nil; want a non-nil empty slice", src)
		}
		if len(got) != 0 {
			t.Errorf("Imports(%q) = %+v; want none", src, got)
		}
	}
}

// The live corpus is the real proof that this agrees with the compiler: every
// `use` the loader resolves has to be reported, across every authoring style
// the tree actually contains.
func TestImports_CoversTheLiveCorpus(t *testing.T) {
	const corpusRoot = "../../../dsl"
	if _, err := os.Stat(corpusRoot); err != nil {
		t.Skipf("dsl tree not reachable from here: %v", err)
	}
	// The oracle is the retired line regex -- the exact expression
	// editors/vscode/src/run/bundle.ts scanned with. Every import it finds in
	// a file with NO block comment must also be found here: that is the
	// agreement claim, and it is what makes "the LSP replaces the regex"
	// checkable rather than asserted.
	//
	// Files containing a block comment are excluded from the oracle
	// comparison, because that is precisely where the two are SUPPOSED to
	// disagree -- and the dedicated test above pins that direction.
	oracle := regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\.\{`)

	svc := newImportsService()
	scanned, withImports, missing := 0, 0, []string{}
	err := filepath.WalkDir(corpusRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return err
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		got := svc.Imports(string(source))
		if len(got) > 0 {
			withImports++
		}
		found := map[string]bool{}
		for _, imp := range got {
			if imp.Path == "" {
				t.Errorf("%s: import with an empty path: %+v", path, imp)
			}
			if len(imp.Names) == 0 {
				t.Errorf("%s: import %q with no names -- Form B always lists at least one", path, imp.Path)
			}
			found[imp.Path] = true
		}
		if strings.Contains(string(source), "/*") {
			return nil
		}
		for _, m := range oracle.FindAllStringSubmatch(string(source), -1) {
			if !found[m[1]] {
				missing = append(missing, m[1]+" in "+path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", corpusRoot, err)
	}
	// A corpus that produced no imports at all would mean the walk silently
	// matches nothing -- which every assertion above would still pass.
	if withImports < 20 {
		t.Fatalf("only %d of %d corpus files carried imports; the corpus or the walk moved", withImports, scanned)
	}
	if len(missing) > 0 {
		t.Errorf("%d corpus imports the retired regex found were not reported:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	t.Logf("scanned %d corpus files, %d carry imports", scanned, withImports)
}

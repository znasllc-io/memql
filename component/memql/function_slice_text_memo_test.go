package memql

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

// treeFunctionSlices is every function slice of the tree under dsl/.
func treeFunctionSlices(t *testing.T) []FunctionSlice {
	t.Helper()
	var out []FunctionSlice
	err := filepath.WalkDir("../../dsl", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, ExtractFunctionSlices(string(raw))...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking dsl/: %v", err)
	}
	if len(out) < 500 {
		t.Fatalf("found %d function slices; the comparison needs the tree", len(out))
	}
	return out
}

// errText is an error's message, or "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestSliceTextFactsAreWhatTheTextReads: for every function slice of the
// tree, what the memo answers is what the four text steps answer when asked
// directly -- the memo changes when the work is done, never what it finds.
func TestSliceTextFactsAreWhatTheTextReads(t *testing.T) {
	for _, s := range treeFunctionSlices(t) {
		got := sliceTextFactsOf(s.Source)
		legacy := languageParser.RejectLegacyProceduralAuthorForm(s.Source)
		if errText(got.legacyForm) != errText(legacy) {
			t.Fatalf("%s: legacy-form refusal %q, the text reads %q", s.Name, errText(got.legacyForm), errText(legacy))
		}
		if legacy != nil {
			continue
		}
		if want := extractAllSignatureConceptNames(s.Source); !reflect.DeepEqual(got.signatureConcepts, want) {
			t.Errorf("%s: signature concepts %v, the text reads %v", s.Name, got.signatureConcepts, want)
		}
		normalised, rerr := languageParser.NormaliseAll(s.Source)
		if got.normalised != normalised || errText(got.rewriteErr) != errText(rerr) {
			t.Errorf("%s: the memoized rewrite differs from NormaliseAll's", s.Name)
		}
		if rerr != nil {
			continue
		}
		authored := s.Source[:s.BodyOffset] + languageParser.AnchorSource(s.Source[s.BodyOffset:], s.Line)
		if got, want := positionLoweringOf(authored, normalised), languageParser.PositionLowering(authored, normalised); got != want {
			t.Errorf("%s: the memoized position lowering differs from PositionLowering's", s.Name)
		}
	}
}

// TestSliceTextIsReadOncePerText: the text steps run once per distinct text,
// asserted as a count; the position lowering once per (authored, lowered)
// pair, so a lowered text that differs -- a registry that resolved the slice
// differently -- is lowered anew.
func TestSliceTextIsReadOncePerText(t *testing.T) {
	src := "/// " + t.Name() + "\nquery thing q" + strings.ReplaceAll(uniqueSuffix("x"), "-", "") + " {\n  sort \"row.createdAt\", \"desc\"\n  paginate 10\n}\n"
	before := sliceTextReads.Load()
	for i := 0; i < 3; i++ {
		sliceTextFactsOf(src)
	}
	if got := sliceTextReads.Load() - before; got != 1 {
		t.Errorf("three reads of one text ran the text steps %d times, want 1", got)
	}

	lowered := sliceTextFactsOf(src).normalised
	if lowered == "" || lowered == src {
		t.Fatalf("fixture: the rewrite produced %q", lowered)
	}
	authored := languageParser.AnchorSource(src, 7)
	before = positionLowerings.Load()
	first := positionLoweringOf(authored, lowered)
	if positionLoweringOf(authored, lowered) != first {
		t.Fatal("the same pair was lowered two ways")
	}
	if got := positionLowerings.Load() - before; got != 1 {
		t.Errorf("two lowerings of one pair ran PositionLowering %d times, want 1", got)
	}
	other := strings.Replace(lowered, "desc", "asc", 1)
	if got, want := positionLoweringOf(authored, other), languageParser.PositionLowering(authored, other); got != want || got == first {
		t.Error("a different lowered text was answered from the first one's entry")
	}
}

// TestSliceTextRefusalIsPlacedPerCall: a refusal is held unpositioned, so the
// same slice anchored at two lines of its file is refused at each.
func TestSliceTextRefusalIsPlacedPerCall(t *testing.T) {
	src := "func (Query) retiredForm(ctx any) (any, error) {\n  return nil, nil\n}\n"
	_, at10 := tryParseFunctionSlice("retiredForm", "query", src, "probe/queries.memql", nil, 10, 0)
	_, at20 := tryParseFunctionSlice("retiredForm", "query", src, "probe/queries.memql", nil, 20, 0)
	if at10 == nil || at20 == nil {
		t.Fatalf("the retired form was accepted: %v, %v", at10, at20)
	}
	var p10, p20 *languageParser.PositionedRewriteError
	if !errors.As(at10, &p10) || !errors.As(at20, &p20) {
		t.Fatalf("the refusals are not positioned: %v / %v", at10, at20)
	}
	if p10.Parse.Line != 10 || p20.Parse.Line != 20 {
		t.Errorf("refusals placed at lines %d and %d, want 10 and 20 -- one call's position reached the other", p10.Parse.Line, p20.Parse.Line)
	}
}

// TestSliceTextSignatureConceptsAreTheCallersCopy: a caller that edits the
// signature concepts it was handed cannot change the next caller's.
func TestSliceTextSignatureConceptsAreTheCallersCopy(t *testing.T) {
	src := "/// " + t.Name() + "\nquery sigCopyThing sigCopyQuery {\n  sort \"row.createdAt\", \"desc\"\n  paginate 10\n}\n"
	first := sliceTextFactsOf(src)
	if len(first.signatureConcepts) != 1 || first.signatureConcepts[0] != "sigCopyThing" {
		t.Fatalf("fixture: signature concepts %v", first.signatureConcepts)
	}
	first.signatureConcepts[0] = "edited"
	if again := sliceTextFactsOf(src); again.signatureConcepts[0] != "sigCopyThing" {
		t.Errorf("an edit by one caller reached the next: %v", again.signatureConcepts)
	}
}

// TestPairMemoBoundCountsEverythingItHolds: one authored text can meet any
// number of lowered texts (a registry that resolves a slice differently each
// time), so the bound must count both keys and the value of every entry. A
// bound that counted the authored text once, as sourceMemo counts its source,
// would hold them all.
func TestPairMemoBoundCountsEverythingItHolds(t *testing.T) {
	m := &pairMemo{maxBytes: 400}
	authored := strings.Repeat("a", 20)
	for i := 0; i < 200; i++ {
		lowered := fmt.Sprintf("lowered-%04d-%s", i, strings.Repeat("b", 20))
		want := "lexed:" + lowered
		if got := m.get(authored, lowered, func() string { return want }); got != want {
			t.Fatalf("entry %d: got %q, want %q", i, got, want)
		}
		// What is actually held, counted from the entries rather than read
		// off the memo's own counter -- the counter is what a wrong bound
		// gets wrong.
		m.mu.Lock()
		held, entries := 0, len(m.byPair)
		for k, v := range m.byPair {
			held += len(k[0]) + len(k[1]) + len(v)
		}
		m.mu.Unlock()
		if held > m.maxBytes {
			t.Fatalf("after %d entries the memo holds %d bytes in %d entries, over its %d bound", i+1, held, entries, m.maxBytes)
		}
	}
	computed := 0
	m.get("big", strings.Repeat("x", 500), func() string { computed++; return "v" })
	m.get("big", strings.Repeat("x", 500), func() string { computed++; return "v" })
	if computed != 2 {
		t.Errorf("an entry larger than the bound was held (computed %d times, want 2)", computed)
	}
}

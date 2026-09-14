package memql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// keyword_slices_memo_test.go -- the slicers are memoized per process by
// source (sourceMemo), which is what took the per-boot slicing cost out of
// every engine boot. A memo is only sound over a pure function, and only as
// long as nothing a caller does to its answer can reach the next caller's; these
// pin both, over the whole embedded tree.

// TestSlicingMemoAgreesWithTheSlicersOverTheTree: for every file of the
// tree and every keyword the loaders slice, the memoized answer is the
// slicer's own -- on the first ask and on the next, which is the hit.
func TestSlicingMemoAgreesWithTheSlicersOverTheTree(t *testing.T) {
	files := baseloader.ReadAll(nil)
	require.NotEmpty(t, files)
	keywords := []string{"concept", "shape", "spec", "trait", "tool", "prompt", "provider", "builtin", "policy", "rule", "seed", "action"}
	compared := 0
	for _, f := range files {
		for _, kw := range keywords {
			want := sliceDeclarations(f.Content, kw)
			for i := 0; i < 2; i++ {
				got := constructDeclarationSlices(f.Content, kw)
				require.True(t, reflect.DeepEqual(want, got), "%s %s (ask %d): the memo's slices differ from the slicer's", f.Path, kw, i+1)
			}
			compared += len(want)
		}
		want := extractFunctionSlices(f.Content)
		for i := 0; i < 2; i++ {
			require.True(t, reflect.DeepEqual(want, ExtractFunctionSlices(f.Content)), "%s (ask %d): the memo's function slices differ from the slicer's", f.Path, i+1)
		}
		compared += len(want)
	}
	require.Greater(t, compared, 1000, "the comparison reached the tree's declarations")
}

// TestSlicingMemoHandsOutCopies: what one caller does to its slices -- append,
// reorder, overwrite an element -- never reaches the next caller's.
func TestSlicingMemoHandsOutCopies(t *testing.T) {
	src := "/// One.\nspec ticket isOpenMemo = row => row.status == \"open\"\n\n/// Two.\nspec ticket isClosedMemo = row => row.status == \"closed\"\n"
	// Both the answer to the ask that fills the memo and the answers to the
	// asks it serves are the caller's own.
	for i := 0; i < 3; i++ {
		got := constructDeclarationSlices(src, "spec")
		require.Len(t, got, 2)
		require.Equal(t, []string{"isOpenMemo", "isClosedMemo"}, []string{got[0].Name, got[1].Name}, "ask %d", i+1)
		got[0].Name = "overwritten"
		got[0], got[1] = got[1], got[0]
		_ = append(got[:1], languageParser.DeclarationSlice{Name: "appended"})
	}

	q := "use x.concepts.{ ticket }\n\n/// Q.\nquery ticket memoQ {\n  filter row => row.status == \"open\"\n}\n"
	for i := 0; i < 3; i++ {
		got := ExtractFunctionSlices(q)
		require.Len(t, got, 1)
		require.True(t, strings.HasPrefix(got[0].Source, "use x.concepts"), "ask %d: %q", i+1, got[0].Source)
		got[0].Source = "overwritten"
	}
}

// TestSlicingMemoIsBounded: a source that would take the memo past its bound
// clears it rather than growing it -- a long-running node validates authored
// bundles through the same slicers -- and a source larger than the bound is
// answered and never held.
func TestSlicingMemoIsBounded(t *testing.T) {
	m := &sourceMemo[int]{maxBytes: 10}
	calls := 0
	compute := func() []int { calls++; return []int{calls} }

	require.Equal(t, []int{1}, m.get("aaaa", "k", compute))
	require.Equal(t, []int{1}, m.get("aaaa", "k", compute), "a hit does not recompute")
	require.Equal(t, 1, calls)
	m.get("bbbb", "k", compute) // 8 bytes held
	require.Equal(t, 8, m.bytes)
	m.get("cccc", "k", compute) // would be 12: cleared, then held alone
	require.Equal(t, 4, m.bytes)
	require.Len(t, m.bySource, 1)
	m.get("aaaa", "k", compute)
	require.Equal(t, 4, calls, "a cleared source is recomputed, never answered wrong")

	m.get("this source is longer than the bound", "k", compute)
	_, held := m.bySource["this source is longer than the bound"]
	require.False(t, held, "a source over the bound is answered and not held")
	require.Equal(t, 8, m.bytes)
}

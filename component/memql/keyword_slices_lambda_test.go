package memql

import (
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// keyword_slices_lambda_test.go -- the edition-2026 spec and trait (memql#5366)
// has no braces, so the brace slicer cannot find it. Without the brace-less
// slicer (languageParser.ExtractPredicateDeclarationSlices, behind
// constructDeclarationSlices) every `=` declaration in a tree is ABSENT from
// the registry -- not refused, not skipped -- and every query applying one
// fails as if it had never been declared. These cases pin, at the loader's
// entry point, that each slice parses on its own as the spec it names.

const lambdaSlicesSource = `use crm.concepts.{ lead }

/// Matches open leads.
spec lead isOpen = row => row.status == "open"

/// A body broken across lines stays one declaration.
spec lead isHot = row =>
  row.score > 80
  && (row.status == "open" || row.status == "new")   // trailing comment
  && row.tags.any(t => t == "hot")

/* spec lead commentedOut = row => row.x == 1 */

trait isActiveRecord = row => row.active == true

/// A legacy body beside the new ones still slices.
spec lead isLegacy = row => row.status == "won"

// A string holding a declaration keyword does not end the body.
spec lead mentionsSpec = row =>
  row.note == "spec lead x = row"
`

func TestExtractKeywordSlices_FindsEditionTwentySixSpecsAndTraits(t *testing.T) {
	specs := ExtractKeywordSlices(lambdaSlicesSource, "spec")
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	require.Equal(t, []string{"isOpen", "isHot", "isLegacy", "mentionsSpec"}, names, "in source order, the commented-out one excluded")

	for _, s := range specs {
		decl, err := languageParser.ParseSpecDecl(s.Source)
		require.NoError(t, err, "slice %q must parse on its own:\n%s", s.Name, s.Source)
		require.Equal(t, s.Name, decl.Name)
	}
	require.Contains(t, specs[0].Source, "/// Matches open leads.\nspec lead isOpen", "the preamble travels with the slice")
	require.Contains(t, specs[1].Source, `&& row.tags.any(t => t == "hot")`, "a continuation line belongs to the body above it")
	require.NotContains(t, specs[1].Source, "trait", "the body ends before the next declaration")
	require.Contains(t, specs[3].Source, `row.note == "spec lead x = row"`)

	traits := ExtractKeywordSlices(lambdaSlicesSource, "trait")
	require.Len(t, traits, 1)
	require.Equal(t, "isActiveRecord", traits[0].Name)
	decl, err := languageParser.ParseSpecDecl(traits[0].Source)
	require.NoError(t, err)
	require.True(t, decl.IsTrait)
	require.NotNil(t, decl.Lambda)
}

func TestExtractKeywordSlices_AComparisonIsNotAHeader(t *testing.T) {
	// `==` after a name is not the declaration's `=`.
	require.Empty(t, ExtractKeywordSlices("spec lead x == y\n", "spec"))
}

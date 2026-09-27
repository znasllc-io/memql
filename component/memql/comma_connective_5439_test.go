package memql

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestExecuteStringRefusesTheCommaConnective is memql#5439 at the engine's own
// seam: the string a client sends to Execute cannot spell OR with a comma. The
// refusal reaches the caller as the parser's *RetiredFormError, carrying
// retired_comma_connective -- the code the edition-2026 grammar refuses the
// same comma with in a .memql file -- so a caller keys on one code whichever
// grammar met it. The `||` spelling of each is what the comma used to mean,
// and it still parses.
func TestExecuteStringRefusesTheCommaConnective(t *testing.T) {
	eng := newParserTestEngine(t)
	for _, tc := range []struct{ comma, pipe string }{
		{"concept==v1:conversation, concept==v1:message", "concept==v1:conversation || concept==v1:message"},
		{"concept==v1:conversation&&payload.active==true,concept==v1:message", "concept==v1:conversation&&payload.active==true||concept==v1:message"},
		// the memql#3612 shape: a comma inside parentheses read as OR, so the
		// ownership conjunct became a disjunct
		{"concept==v1:todos:todo && (ownerUserId==actor.userId, visibility==\"public\")", "concept==v1:todos:todo && (ownerUserId==actor.userId || visibility==\"public\")"},
		{"parentOf(concept==v1:conversation, concept==v1:message)", "parentOf(concept==v1:conversation || concept==v1:message)"},
		{"sort(paginate((concept==v1:conversation, concept==v1:message), 10), \"createdAt\", \"desc\")", "sort(paginate((concept==v1:conversation || concept==v1:message), 10), \"createdAt\", \"desc\")"},
	} {
		t.Run(tc.comma, func(t *testing.T) {
			_, err := eng.Parse(tc.comma)
			require.Errorf(t, err, "the engine must refuse %q", tc.comma)
			var rf *languageParser.RetiredFormError
			require.Truef(t, errors.As(err, &rf), "%q: got %T (%v), want the parser's *RetiredFormError", tc.comma, err, err)
			require.Equal(t, "retired_comma_connective", rf.RuleCode())
			require.Contains(t, err.Error(), "write ||")

			_, err = eng.Parse(tc.pipe)
			require.NoErrorf(t, err, "the || spelling must parse: %q", tc.pipe)
		})
	}
}

package accounts

// export_sweep_db_test.go -- design record section E, point 5, "Nothing
// foreign": for every account-scoped kind of the registry, a row in each of
// two accounts. One account's file holds its own row and not the other's, and
// a kind that stays is in neither. The kinds are read from the registry when
// the test runs, so a kind added later is covered with no change here.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/accountexport"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestExportRunSweepEveryAccountScopedKind(t *testing.T) {
	f := newRunFixture(t)
	f.schema(t)
	kinds := memql.AccountKinds(memorynodes.List(), false)
	require.Greater(t, len(kinds), 100, "the registry is loaded")
	for _, acct := range []string{"sweep-a", "sweep-b"} {
		f.put(t, accountConcept, canonicalPrefix+acct, 0, "system", map[string]any{"name": acct}, nil)
		for _, kind := range kinds {
			f.put(t, kind, kind+":"+acct, 1, "system", map[string]any{"accountId": acct}, nil)
		}
	}

	for _, pair := range [][2]string{{"sweep-a", "sweep-b"}, {"sweep-b", "sweep-a"}} {
		own, foreign := pair[0], pair[1]
		res, err := f.run(t, f.export(own, false), "")
		require.NoError(t, err, own)
		file := f.store.committed[res.ObjectKey]
		_, err = accountexport.Verify(bytes.NewReader(file))
		require.NoError(t, err)
		require.NotContains(t, string(file), ":"+foreign+`"`, "%s's file names a row of %s", own, foreign)
		ms := members(t, file)
		travelled := 0
		for _, kind := range kinds {
			c, _ := memorynodes.Get(kind)
			dir := accountexport.RowsDir(kind)
			require.NotEmpty(t, dir, kind)
			lines := partLines(t, ms, dir)
			if !c.Travels() {
				require.Empty(t, lines, "%s stays, and is in %s's file", kind, own)
				continue
			}
			travelled++
			require.Len(t, lines, 1, "%s in %s's file", kind, own)
			require.Equal(t, kind+":"+own, lines[0]["id"], kind)
		}
		require.Greater(t, travelled, 100)
		for _, m := range ms {
			require.False(t, strings.Contains(m.path, foreign), m.path)
		}
	}
}

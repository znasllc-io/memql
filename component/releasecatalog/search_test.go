package releasecatalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseSearchPagesBeforeReturningTenAndBindsContinuation(t *testing.T) {
	fixture := make([]ReleaseSummary, 43)
	for n := range fixture {
		name := "Other"
		if n%2 == 0 {
			name = "Needle"
		}
		fixture[n] = ReleaseSummary{CandidateID: fmt.Sprint(n), Components: []ComponentSummary{{Name: name, Version: fmt.Sprint(n)}}}
	}
	calls := 0
	read := func(_ context.Context, cursor string) (Page, error) {
		calls++
		offset, _ := strconv.Atoi(cursor)
		end := min(offset+10, len(fixture))
		next := ""
		if end < len(fixture) {
			next = strconv.Itoa(end)
		}
		return Page{SourceID: "publisher", Publisher: "MemQL", Releases: fixture[offset:end], NextCursor: next}, nil
	}
	first, err := searchPages(t.Context(), "publisher", "", "needle", 10, read)
	require.NoError(t, err)
	require.Len(t, first.Releases, 10)
	require.NotEmpty(t, first.NextCursor)
	require.Equal(t, 2, calls)
	second, err := searchPages(t.Context(), "publisher", first.NextCursor, "needle", 10, read)
	require.NoError(t, err)
	require.Len(t, second.Releases, 10)
	third, err := searchPages(t.Context(), "publisher", second.NextCursor, "needle", 10, read)
	require.NoError(t, err)
	require.Len(t, third.Releases, 2)
	require.Empty(t, third.NextCursor)
	all := append(append(first.Releases, second.Releases...), third.Releases...)
	for n, r := range all {
		require.Equal(t, strconv.Itoa(n*2), r.CandidateID)
	}
	_, err = searchPages(t.Context(), "publisher", first.NextCursor, "other", 10, read)
	require.ErrorContains(t, err, "cursor")
	_, err = searchPages(t.Context(), "other-publisher", first.NextCursor, "needle", 10, read)
	require.ErrorContains(t, err, "cursor")
	_, err = searchPages(t.Context(), "publisher", "", strings.Repeat("x", 513), 10, read)
	require.Error(t, err)
}

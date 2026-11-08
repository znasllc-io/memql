package release

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

func TestCandidateBrowseSearchBeforePagingDB(t *testing.T) {
	db := candidateTestDB(t)
	ledger := &candidateLedger{db: func() *sql.DB { return db }}
	owner := id.NewShortId()
	ctx := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
	for n := 0; n < 25; n++ {
		manifest := candidateTestManifest()
		manifest.OwnerUserID = owner
		manifest.Components[0].Version = fmt.Sprintf("0.25.%d", n)
		if n < 12 {
			manifest.Components[0].Repository = "acme/old-needle"
		}
		record, err := ledger.prepare(ctx, manifest)
		require.NoError(t, err)
		cleanupCandidate(t, db, record.ID)
		_, err = db.Exec(`UPDATE release_candidates SET created_at=$2 WHERE candidate_id=$1`, record.ID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
	}
	walk := func(search string, sizes []int) {
		t.Helper()
		cursor := ""
		seen := map[string]bool{}
		for n, size := range sizes {
			page, err := ledger.list(ctx, cursor, 10, search)
			require.NoError(t, err)
			require.Len(t, page.Candidates, size)
			for _, row := range page.Candidates {
				require.False(t, seen[row.ID])
				seen[row.ID] = true
			}
			cursor = page.NextCursor
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
	}
	walk("", []int{10, 10, 5})
	walk("OLD-NEEDLE", []int{10, 2})
	walk("%_", []int{0})
	page, err := ledger.list(ctx, "", 10, "old-needle")
	require.NoError(t, err)
	_, err = ledger.list(ctx, page.NextCursor, 10, "different")
	require.ErrorContains(t, err, "cursor")
}

func TestReleaseBuildBrowseSearchBeforePagingDB(t *testing.T) {
	f := newBuildDBFixture(t)
	base := f.prepare(t)
	ledger := &candidateLedger{db: func() *sql.DB { return f.db }}
	owner := id.NewShortId()
	ctx := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM release_build_intents WHERE owner_user_id=$1`, owner) })
	for n := 0; n < 25; n++ {
		record := base
		record.Selection.Owner = owner
		record.Selection.Source.OwnerUserID = owner
		record.Selection.Version = fmt.Sprintf("0.25.%d", n)
		record.Selection.Tag = "v" + record.Selection.Version
		record.Selection.Request.Tag = record.Selection.Tag
		record.Selection.ProfileName = "recent"
		if n < 12 {
			record.Selection.ProfileName = "old-needle"
		}
		var err error
		record.SelectionDigest, err = profileDigest(record.Selection)
		require.NoError(t, err)
		record.ID, err = buildRequestID(record.Selection)
		require.NoError(t, err)
		_, err = ledger.saveBuild(ctx, record)
		require.NoError(t, err)
		_, err = f.db.Exec(`UPDATE release_build_intents SET created_at=$2 WHERE intent_id=$1`, record.ID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
	}
	walk := func(search string, sizes []int) {
		t.Helper()
		cursor := ""
		seen := map[string]bool{}
		for n, size := range sizes {
			rows, next, err := ledger.buildsPage(ctx, cursor, search, 10)
			require.NoError(t, err)
			require.Len(t, rows, size)
			for _, row := range rows {
				require.False(t, seen[row.ID])
				seen[row.ID] = true
			}
			cursor = next
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
	}
	walk("", []int{10, 10, 5})
	walk("OLD-NEEDLE", []int{10, 2})
	walk("%_", []int{0})
	_, cursor, err := ledger.buildsPage(ctx, "", "old-needle", 10)
	require.NoError(t, err)
	_, _, err = ledger.buildsPage(ctx, cursor, "different", 10)
	require.ErrorContains(t, err, "cursor")
}

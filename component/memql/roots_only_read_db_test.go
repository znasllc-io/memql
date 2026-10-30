package memql

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

// rootsOnlyCounter counts the statements a read made.
type rootsOnlyCounter struct {
	mu sync.Mutex
	n  int
	on bool
}

func (c *rootsOnlyCounter) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (c *rootsOnlyCounter) AfterQuery(context.Context, *bun.QueryEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on {
		c.n++
	}
}

func (c *rootsOnlyCounter) count(act func()) int {
	c.mu.Lock()
	c.n, c.on = 0, true
	c.mu.Unlock()
	act()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on = false
	return c.n
}

// A READ THAT ONLY WANTS ITS OWN ROWS PAYS ONLY FOR THOSE. Every read expands each row's
// outgoing relationships to depth 1, which is the right default for a bundle a client
// walks and two more statements per relationship for a caller that compares three fields
// and throws the rest away, and it is what stood between the access surface's point reads
// and being point reads.
func TestARootsOnlyReadSkipsTheRelationships(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	counter := &rootsOnlyCounter{}
	db.AddQueryHook(counter)

	suffix := id.NewShortId()
	account, pkg, site := "rootsonly-acct-"+suffix, "rootsonly-pkg-"+suffix, "rootsonly-site-"+suffix
	seed := auth.ContextWithInternalOrigin(auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "rootsonly-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}), &auth.TokenInfo{Subject: "rootsonly-seeder"}))
	owner := "v1:identity:user:rootsonly-owner-" + suffix
	insert := func(concept, rowID, payload string) {
		t.Helper()
		_, err := eng.Execute(seed, fmt.Sprintf(`insert(%q, id=%q, payload=%s)`, concept, rowID, payload))
		require.NoError(t, err)
	}
	insert("v1:accounts:account", account, `{"name":"Acme","status":"active","domainStatus":"unverified"}`)
	insert("v1:platform:package", pkg, fmt.Sprintf(`{"name":"desk","sourceKind":"repo","status":"active","ownerUserId":%q,"accountId":%q}`, owner, account))
	insert("v1:platform:site", site, fmt.Sprintf(
		`{"hostname":%q,"kind":"spa","status":"draft","bundleRef":"blob://x/v1","ownerUserId":%q,"packageId":%q,"accountId":%q,"packageDeployableName":"desk"}`,
		site+".example.test", owner, "v1:platform:package:"+pkg, account))
	t.Cleanup(func() {
		for _, row := range []string{"v1:accounts:account:" + account, "v1:platform:package:" + pkg, "v1:platform:site:" + site} {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", row).Exec(context.Background())
		}
	})

	query := fmt.Sprintf(`query siteById(siteId: %q)`, site)
	reader := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "rootsonly"))
	full := ContextWithFreshRead(reader)
	roots := ContextWithRootsOnlyRead(full)

	var fullResult, rootsResult *ExecuteResult
	fullCost := counter.count(func() {
		var err error
		fullResult, err = eng.Execute(full, query)
		require.NoError(t, err)
	})
	rootsCost := counter.count(func() {
		var err error
		rootsResult, err = eng.Execute(roots, query)
		require.NoError(t, err)
	})

	require.Less(t, rootsCost, fullCost, "the read expanded the relationships anyway")
	require.LessOrEqual(t, rootsCost, 2, "a point read is the row and its latest-version check")

	// THE SAME ROWS: only what nobody asked for is missing.
	require.Equal(t, MaterializeRows(fullResult), MaterializeRows(rootsResult))
	fullBundle, rootsBundle := fullResult.Bundle, rootsResult.Bundle
	require.Greater(t, len(fullBundle.GetNodes()), 1, "the premise failed: the row has no relationship to expand, so this proves nothing")
	require.Len(t, rootsBundle.GetNodes(), 1)
	require.Equal(t, fullBundle.GetRootIds(), rootsBundle.GetRootIds())

	// NOT ONE ANSWER FOR THE OTHER. A cached read of the shallow bundle must not stand in
	// for a read that wants the relationships, and the depth in the key is what stops it.
	cached := ContextWithRootsOnlyRead(reader)
	_, err := eng.Execute(cached, query)
	require.NoError(t, err)
	walked, err := eng.Execute(reader, query)
	require.NoError(t, err)
	require.Greater(t, len(walked.Bundle.GetNodes()), 1, "a roots-only read answered a read that wants the relationships from the cache")
}

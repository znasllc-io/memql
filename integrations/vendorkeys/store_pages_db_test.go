package vendorkeys

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// The reads that span owners must return every row, however many there are.
// One read of the engine returns a bounded page and says nothing when there
// were more rows, so the store walks the pages. A store that read one page
// would leave the keys past it unseen by the calls they should pay for, and
// unchecked for ever by the daily check.
func TestEveryActiveKeyIsReadPastOnePage(t *testing.T) {
	d := newWorld(t, "pages")
	ana := d.person("ana")
	var want []string
	for n := 0; n < 5; n++ {
		opts := []keyOption{atVendor(memql.VendorOpenAI)}
		if n > 0 {
			opts = append(opts, notTheDefault())
		}
		want = append(want, d.key(ana, fmt.Sprintf("p%d", n), opts...))
	}

	// A second engine over the same rows whose reads return two rows a page.
	t.Setenv("MEMQL_MEMORY_ENGINE_MAX_RESULTS", "2")
	small, smallDB, err := newEngine(sharedBoot.dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = smallDB.Close() })
	paged, _ := d.over(small)

	keys, err := paged.ActiveKeysAtVendor(context.Background(), memql.VendorOpenAI)
	require.NoError(t, err)
	var got []string
	for _, key := range keys {
		if strings.HasSuffix(key.Id, d.suffix) {
			got = append(got, key.Id)
		}
	}
	require.ElementsMatch(t, want, got, "keys past the first page were not read")

	states, err := paged.States(context.Background(), want)
	require.NoError(t, err)
	require.Len(t, states, len(want), "states past the first page were not read")
}

// A key's activity is the sum of every call it paid for this month. A busy key
// pays for more calls than one read returns, and a sum over the first page
// would be an undercount presented as the total.
func TestActivityCountsEveryCallPastOnePage(t *testing.T) {
	d := newDesk(t, "activitypages")
	dana := d.member("dana", auth.RoleDeveloper)
	wes := d.member("wes", auth.RoleWriter)
	ctx := session(dana, auth.RoleDeveloper)
	keyId := d.mustAdd(ctx, "Busy")["id"].(string)
	for n := 0; n < 5; n++ {
		d.recordCall(asTheRouter(), wes, keyId, dana, 0.10, true, "ok")
	}
	require.EqualValues(t, 5, d.activity(ctx, keyId)["calls"])

	// The same question on an engine whose reads return two rows a page.
	t.Setenv("MEMQL_MEMORY_ENGINE_MAX_RESULTS", "2")
	small, smallDB, err := newEngine(sharedBoot.dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = smallDB.Close() })
	store, resolver := d.over(small)
	paged := NewIntegration(store, resolver, nil, small, discardLogger())

	nodes, err := paged.handleActivity(ctx, map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)
	got := cardOf(t, nodes)
	require.EqualValues(t, 5, got["calls"], "calls past the first page were not counted")
	require.InDelta(t, 0.50, got["estimatedCost"], 1e-9)
}

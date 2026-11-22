package vendorkeys

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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

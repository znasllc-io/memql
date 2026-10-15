package readscale_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// The engine's reads measured at production scale. A package of its own so
// that seeding hundreds of thousands of rows spends this package's go test
// budget rather than component/memql's, which CI already runs close to its
// timeout (scripts/ci/selection: one -timeout per package).
//
// TestMain migrates the shared schema first, as component/memql's does: the
// scale fixtures copy public."MemoryNodes" -- its indexes included -- so they
// measure the indexes the migrations actually build.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

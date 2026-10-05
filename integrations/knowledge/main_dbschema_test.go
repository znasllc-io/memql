package knowledge

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"os"
	"testing"
)

// Join the provisioned DB lane and serialize migration before vector tests.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

package filebackups

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The narrow storage harness creates its own schemas and needs plain
	// PostgreSQL only. CI's shared engine DSN uses the usual migration barrier.
	if os.Getenv("MEMQL_STORAGE_TEST_DSN") == "" {
		if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

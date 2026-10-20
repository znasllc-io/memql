package database_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestReadinessRefusesMissingLivenessAfterPartialMigration(t *testing.T) {
	dsn := scratchDatabase(t)
	bootScratch(t, dsn)
	db := openScratch(t, dsn)
	var options database.Database
	service, err := memoryNodes.NewMemoryNodesDatabase(database.WithDSN(dsn), options.WithMigrateOnStart(false))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service.Start(ctx)
	defer service.Stop(context.Background())
	select {
	case <-service.Ready():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := service.AssertCriticalSchema(ctx); err != nil {
		t.Fatal(err)
	}
	execScratch(t, db, `ALTER TABLE memory_liveness RENAME TO test_missing_liveness`)
	if err := service.AssertCriticalSchema(ctx); err == nil || !strings.Contains(err.Error(), "memory_liveness") {
		t.Fatalf("missing live-read table reported ready: %v", err)
	}
	execScratch(t, db, `ALTER TABLE test_missing_liveness RENAME TO memory_liveness`)
	if err := service.AssertCriticalSchema(ctx); err != nil {
		t.Fatal(err)
	}
}

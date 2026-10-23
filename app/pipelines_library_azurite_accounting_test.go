package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// Real-provider proofs must enter through the same capacity admission as the
// running app. In particular, neither inventory readiness nor usage totals are
// fabricated: the native client inventories the actual disposable account.
func preparePipelineAzuriteAccounting(t *testing.T, engine *memql.MemQLEngine, db *sql.DB, uploader *azureblob.AzureBlobUploader, owner string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ownerCtx := auth.ContextWithUserActor(ctx, owner)
	templateMutation(t, engine, auth.ContextWithInternalOrigin(ownerCtx), "identity.createUserOnFirstLogin", map[string]any{
		"userId": owner, "displayName": "Artifact qualification owner", "primaryEmail": owner + "@example.test", "role": "owner", "internal": false,
	})
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE concept='v1:identity:user' AND (id=$1 OR id='v1:identity:user:' || $1)`, owner)
	})
	previous := azureblob.CapacityStore()
	azureblob.ConfigureCapacity(func() *sql.DB { return db }, func() *sql.DB { return db })
	t.Cleanup(func() {
		if previous == nil {
			azureblob.ConfigureCapacity(nil, nil)
		} else {
			azureblob.ConfigureCapacity(previous.DB, previous.GateDB)
		}
	})
	for {
		if _, err := uploader.StorageUsage(ownerCtx); !errors.Is(err, storagequota.ErrInventoryPending) {
			if err != nil {
				t.Fatal("native storage inventory/accounting", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("native storage inventory/accounting", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func assertPipelineAzuriteCharge(t *testing.T, uploader *azureblob.AzureBlobUploader, owner string, bytes int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	usage, err := uploader.StorageUsage(auth.ContextWithUserActor(ctx, owner))
	if err != nil || usage.Used != bytes+storagequota.ObjectOverhead || usage.Reserved != 0 {
		t.Fatalf("native artifact must be charged to its owner exactly once: %+v, %v", usage, err)
	}
}

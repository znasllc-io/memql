package azureblob

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/storagequota"
)

// The test owns one schema and two independent pools, matching the quota
// store's separate admission/gate connections. It never touches shared rows.
func nativeAssetAccountingDB(t *testing.T, account string) *storagequota.Store {
	t.Helper()
	dsn := os.Getenv("MEMQL_STORAGE_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("MEMQL_DATABASE_DSN")
	}
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("native shared asset accounting requires PostgreSQL")
		}
		t.Skip("PostgreSQL DSN is not configured")
	}
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	schema := fmt.Sprintf("native_assets_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("owned schema cleanup: %v", err)
		}
	})
	open := func() *sql.DB {
		db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db, gates := open(), open()
	store := &storagequota.Store{DB: func() *sql.DB { return db }, GateDB: func() *sql.DB { return gates }}
	migration, err := os.ReadFile(filepath.Join("..", "..", "component", "database", "memory-nodes", "migrations", "20261015010000_storage_capacity.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE "MemoryNodes"(id text,concept text,payload jsonb,"createdAt" timestamptz DEFAULT clock_timestamp()); INSERT INTO "MemoryNodes"(id,concept,payload) VALUES ('member','v1:identity:user','{"role":"writer"}')`); err != nil {
		t.Fatal(err)
	}
	if err = store.Initialize(ctx, account, func(func(string, string, int64) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	previous := capacityStore.Swap(store)
	t.Cleanup(func() { capacityStore.Store(previous) })
	return store
}

func nativeAssetRetentionResponse(r *http.Request) (*http.Response, bool) {
	if r.Method != http.MethodGet || r.URL.Query().Get("comp") != "properties" || r.URL.Query().Get("restype") != "service" {
		return nil, false
	}
	body := []byte(`<StorageServiceProperties><DeleteRetentionPolicy><Enabled>false</Enabled></DeleteRetentionPolicy></StorageServiceProperties>`)
	response := activeSetResponse(r, 200, body, "", "")
	response.Header.Set("Content-Type", "application/xml")
	return response, true
}

func TestNativeSharedAssetAccountingPreservesOwnerAndUncertainCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "member", Role: "writer"}), 30*time.Second)
	defer cancel()
	sharedFixture := &verifiedFixture{blocks: map[string][]byte{}}
	makeClient := func(f *verifiedFixture) *AzureBlobUploader {
		u := activeSetClient(t, func(r *http.Request) (*http.Response, error) {
			actual, _ := auth.AccessFromContext(r.Context())
			expected, _ := auth.AccessFromContext(ctx)
			if actual != expected {
				t.Fatal("accounting changed original caller")
			}
			if response, ok := nativeAssetRetentionResponse(r); ok {
				return response, nil
			}
			return f.do(r)
		})
		u.meterWrites = true
		return u
	}
	u := makeClient(sharedFixture)
	store := nativeAssetAccountingDB(t, u.client.URL())
	readLedger := func(object string) (string, int64, int64) {
		t.Helper()
		bc, err := u.blockClient("container", object)
		if err != nil {
			t.Fatal(err)
		}
		var owner string
		var used, capacity int64
		if err = store.DB().QueryRowContext(ctx, `SELECT owner_id,used_bytes,capacity_bytes FROM storage_capacity_objects WHERE object_key=$1`, bc.URL()).Scan(&owner, &used, &capacity); err != nil {
			t.Fatal(err)
		}
		return owner, used, capacity
	}
	body := []byte("shared content")
	sharedPath := "sites/site-one/v123456789abc/shared.html"
	if _, err := u.ClusterAssetsCreateVerifiedStream(ctx, "container", sharedPath, bytes.NewReader(body), int64(len(body)), verifiedSHA(body), "text/html"); err != nil {
		t.Fatal(err)
	}
	owner, used, capacity := readLedger(sharedPath)
	if owner != "" || used != int64(len(body))+storagequota.ObjectOverhead || capacity != used {
		t.Fatalf("shared ledger owner=%q used=%d capacity=%d", owner, used, capacity)
	}

	ordinaryFixture := &verifiedFixture{blocks: map[string][]byte{}}
	ordinary := makeClient(ordinaryFixture)
	ordinaryPath := "sites/site-one/v123456789abc/personal.html"
	if _, err := ordinary.CreateVerifiedStream(ctx, "container", ordinaryPath, bytes.NewReader(body), int64(len(body)), verifiedSHA(body), "text/html"); err != nil {
		t.Fatal(err)
	}
	owner, used, capacity = readLedger(ordinaryPath)
	if owner != "member" || used != int64(len(body))+storagequota.ObjectOverhead || capacity != used {
		t.Fatalf("ordinary ledger owner=%q used=%d capacity=%d", owner, used, capacity)
	}
	before := ordinaryFixture.writes
	if _, err := ordinary.ClusterAssetsCreateVerifiedStream(ctx, "container", ordinaryPath, bytes.NewReader(body), int64(len(body)), verifiedSHA(body), "text/html"); err == nil || !strings.Contains(err.Error(), "different owner") || ordinaryFixture.writes != before {
		t.Fatalf("personal object was adopted: %v writes=%d", err, ordinaryFixture.writes)
	}
	owner, _, _ = readLedger(ordinaryPath)
	if owner != "member" {
		t.Fatal("owner changed after refusal")
	}

	f := &activeSetFixture{}
	active := activeSetClient(t, func(r *http.Request) (*http.Response, error) {
		if response, ok := nativeAssetRetentionResponse(r); ok {
			return response, nil
		}
		if r.Method == http.MethodHead {
			if !f.exists {
				return activeSetResponse(r, 404, nil, "", "BlobNotFound"), nil
			}
			return activeSetResponse(r, 200, f.body, f.etag, ""), nil
		}
		return f.do(r)
	})
	active.meterWrites = true
	current, err := active.CompareAndSwapPackageActiveSet(ctx, "container", PackageActiveSetSnapshot{}, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	owner, oldUsed, oldCapacity := readLedger(packageActiveSetObject)
	if owner != "" || oldUsed != 2+storagequota.ObjectOverhead || oldCapacity != oldUsed {
		t.Fatalf("active set ledger owner=%q used=%d capacity=%d", owner, oldUsed, oldCapacity)
	}
	f.lostWrite = true
	replacement := []byte(`{"domain":"prefix/"}`)
	if _, err = active.CompareAndSwapPackageActiveSet(ctx, "container", current, replacement); !errors.Is(err, ErrBlobCommitUncertain) {
		t.Fatalf("lost reply: %v", err)
	}
	owner, used, capacity = readLedger(packageActiveSetObject)
	if owner != "" || used != oldUsed || capacity < oldUsed+int64(len(replacement)) {
		t.Fatalf("uncertain write released capacity owner=%q used=%d capacity=%d", owner, used, capacity)
	}
	observed, err := active.ReadPackageActiveSet(ctx, "container")
	if err != nil || !bytes.Equal(observed.Data, replacement) || f.puts != 2 {
		t.Fatalf("read-only reconciliation: %+v %v writes=%d", observed, err, f.puts)
	}
	_, afterUsed, afterCapacity := readLedger(packageActiveSetObject)
	if afterUsed != used || afterCapacity != capacity {
		t.Fatal("read-only observation changed uncertain accounting")
	}
}

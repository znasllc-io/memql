package packages

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"encoding/base64"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestExternalAssetCacheUsesAzureProtocolAndDeclaredReadBudget(t *testing.T) {
	assetCapacityFixture(t)
	var mu sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("comp") == "properties" && r.URL.Query().Get("restype") == "service" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<StorageServiceProperties><DeleteRetentionPolicy><Enabled>false</Enabled></DeleteRetentionPolicy></StorageServiceProperties>`)
			return
		}
		if r.URL.Query().Get("comp") == "list" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<EnumerationResults><Containers/><NextMarker/></EnumerationResults>`)
			return
		}
		if r.Method == http.MethodPut {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			objects[r.URL.Path] = raw
			w.Header().Set("ETag", `"asset"`)
			w.WriteHeader(http.StatusCreated)
			return
		}
		raw, ok := objects[r.URL.Path]
		if !ok {
			w.Header().Set("x-ms-error-code", "BlobNotFound")
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "AccountName=test;AccountKey="+key+";BlobEndpoint="+server.URL+"/test;")
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "memql")
	t.Setenv("MEMQL_AZURE_BLOB_AUTOCREATE", "false")
	ctx := context.Background()
	object := "packages/assets/package/digest"
	if _, found, err := (azureAssetCache{}).Read(ctx, object, 5); err != nil || found {
		t.Fatalf("empty cache: %v %v", found, err)
	}
	if err := (azureAssetCache{}).Write(ctx, object, []byte("movie")); err != nil {
		t.Fatal(err)
	}
	// New cache instance models another replica using the same object store.
	if got, found, err := (azureAssetCache{}).Read(ctx, object, 5); err != nil || !found || string(got) != "movie" {
		t.Fatalf("durable cache: %q %v %v", got, found, err)
	}
	if _, _, err := (azureAssetCache{}).Read(ctx, object, 4); err == nil {
		t.Fatal("cache read ignored declared byte limit")
	}
}

// Exercise the production metering constructor rather than giving the asset
// writer a test-only quota bypass. The schema belongs only to this fixture.
func assetCapacityFixture(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("MEMQL_STORAGE_TEST_DSN")
	if dsn == "" {
		dsn = dbtest.DSN()
	}
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := admin.PingContext(ctx)
	cancel()
	if err != nil {
		dbtest.Unreachable(t, "metered Azure package assets", dsn, err)
	}
	schema := fmt.Sprintf("asset_quota_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	open := func() *sql.DB {
		return sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
	}
	db, gates := open(), open()
	migration, err := os.ReadFile("../database/memory-nodes/migrations/20261015010000_storage_capacity.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	previous := azureblob.CapacityStore()
	azureblob.ConfigureCapacity(func() *sql.DB { return db }, func() *sql.DB { return gates })
	t.Cleanup(func() {
		if previous == nil {
			azureblob.ConfigureCapacity(nil, nil)
		} else {
			azureblob.ConfigureCapacity(previous.DB, previous.GateDB)
		}
		db.Close()
		gates.Close()
	})
}

package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// Exercise the production Azure client and durable capacity gate, rather than
// a FileUploader fake that accepts any object path. The protocol fixture owns
// an empty account; the database schema and both owners belong to this test.
func TestPipelineLogUsesItsOwnersMeteredStorage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "pipeline log storage accounting", dbtest.DSN(), err)
	}
	schema := "pipeline_log_" + strings.ReplaceAll(id.NewShortId(), "-", "_")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
	t.Cleanup(func() { _ = db.Close() })
	migration, err := os.ReadFile("../component/database/memory-nodes/migrations/20261015010000_storage_capacity.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE "MemoryNodes" (id text, concept text, payload jsonb, "createdAt" timestamptz DEFAULT now());
INSERT INTO "MemoryNodes" VALUES ('v1:identity:user:alice','v1:identity:user','{"role":"writer"}',now()), ('v1:identity:user:bob','v1:identity:user','{"role":"writer"}',now())`); err != nil {
		t.Fatal(err)
	}
	previous := azureblob.CapacityStore()
	azureblob.ConfigureCapacity(func() *sql.DB { return db }, func() *sql.DB { return db })
	t.Cleanup(func() {
		if previous == nil {
			azureblob.ConfigureCapacity(nil, nil)
		} else {
			azureblob.ConfigureCapacity(previous.DB, previous.GateDB)
		}
	})

	var mu sync.Mutex
	objects := map[string][]byte{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Query().Get("comp") == "properties":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<StorageServiceProperties><DeleteRetentionPolicy><Enabled>false</Enabled></DeleteRetentionPolicy></StorageServiceProperties>`)
		case r.URL.Query().Get("comp") == "list":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<EnumerationResults><Containers/><NextMarker/></EnumerationResults>`)
		case r.Method == http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			objects[r.URL.Path] = body
			w.Header().Set("ETag", `"fixture"`)
			w.WriteHeader(http.StatusCreated)
		default:
			body, ok := objects[r.URL.Path]
			if !ok {
				w.Header().Set("x-ms-error-code", "BlobNotFound")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", `"fixture"`)
			if r.Method == http.MethodGet {
				_, _ = w.Write(body)
			}
		}
	}))
	defer provider.Close()
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "DefaultEndpointsProtocol=http;AccountName=fixture;AccountKey="+base64.StdEncoding.EncodeToString([]byte("fixture-key"))+";BlobEndpoint="+provider.URL+"/fixture;")
	uploader, err := azureblob.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := auth.ContextWithUserActor(ctx, pipelinesTestOwner)
	// Inventory is deliberately asynchronous in production. Wait for the real
	// empty-account inventory; no test-only unmetered client or admission bypass.
	for {
		_, err := uploader.StorageUsage(ownerCtx)
		if !errors.Is(err, storagequota.ErrInventoryPending) {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	s, engine, _ := newPipelinesStoreForTest(t)
	s.uploader = uploader
	// A receiving node has an unrelated transport actor, not Alice's session.
	forwardCtx := auth.ContextWithUserActor(ctx, "v1:identity:user:bob")
	for _, owner := range []string{pipelinesTestOwner, "alice"} {
		f := pipelineLogFile()
		f.OwnerUserID = owner
		stored, err := s.StoreRunFile(forwardCtx, f)
		if err != nil || stored.FileID == "" || stored.Omitted != "" {
			t.Fatalf("owner %q: %+v, %v", owner, stored, err)
		}
		creates := engine.callsTo("createLibraryFile")
		url := pipelinesCallArg(t, creates[len(creates)-1].query, "blobUrl").(string)
		if !strings.Contains(url, "/library/alice/"+stored.FileID+"/") {
			t.Fatalf("log has the wrong storage owner: %s", url)
		}
		reader, err := azureblob.New(ctx)
		if err != nil {
			t.Fatal(err)
		}
		body, err := reader.DownloadURL(ownerCtx, url)
		if err != nil || string(body) != string(f.Bytes) {
			t.Fatalf("independent client readback: %q, %v", body, err)
		}
		if _, err := uploader.Upload(forwardCtx, "memql", "library/alice/forbidden/log", f.Bytes, f.MimeType); err == nil || !strings.Contains(err.Error(), "another user") {
			t.Fatalf("another user's write was not refused: %v", err)
		}
	}
	usage, err := uploader.StorageUsage(ownerCtx)
	want := 2 * (int64(len(pipelineLogFile().Bytes)) + storagequota.ObjectOverhead)
	if err != nil || usage.Used != want || usage.Reserved != 0 {
		t.Fatalf("Alice's retained logs were not charged once each: %+v, %v; want %d", usage, err, want)
	}
	other, err := uploader.StorageUsage(forwardCtx)
	if err != nil || other.Used != 0 || other.Reserved != 0 {
		t.Fatalf("transport actor was charged for Alice's logs: %+v, %v", other, err)
	}
}

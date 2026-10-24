package packages

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type sharedAssetObject struct {
	owner string
	body  []byte
}
type sharedAssetFixture struct {
	client    *azblob.Client
	uploader  *azureblob.AzureBlobUploader
	store     *storagequota.Store
	mu        sync.Mutex
	puts      int
	afterPut  func()
	retention bool
}

// Real Azure SDK and durable quota journal; only the provider transport is a
// loopback fixture. Every test owns the private schema from assetCapacityFixture.
func sharedAssetStorage(t *testing.T, objects map[string]sharedAssetObject) *sharedAssetFixture {
	t.Helper()
	assetCapacityFixture(t)
	f := &sharedAssetFixture{store: azureblob.CapacityStore()}
	if _, err := f.store.DB().Exec(`CREATE TABLE "MemoryNodes" (id text, concept text, payload jsonb, "createdAt" timestamptz DEFAULT clock_timestamp()); INSERT INTO "MemoryNodes" (id,concept,payload) VALUES ('v1:identity:user:person','v1:identity:user','{"role":"developer"}'), ('v1:identity:user:other','v1:identity:user','{"role":"writer"}')`); err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{}
	for key, object := range objects {
		bodies["/test/memql/"+key] = bytes.Clone(object.body)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Get("comp") == "properties" && r.URL.Query().Get("restype") == "service" {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<StorageServiceProperties><DeleteRetentionPolicy><Enabled>%t</Enabled></DeleteRetentionPolicy></StorageServiceProperties>`, f.retention)
			return
		}
		if r.Method == http.MethodPut {
			data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			bodies[r.URL.Path] = data
			f.puts++
			if f.afterPut != nil {
				f.afterPut()
			}
			w.Header().Set("ETag", `"shared"`)
			w.WriteHeader(201)
			return
		}
		data, ok := bodies[r.URL.Path]
		if !ok {
			w.Header().Set("x-ms-error-code", "BlobNotFound")
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", `"shared"`)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(server.Close)
	conn := "AccountName=test;AccountKey=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)) + ";BlobEndpoint=" + server.URL + "/test;"
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", conn)
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "memql")
	var err error
	f.client, err = azblob.NewClientFromConnectionString(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.uploader, err = azureblob.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.Initialize(context.Background(), f.client.URL(), func(add func(string, string, int64) error) error {
		for key, object := range objects {
			if err := add(f.key(key), object.owner, int64(len(object.body))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *sharedAssetFixture) key(object string) string {
	return f.client.ServiceClient().NewContainerClient("memql").NewBlockBlobClient(object).URL()
}
func (f *sharedAssetFixture) writes() int { f.mu.Lock(); defer f.mu.Unlock(); return f.puts }
func (f *sharedAssetFixture) state(t *testing.T, object string) string {
	t.Helper()
	var owner string
	var used, capacity, planned int64
	var retired bool
	err := f.store.DB().QueryRow(`SELECT owner_id,used_bytes,capacity_bytes,planned_bytes,retired FROM storage_capacity_objects WHERE object_key=$1`, f.key(object)).Scan(&owner, &used, &capacity, &planned, &retired)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s:%d:%d:%d:%t", owner, used, capacity, planned, retired)
}
func (f *sharedAssetFixture) assertOwner(t *testing.T, object, owner string) {
	t.Helper()
	var actual string
	var reserved int64
	err := f.store.DB().QueryRow(`SELECT owner_id,planned_bytes FROM storage_capacity_objects WHERE object_key=$1`, f.key(object)).Scan(&actual, &reserved)
	if err != nil || actual != owner || reserved != 0 {
		t.Fatalf("accounting %s owner=%q planned=%d: %v", object, actual, reserved, err)
	}
}
func sharedAssetCaller(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return auth.ContextWithToken(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "v1:identity:user:person", Role: auth.RoleDeveloper}), &auth.TokenInfo{Subject: "v1:identity:user:person"})
}

// This recorder measures the native package publisher's actor both before the
// first byte and at the final permission/pointer check. Organization membership
// itself is separately exercised by the existing real-engine organization gate.
type sharedAssetEngine struct {
	recordingEngine
	t       *testing.T
	caller  context.Context
	allowed atomic.Bool
}

func (e *sharedAssetEngine) assertActor(ctx context.Context) {
	e.t.Helper()
	got, ok := auth.AccessFromContext(ctx)
	want, _ := auth.AccessFromContext(e.caller)
	if !ok || got != want || got.Synthetic || ctx != e.caller {
		e.t.Error("shared accounting replaced the authenticated caller context")
	}
}
func (e *sharedAssetEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	e.assertActor(ctx)
	return e.recordingEngine.Execute(ctx, q)
}
func (e *sharedAssetEngine) OrganizationCapable(ctx context.Context, account, verb, resource string) bool {
	e.assertActor(ctx)
	return e.allowed.Load() && account == "client-org"
}
func sharedAssetPublisher(t *testing.T, f *sharedAssetFixture, ctx context.Context) (*enginePublisher, *sharedAssetEngine) {
	e := &sharedAssetEngine{t: t, caller: ctx, recordingEngine: recordingEngine{rows: map[string][]map[string]any{"query siteById": {{"id": "site-one", "accountId": "client-org"}}}}}
	e.allowed.Store(true)
	return &enginePublisher{engine: e, store: &store{engine: e}, uploader: f.uploader, container: "memql"}, e
}

type sharedAssetPointer struct{}

func (sharedAssetPointer) PointVersion(context.Context, string, edge.Target, string) error {
	return nil
}
func sharedSiteKey(t *testing.T, bundle edge.Bundle) string {
	t.Helper()
	out, err := edge.NewPublisher(&organizationRecordingBlobs{}, sharedAssetPointer{}).Publish(context.Background(), "site-one", bundle, edge.TargetServing)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(out.BundleRef, "blob://") + "index.html"
}

func TestPackageSharedAssetsUseClusterAccountingAcrossReplicas(t *testing.T) {
	tree := fstest.MapFS{"agents.memql": {Data: []byte("fixture DSL")}}
	prefix, err := (&blobStager{}).PrefixFor("example", tree)
	if err != nil {
		t.Fatal(err)
	}
	bundle := edge.Bundle{"index.html": []byte("<!doctype html><title>Shared</title>")}
	siteKey := sharedSiteKey(t, bundle)
	f := sharedAssetStorage(t, map[string]sharedAssetObject{prefix + "agents.memql": {body: []byte("fixture DSL")}, ActiveSetPath: {body: []byte(`{"old":"packages/old/"}`)}, siteKey: {body: bundle["index.html"]}})
	ctx := sharedAssetCaller(t)
	// The prior route demonstrably refuses the existing cluster-owned object.
	if _, err := f.uploader.Upload(ctx, "memql", prefix+"agents.memql", tree["agents.memql"].Data, "text/plain"); err == nil || !strings.Contains(err.Error(), "different owner") {
		t.Fatalf("original accounting did not reproduce collision: %v", err)
	}
	if f.writes() != 0 {
		t.Fatal("owner refusal reached provider")
	}
	stager := &blobStager{uploader: f.uploader, container: "memql"}
	if _, err := stager.StageDomain(ctx, "example", tree); err != nil {
		t.Fatal(err)
	}
	if err := stager.WriteActiveSet(ctx, map[string]string{"example": prefix}); err != nil {
		t.Fatal(err)
	}
	// Two new clients with no originating request state share only durable
	// accounting. Repeated concurrent content writes must charge bytes once.
	before := f.state(t, prefix+"agents.memql")
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		up, err := azureblob.New(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := (&blobStager{uploader: up, container: "memql"}).StageDomain(ctx, "example", tree)
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.state(t, prefix+"agents.memql"); got != before {
		t.Fatalf("replica replay changed accounting: %s -> %s", before, got)
	}
	pub, engine := sharedAssetPublisher(t, f, ctx)
	if _, err := pub.PublishBundle(ctx, "site-one", bundle, edge.TargetServing); err != nil {
		t.Fatal(err)
	}
	if !engine.sawStatement("mutation updateSiteBundle") {
		t.Fatal("authorized site pointer did not update")
	}
	for _, key := range []string{prefix + "agents.memql", ActiveSetPath, siteKey} {
		f.assertOwner(t, key, "")
	}
	// A new shared domain is assigned to the same cluster scope, rather than
	// merely accepting already inventoried objects.
	freshPrefix, err := stager.StageDomain(ctx, "additional", tree)
	if err != nil {
		t.Fatal(err)
	}
	f.assertOwner(t, freshPrefix+"agents.memql", "")
	// StoreSnapshot deliberately retains the ordinary user-accounted route.
	if _, err := pub.StoreSnapshot(ctx, "package-one", "source-one", []byte("snapshot")); err != nil {
		t.Fatal(err)
	}
	f.assertOwner(t, "packages/snapshots/package-one/source-one.tar.gz", "person")
	usage, err := f.uploader.StorageUsage(ctx)
	if err != nil || usage.Used != int64(len("snapshot"))+storagequota.ObjectOverhead || usage.Reserved != 0 {
		t.Fatalf("shared bytes charged to caller: %+v %v", usage, err)
	}
}

func TestPackageSharedAssetsNeverAdoptPersonalLedgerRows(t *testing.T) {
	tree := fstest.MapFS{"agents.memql": {Data: []byte("fixture DSL")}}
	prefix, err := (&blobStager{}).PrefixFor("example", tree)
	if err != nil {
		t.Fatal(err)
	}
	bundle := edge.Bundle{"index.html": []byte("<!doctype html><title>Refuse</title>")}
	siteKey := sharedSiteKey(t, bundle)
	for _, owner := range []string{"person", "other"} {
		for _, kind := range []string{"dsl", "pointer", "site"} {
			t.Run(owner+"/"+kind, func(t *testing.T) {
				key := map[string]string{"dsl": prefix + "agents.memql", "pointer": ActiveSetPath, "site": siteKey}[kind]
				f := sharedAssetStorage(t, map[string]sharedAssetObject{key: {owner: owner, body: []byte("retained")}})
				before := f.state(t, key)
				ctx := sharedAssetCaller(t)
				stager := &blobStager{uploader: f.uploader, container: "memql"}
				pub, engine := sharedAssetPublisher(t, f, ctx)
				var err error
				switch kind {
				case "dsl":
					_, err = stager.StageDomain(ctx, "example", tree)
				case "pointer":
					err = stager.WriteActiveSet(ctx, map[string]string{"example": prefix})
				case "site":
					_, err = pub.PublishBundle(ctx, "site-one", bundle, edge.TargetServing)
				}
				if err == nil || !strings.Contains(err.Error(), "different owner") {
					t.Fatalf("personal ledger accepted: %v", err)
				}
				if f.writes() != 0 || engine.sawStatement("mutation updateSiteBundle") || f.state(t, key) != before {
					t.Fatal("refused owner was written, adopted or re-pointed")
				}
			})
		}
	}
}

func TestPackageSharedSiteAccountingPreservesAuthorization(t *testing.T) {
	f := sharedAssetStorage(t, nil)
	ctx := sharedAssetCaller(t)
	pub, engine := sharedAssetPublisher(t, f, ctx)
	bundle := edge.Bundle{"index.html": []byte("<!doctype html><title>Authority</title>")}
	engine.allowed.Store(false)
	if _, err := pub.PublishBundle(ctx, "site-one", bundle, edge.TargetServing); err == nil || f.writes() != 0 {
		t.Fatalf("denied site reached storage: %v", err)
	}
	engine.allowed.Store(true)
	f.afterPut = func() { engine.allowed.Store(false) }
	if _, err := pub.PublishBundle(ctx, "site-one", bundle, edge.TargetServing); err == nil || f.writes() != 1 {
		t.Fatalf("revoked post-upload authority was not rechecked: %v", err)
	}
	if engine.sawStatement("mutation updateSiteBundle") {
		t.Fatal("revoked site was pointed at uploaded bytes")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	before := f.writes()
	if _, err := f.uploader.ClusterAssets().Upload(canceled, "memql", ActiveSetPath, []byte("{}"), "application/json"); err == nil || f.writes() != before {
		t.Fatalf("canceled shared write escaped its context: %v", err)
	}
}

func TestPackageSharedAssetsRetainCapacityAndRetentionGates(t *testing.T) {
	for _, kind := range []string{"capacity", "retention"} {
		t.Run(kind, func(t *testing.T) {
			f := sharedAssetStorage(t, nil)
			ctx := sharedAssetCaller(t)
			if kind == "capacity" {
				g, err := f.store.Guard(ctx, f.client.URL(), f.key("fixture-capacity"), "")
				if err != nil {
					t.Fatal(err)
				}
				err = g.Plan(ctx, storagequota.ClusterBytes-storagequota.ObjectOverhead)
				g.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				f.retention = true
			}
			_, err := f.uploader.ClusterAssets().Upload(ctx, "memql", ActiveSetPath, []byte("{}"), "application/json")
			if err == nil || (kind == "capacity" && !errors.Is(err, storagequota.ErrLimit)) || (kind == "retention" && !strings.Contains(err.Error(), "soft deletion")) || f.writes() != 0 {
				t.Fatalf("shared %s gate: %v, writes=%d", kind, err, f.writes())
			}
		})
	}
}

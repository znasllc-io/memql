package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/gatecontext"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// Use the App's actual wiring: a test that assigns uploadDB itself would miss
// the production regression, where journal gates took the request pool. Each
// pool has one connection, so holding either while requesting it again cannot
// accidentally succeed just because a fixture has an unbounded pool.
func pipelineGatePoolStore(t *testing.T) (*pipelinesLibraryStore, *sql.DB, *sql.DB) {
	t.Helper()
	_, engine, request := pipelineStreamDBStore(t, &pipelineStreamFake{})
	gate, err := database.NewGatePool(dbtest.DSN(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	request.SetMaxOpenConns(1)
	engine.SetAskCoordinationPool(func() *sql.DB { return gate })
	a := &App{engine: engine, db: &memorynodes.MemoryNodesDatabase{
		TimescaleDBDatabase: &database.TimescaleDBDatabase{PostgresDatabase: &database.PostgresDatabase{
			Database: &database.Database{DB: request, Bun: bun.NewDB(request, pgdialect.New()), Gate: gate},
		}},
	}}
	store := a.pipelinesLibraryStoreFor(&pipelineStreamFake{}, "stream-gate-test").(*pipelinesLibraryStore)
	return store, request, gate
}

type pipelineQuotaGateUploader struct {
	pipelineStreamFake
	quota  *storagequota.Store
	ctx    context.Context
	called bool
}

func (u *pipelineQuotaGateUploader) ReserveStorage(ctx context.Context, account, key string, size int64) error {
	u.ctx, u.called = ctx, true
	access, _ := auth.AccessFromContext(ctx)
	guard, err := u.quota.Guard(ctx, account, key, access.UserId)
	if err != nil {
		return err
	}
	defer guard.Close()
	return guard.Plan(ctx, size)
}

func (u *pipelineQuotaGateUploader) CreateVerifiedStream(ctx context.Context, bucket, object string, source io.Reader, size int64, digest, mime string) (azureblob.VerifiedBlob, error) {
	if gatecontext.Transaction(ctx, u.quota.GateDB()) != nil {
		return azureblob.VerifiedBlob{}, errors.New("reservation transaction leaked into the independent blob transfer")
	}
	return u.pipelineStreamFake.CreateVerifiedStream(ctx, bucket, object, source, size, digest, mime)
}

func TestPipelineArtifactGatePoolSupportsNestedQuotaAndEngineWork(t *testing.T) {
	store, request, gate := pipelineGatePoolStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	uploader := &pipelineQuotaGateUploader{quota: &storagequota.Store{
		DB: func() *sql.DB { return request }, GateDB: func() *sql.DB { return gate },
	}}
	store.uploader = uploader
	store.bucket = "pipeline-gate-" + id.NewShortId()
	if err := uploader.quota.Initialize(ctx, store.bucket, func(func(string, string, int64) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	file := pipelineStreamFile(id.NewShortId(), "nested quota and Library work")
	if _, err := request.ExecContext(ctx, `INSERT INTO "MemoryNodes"
(id,concept,type,schema,payload,"createdAt","createdBy")
VALUES($1,'v1:identity:user','object','{}','{"role":"writer"}',clock_timestamp(),$1)`, "v1:identity:user:"+file.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	// A native capability reached synchronously by engine execution may need
	// another gate. Exercise that path on the actual Library writes without
	// requiring a second free connection or replacing their SQL with a fake.
	delegate := store.engine
	var finalizerCtx context.Context
	store.engine = pipelineStreamEngineFunc(func(ctx context.Context, query string) (any, error) {
		finalizerCtx = ctx
		hold, err := memql.AcquireGateHold(ctx, gate, time.Second, "pipeline-finalizer-test:"+file.OwnerUserID)
		if err != nil {
			return nil, err
		}
		defer hold.Release()
		return delegate.Execute(ctx, query)
	})
	store.library = server.NewEngineLibraryStore(store.engine)
	got, err := store.StoreRunFileStream(ctx, file)
	if err != nil || got.FileID == "" || got.Receipt == nil {
		t.Fatalf("one connection per pool must finish the quota and Library write: %+v %v", got, err)
	}
	if !uploader.called || gatecontext.Transaction(uploader.ctx, gate) == nil {
		t.Fatal("reservation did not lend its gate transaction to storage quota")
	}
	// The reservation context cannot open a fresh gate after the owner commits.
	// That would let delayed work outlive the journal fence it relied on.
	if _, err := uploader.quota.Guard(uploader.ctx, store.bucket, "late-"+id.NewShortId(), file.OwnerUserID); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("ended reservation context acquired an unrelated gate: %v", err)
	}
	if _, err := memql.AcquireGateHold(finalizerCtx, gate, time.Second, "late-finalizer"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("ended finalizer context acquired an unrelated gate: %v", err)
	}
	if gate.Stats().InUse != 0 || request.Stats().InUse != 0 {
		t.Fatal("finished upload retained a database connection")
	}
}

func TestPipelineArtifactGatePoolConcurrentOwnersKeepRequestConnectionAvailable(t *testing.T) {
	for _, sameOwner := range []bool{true, false} {
		t.Run(map[bool]string{true: "same owner", false: "different owners"}[sameOwner], func(t *testing.T) {
			store, request, gate := pipelineGatePoolStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			const count = 6
			owner := id.NewShortId()
			start := make(chan struct{})
			results := make(chan error, count)
			var workers sync.WaitGroup
			for range count {
				fileOwner := owner
				if !sameOwner {
					fileOwner = id.NewShortId()
				}
				file := pipelineStreamFile(fileOwner, "parallel artifact")
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					got, err := store.StoreRunFileStream(ctx, file)
					if err == nil && (got.FileID == "" || got.Receipt == nil) {
						err = errors.New("successful upload omitted its receipt")
					}
					results <- err
				}()
			}
			close(start)
			workers.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Errorf("parallel upload starved its own work: %v", err)
				}
			}
			if gate.Stats().WaitCount == 0 {
				t.Error("fixture did not exercise gate-pool contention")
			}
			if request.Stats().InUse != 0 || gate.Stats().InUse != 0 {
				t.Error("parallel uploads leaked database connections")
			}
		})
	}
}

func TestPipelineArtifactJournalRefusesMissingGatePool(t *testing.T) {
	// A usable request handle must never become the fallback for a missing
	// coordination pool. No actual connection is needed for this wiring test.
	request := &sql.DB{}
	a := &App{db: &memorynodes.MemoryNodesDatabase{
		TimescaleDBDatabase: &database.TimescaleDBDatabase{PostgresDatabase: &database.PostgresDatabase{
			Database: &database.Database{DB: request, Bun: bun.NewDB(request, pgdialect.New())},
		}},
	}}
	store := a.pipelinesLibraryStoreFor(&pipelineStreamFake{}, "stream-test").(*pipelinesLibraryStore)
	if db, err := store.pipelineReceiptDB(); err == nil || db != nil {
		t.Fatal("missing gate pool fell back to the request pool")
	}
}

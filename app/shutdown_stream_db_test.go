package app

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Use the real engine/database getter and real bidirectional TCP transport.
// Only authentication is fixed to a fixture owner; no query handler is mocked.
func shutdownStreamFixture(t *testing.T) (*App, *bun.DB, memqlv1.MemqlService_StreamClient) {
	t.Helper()
	t.Setenv("MEMQL_GRPC_TLS_CERT_FILE", "")
	t.Setenv("MEMQL_GRPC_TLS_KEY_FILE", "")
	engine, pool := workTemplateDBEngineAndDB(t)
	db, err := memorynodes.NewMemoryNodesDatabase(database.WithDSN(dbtest.DSN()))
	require.NoError(t, err)
	// TestMain migrated the isolated database. Attach this real pool without
	// starting another migration/monitor; ClosePool is the production close.
	db.DB, db.Bun, db.Logger = pool.DB, pool, engine.Logger
	engine.SetDatabaseGetter(db.BunDB)
	srv := memqlgrpc.NewServer(loopbackAddress(t), engine.Logger)
	srv.SetEngine(engine)
	srv.SetStreamInterceptor(memqlgrpc.NewLocalDevStreamInterceptor(nil))
	srv.Start(context.Background())
	t.Cleanup(func() { srv.Stop(context.Background()); db.ClosePool(context.Background()) })
	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("public stream server did not start")
	}
	conn, err := grpc.NewClient(srv.Address(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	stream, err := memqlv1.NewMemqlServiceClient(conn).Stream(ctx)
	require.NoError(t, err)
	shutdownSendQuery(t, stream, "before", `concept==v1:cluster:node`)
	shutdownReadQuery(t, stream, "before")
	return &App{Logger: engine.Logger, db: db, engine: engine, grpcServer: srv, Dependencies: []common.Dependency{srv}}, pool, stream
}

func shutdownSendQuery(t *testing.T, stream memqlv1.MemqlService_StreamClient, requestID, source string) {
	t.Helper()
	require.NoError(t, stream.Send(&memqlv1.MemqlClientMessage{MessageId: requestID, Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
		ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: requestID, Query: source},
	}}))
}

func shutdownReadQuery(t *testing.T, stream memqlv1.MemqlService_StreamClient, requestID string) {
	t.Helper()
	for {
		message, err := stream.Recv()
		require.NoError(t, err)
		if failure := message.GetQueryError(); failure != nil && failure.GetRequestId() == requestID {
			t.Fatalf("query %s failed: %s", requestID, failure.GetError().GetMessage())
		}
		if result := message.GetQueryResult(); result != nil && result.GetRequestId() == requestID && result.GetDone() {
			return
		}
	}
}

func TestShutdownDBClosesHeldPublicStreamBeforePoolRelease(t *testing.T) {
	for _, budget := range []time.Duration{8 * time.Second, 20 * time.Millisecond} {
		t.Run(budget.String(), func(t *testing.T) {
			a, _, stream := shutdownStreamFixture(t)
			peer, _, peerStream := shutdownStreamFixture(t)
			started := time.Now()
			a.releaseDatabasePools(budget)
			require.Less(t, time.Since(started), budget+time.Second, "held stream must not defeat bounded shutdown")
			require.Nil(t, a.db.BunDB(), "the connection pool must actually be released")
			// A browser can send another command on its existing connection after
			// endpoint removal. It must now see a closed transport, never execute
			// against a nil database. Distinct source avoids the warmed cache.
			_ = stream.Send(&memqlv1.MemqlClientMessage{MessageId: "after", Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
				ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: "after", Query: `paginate(concept==v1:cluster:node, 1)`},
			}})
			for {
				message, err := stream.Recv()
				if err != nil {
					break
				}
				if failure := message.GetQueryError(); failure != nil {
					t.Fatalf("pool was closed under an admitted stream: %s", failure.GetError().GetMessage())
				}
				if result := message.GetQueryResult(); result != nil && result.GetRequestId() == "after" {
					t.Fatal("a request reached the engine after database teardown")
				}
			}
			// The other replica serves the same database through its own pool.
			// Stopping this generation must not interrupt that peer's requests.
			shutdownSendQuery(t, peerStream, "peer-after", `paginate(concept==v1:cluster:node, 1)`)
			shutdownReadQuery(t, peerStream, "peer-after")
			require.NotNil(t, peer.db.BunDB())
			// The ordinary dependency sweep may call Stop again.
			DefaultStopDependencies(context.Background(), a.Dependencies...)
		})
	}
}

type shutdownQueryHold struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (h *shutdownQueryHold) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	if h.armed.CompareAndSwap(true, false) {
		close(h.entered)
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*shutdownQueryHold) AfterQuery(context.Context, *bun.QueryEvent) {}

func TestShutdownDBLetsAcceptedQueryFinishDuringInflightGrace(t *testing.T) {
	a, pool, stream := shutdownStreamFixture(t)
	hold := &shutdownQueryHold{entered: make(chan struct{}), release: make(chan struct{})}
	pool.AddQueryHook(hold)
	hold.armed.Store(true)
	shutdownSendQuery(t, stream, "inflight", `paginate(concept==v1:cluster:node, 2)`)
	select {
	case <-hold.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted request did not reach the real database")
	}
	// Exercise the canonical signal -> drain -> grace -> pool -> dependencies
	// path, while one accepted SQL query is deliberately held at its boundary.
	draining, finished, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.nodeIdentity = &node.Identity{ID: "shutdown-query-fixture", Type: node.NodeTypeBFF}
	a.eventBus = events.NewBus(events.WithLogger(a.Logger))
	unsubscribe := a.eventBus.Subscribe(events.TopicSystemStartup, func(events.Event) { close(started) })
	defer unsubscribe()
	var active atomic.Int64
	active.Store(1)
	go func() {
		runApplication(RunConfig{
			Logger: a.Logger, ShutdownTimeout: 8 * time.Second, DrainDelay: -1, GracePeriod: 3 * time.Second,
			Start: func(...common.Dependency) {},
			WaitForSignal: func(ctx context.Context) os.Signal {
				select {
				case <-started:
					return syscall.SIGTERM
				case <-ctx.Done():
					return nil
				}
			},
			WaitForReady: func(context.Context, []common.Dependency) error { return nil },
			BeginDrain:   func() { close(draining) }, ActiveWork: active.Load,
		}, a)
		close(finished)
	}()
	<-draining
	require.NotNil(t, a.db.BunDB(), "in-flight grace must retain the database")
	close(hold.release)
	shutdownReadQuery(t, stream, "inflight")
	require.NotNil(t, a.db.BunDB(), "accepted query must complete before pool teardown")
	require.NoError(t, stream.CloseSend())
	active.Store(0)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("completed request did not let shutdown finish")
	}
	require.Nil(t, a.db.BunDB())
}

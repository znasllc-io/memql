package magiclink

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestBootstrapFinishNeverConsumesWithoutCoordination(t *testing.T) {
	v, engine, _ := newFloorVerifier("owner")
	engine.oauthCtx = `{"bootstrap":true,"adminSession":true}`
	if _, err := finishOnce(v); err == nil {
		t.Fatal("bootstrap finished without a database gate")
	}
	if engine.consumeCalls != 0 {
		t.Fatal("link was spent before ownership coordination was available")
	}
}

type bootstrapFixture struct {
	mu      sync.Mutex
	claimed bool
	owners  int
}

type bootstrapReplicaEngine struct {
	*mlFakeEngine
	shared *bootstrapFixture
	link   string
}

func (e *bootstrapReplicaEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	empty := &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}
	switch {
	case strings.Contains(query, "clusterSettingsCurrent("):
		e.shared.mu.Lock()
		claimed := e.shared.claimed
		e.shared.mu.Unlock()
		stamp := ""
		if claimed {
			stamp = time.Now().UTC().Format(time.RFC3339)
		}
		payload, _ := structpb.NewStruct(map[string]any{"id": "cluster", "bootstrappedAt": stamp})
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: "cluster", Payload: payload}}}}, nil
	case strings.Contains(query, "userByEmail("):
		return empty, nil
	case strings.Contains(query, "createUserOnFirstLogin("):
		// A real read/write gap: both replicas would create owners without the gate.
		time.Sleep(80 * time.Millisecond)
		e.shared.mu.Lock()
		e.shared.owners++
		e.shared.mu.Unlock()
		return empty, nil
	case strings.Contains(query, "updateClusterSettings("):
		e.shared.mu.Lock()
		e.shared.claimed = true
		e.shared.mu.Unlock()
		return empty, nil
	}
	result, err := e.mlFakeEngine.Execute(ctx, query)
	if strings.Contains(query, "magicLinkRequestById(") && err == nil {
		result.Bundle.Nodes[0].Id = e.link
		result.Bundle.Nodes[0].Payload.Fields["id"] = structpb.NewStringValue(e.link)
	}
	return result, err
}

func TestDifferentBootstrapLinksAcrossReplicasCreateOnlyOneOwner(t *testing.T) {
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "cross-replica ownership", dbtest.DSN(), err)
		return
	}
	shared := &bootstrapFixture{}
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	for _, email := range []string{"one@example.test", "two@example.test"} {
		engine := &bootstrapReplicaEngine{mlFakeEngine: &mlFakeEngine{email: email, oauthCtx: `{"bootstrap":true,"adminSession":true}`}, shared: shared, link: email}
		verifier := &Verifier{Store: &identity.Store{Engine: engine, DirectDB: func() *sql.DB { return db }}}
		go func() { <-start; _, err := verifier.Finish(ctx, FinishInput{RequestId: email}); outcomes <- err }()
	}
	close(start)
	won := 0
	for range 2 {
		if err := <-outcomes; err == nil {
			won++
		}
	}
	if won != 1 || shared.owners != 1 {
		t.Fatalf("successful claims=%d owner creations=%d; want one each", won, shared.owners)
	}
}

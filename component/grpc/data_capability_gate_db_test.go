package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/provenance"
)

// TestExecuteQueryGate_AgainstLiveDatabase is the AC 5 end-to-end proof against
// a REAL database: the same DSL mutation, run through the same request-path
// handler, is refused for a reader and lands a row for a writer.
//
// Skips (via openWireTestDB -> dbtest.Unreachable) when no Postgres is
// reachable. The DB-less sibling in data_capability_gate_test.go still
// exercises the handler path and the refusal in every environment; this test
// is what proves the ADMITTED half actually writes.
func TestExecuteQueryGate_AgainstLiveDatabase(t *testing.T) {
	db := openWireTestDB(t)
	ctx := provenance.ContextWithProvenance(context.Background(), provenance.Direct("test:#3179"))

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))

	const ownerId = "v1:identity:user:gate-3179-db"
	const readerAgentShort = "gate-3179-reader"
	const writerAgentShort = "gate-3179-writer"
	const readerAgentId = "v1:agents:agent:" + readerAgentShort
	const writerAgentId = "v1:agents:agent:" + writerAgentShort

	seedUserRow(ctx, t, db, ownerId)
	t.Cleanup(func() {
		for _, rowId := range []string{ownerId, readerAgentId, writerAgentId} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).
				Where("id = ?", rowId).Exec(context.Background())
		}
	})

	newSession := func(role auth.Role) (*streamSession, *captureStream) {
		logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		svc := &service{engine: eng, logger: logger}
		cs := newCaptureStream(t)
		cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: ownerId})
		return &streamSession{
			service:      svc,
			stream:       cs,
			logger:       logger,
			access:       &auth.AccessContext{UserId: ownerId, Role: role},
			accessLoaded: true,
		}, cs
	}

	mutation := func(short string) string {
		return fmt.Sprintf(`createAgent(agentId: %q, ownerUserId: %q, name: "GateTest")`, short, ownerId)
	}

	rowExists := func(rowId string) bool {
		n, countErr := db.NewSelect().Model((*concept.MemoryNode)(nil)).
			Where("id = ?", rowId).Count(context.Background())
		require.NoError(t, countErr)
		return n > 0
	}

	// READER: refused, and nothing written.
	readerSession, readerStream := newSession(auth.RoleReader)
	driveGateQuery(t, readerSession, "db-reader", mutation(readerAgentShort))
	qe := queryErrorFor(readerStream, "db-reader")
	require.NotNil(t, qe, "reader must be refused the mutation")
	require.Equal(t, codes.PermissionDenied.String(), qe.GetCode(), "refusal message: %s", qe.GetMessage())
	require.False(t, rowExists(readerAgentId), "a refused mutation must not reach the database")

	// WRITER: admitted, and the row lands.
	writerSession, writerStream := newSession(auth.RoleWriter)
	driveGateQuery(t, writerSession, "db-writer", mutation(writerAgentShort))
	_ = waitForQueryResult(t, writerStream, "db-writer", 8*time.Second)
	require.True(t, rowExists(writerAgentId), "an admitted mutation must reach the database")

	// The bootstrap call shape -- Engine.Execute, context.Background(), no
	// AccessContext -- still works against the live database. AC 3: the check
	// is not on that path, so there is nothing to bypass.
	_, err = eng.Execute(context.Background(), "concept==v1:cluster:node")
	require.NoError(t, err, "node bootstrap's Engine.Execute call must stay ungated")
}

package pipelinehop

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/emailrules"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/outbound"
)

type deliveryConsumerEngine struct{ *memql.MemQLEngine }

func (e deliveryConsumerEngine) Execute(ctx context.Context, query string) (any, error) {
	return e.MemQLEngine.Execute(ctx, query)
}

func TestDeliveryConsumersKeepTheirOperationalAuthority(t *testing.T) {
	ctx := t.Context()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "delivery consumer authority", dbtest.DSN(), err)
	}
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memql.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	adapter := deliveryConsumerEngine{eng}
	id := fmt.Sprintf("delivery-consumers-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id IN (?, ?)",
			"v1:platform:inboundRequest:"+id, "v1:platform:outboundRequest:"+id).Exec(context.Background())
		require.NoError(t, err)
	})

	// The receiver's server-only stage still works. Datasync reads the exact
	// signed body as its existing operator identity, with no new bypass.
	receiver := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "inbound"))
	_, err = eng.Execute(receiver, fmt.Sprintf(`mutation stageInboundRequest(requestId: %s, source: "delivery-test", medium: "webhook", body: "private body", signatureVerified: true, receivedAt: "2026-10-05T12:00:00Z")`, langparser.QuoteString(id)))
	require.NoError(t, err)
	staged, found, err := datasync.NewStore(adapter).StagedInboundRequest(datasync.OperatorContext(ctx), id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "private body", string(staged.Body))

	// A validated email rule acts for its author, who need not be a cluster
	// operator. Its bounded staging write retains that provenance without
	// granting the author permission to read the outbox.
	author := auth.ContextWithUserActor(ctx, id+"-author")
	require.NoError(t, emailrules.NewStore(adapter).StageOutbound(author, id, "recipient@example.test", "Notice", "private body", id, id+"-rule"))
	res, err := eng.Execute(author, `query outboundRequestsByStatus(status: "pending")`)
	require.NoError(t, err)
	require.Empty(t, res.Bundle.Nodes)

	// Use the actual drain actor helper and query, not an invented owner.
	worker := outbound.SystemActorContext(ctx)
	res, err = eng.Execute(worker, `query outboundRequestsByStatus(status: "pending")`)
	require.NoError(t, err)
	require.NotEmpty(t, res.Bundle.Nodes)
	_, err = eng.Execute(auth.ContextWithInternalOrigin(worker), fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "sent")`, langparser.QuoteString(id)))
	require.NoError(t, err)
}

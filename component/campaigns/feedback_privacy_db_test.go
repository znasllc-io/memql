package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestFeedbackAutomationReadsPrivateDeliveryAndKeepsClientRefusal(t *testing.T) {
	ctx := t.Context()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "feedback delivery privacy", dbtest.DSN(), err)
	}
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memql.New(db)
	require.NoError(t, err)
	eng.Logger = quietLogger()
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	id := fmt.Sprintf("feedback-private-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", "v1:platform:inboundRequest:"+id).Exec(context.Background())
		require.NoError(t, err)
	})
	operator := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "inbound"))
	_, err = eng.Execute(operator, fmt.Sprintf(`mutation stageInboundRequest(requestId: %s, source: "feedback-privacy", medium: "webhook", body: "private body", signatureVerified: true, receivedAt: "2026-10-05T12:00:00Z")`, langparser.QuoteString(id)))
	require.NoError(t, err)
	w := &Worker{store: NewStore(deliveryDBEngine{eng}), logger: quietLogger()}
	reader := auth.ContextWithUserActor(ctx, id+"-reader")
	_, found, err := w.store.InboundRequestByID(reader, id)
	require.NoError(t, err)
	require.False(t, found)

	// No configured feedback format: the handler must read the real row and
	// report the unrelated source, without processing it. The ordinary
	// automation actor cannot do that lookup itself after the privacy tier.
	args := map[string]any{"inboundRequestId": id}
	rows, err := w.handleIngestFeedback(auth.ContextWithInternalOrigin(reader), args, 0)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	_, err = w.handleIngestFeedback(reader, args, 0)
	require.ErrorIs(t, err, errIngestFeedbackClientOrigin)
}

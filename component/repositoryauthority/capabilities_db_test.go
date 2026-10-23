package repositoryauthority

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// Integration methods can return correct receipts while their builtin envelope
// disappears in nodesToMap. Exercise the installed DSL and real engine-to-wire
// conversion, including a second replica reading the durable request.
func TestAuthorityBuiltinReceiptsReachTheEngineWire(t *testing.T) {
	f := newFixture(t)
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	db := bun.NewDB(f.i.deps().DB, pgdialect.New())
	engines := make([]*memql.MemQLEngine, 2)
	for n := range engines {
		engines[n], err = memql.New(db)
		require.NoError(t, err)
		engines[n].Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		require.NoError(t, engines[n].Init(memorynodes.DefaultRegistry()))
		require.NoError(t, engines[n].RegisterIntegration(New(f.i.deps())))
	}
	seenEnvelopes := map[string]bool{}
	read := func(engine *memql.MemQLEngine, ctx context.Context, name string, args map[string]any) map[string]any {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		before := time.Now().UTC()
		result, err := engine.Execute(ctx, "builtin "+call)
		require.NoError(t, err)
		bundle, values, err := result.ToAPIResult()
		require.NoError(t, err)
		require.Nil(t, bundle)
		require.Len(t, values, 1, "a successful authority builtin must not silently lose its receipt")
		encoded, err := protojson.Marshal(values[0])
		require.NoError(t, err)
		var decoded structpb.Value
		require.NoError(t, protojson.Unmarshal(encoded, &decoded))
		nodes := decoded.GetStructValue().GetFields()
		require.Len(t, nodes, 1)
		for key, value := range nodes {
			node := value.GetStructValue().GetFields()
			require.NotEmpty(t, key)
			require.Equal(t, key, node["id"].GetStringValue())
			require.False(t, seenEnvelopes[key], "ephemeral replies must have independent identities")
			seenEnvelopes[key] = true
			require.Equal(t, "v1:repositoryauthority:result", node["concept"].GetStringValue())
			createdAt, err := time.Parse(time.RFC3339Nano, node["createdAt"].GetStringValue())
			require.NoError(t, err)
			require.False(t, createdAt.Before(before))
			require.False(t, createdAt.After(time.Now().UTC()))
			return node["payload"].GetStructValue().AsMap()
		}
		t.Fatal("missing reply node")
		return nil
	}

	access := read(engines[0], actor(f.maintainer), "repositoryChangeAccess", map[string]any{"sourceId": "maintainer-source"})
	require.Equal(t, "maintainer-source", access["sourceId"])
	require.Equal(t, "core/engine", access["repository"])
	require.Equal(t, float64(123), access["repositoryId"])
	require.Equal(t, true, access["canWrite"])
	require.Equal(t, true, access["canRequest"])
	require.Equal(t, "own", access["route"])
	require.Equal(t, []any{OperationPropose}, access["operations"])
	upstream := read(engines[1], actor(f.requester), "repositoryChangeAccess", map[string]any{"sourceId": "request-source"})
	require.Equal(t, false, upstream["canWrite"])
	require.Equal(t, "maintainer", upstream["route"])
	require.Zero(t, f.opens, "reading authority must not create work")

	empty := read(engines[0], actor(f.requester), "repositoryChangeRequests", map[string]any{"sourceId": "request-source"})
	require.Empty(t, empty["requests"])
	request := f.request("request-source")
	args := map[string]any{"sourceId": request.SourceID, "requestId": request.RequestID, "statement": request.Statement, "operation": request.Operation, "paths": request.Paths}
	receipt := read(engines[0], actor(f.requester), "repositoryChangeRequest", args)
	require.Equal(t, "pending", receipt["state"])
	require.NotEmpty(t, receipt["id"])
	require.Equal(t, request.Statement, receipt["statement"])
	for _, key := range []string{"goalId", "runId", "approvalId"} {
		require.NotContains(t, receipt, key, "requesters must not receive the maintainer's private journal")
	}
	again := read(engines[1], actor(f.requester), "repositoryChangeStatus", map[string]any{"requestId": receipt["id"]})
	require.Equal(t, receipt, again, "the second replica must return the same durable receipt")
	require.Equal(t, 1, f.opens)

	_, err = engines[1].Execute(actor("unrelated-owner"), `builtin repositoryChangeStatus(requestId: `+parser.QuoteString(receipt["id"].(string))+`)`)
	require.Error(t, err, "an unavailable private request must remain a refusal")
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: f.requester, Role: auth.RoleReader})
	call, err := parser.RenderCall("repositoryChangeRequest", args)
	require.NoError(t, err)
	_, err = engines[0].Execute(reader, "builtin "+call)
	require.Error(t, err, "a valid envelope must not relax the request capability")
	require.Equal(t, 1, f.opens)
}

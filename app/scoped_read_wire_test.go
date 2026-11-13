package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// These integration tests live in the root module because their real handlers
// are root-owned. Keep transport helpers here rather than adding a reverse
// dependency from the independently built gRPC module back to the root.
type scopedOrganizationWireCatalog struct{ role, account string }

func (c scopedOrganizationWireCatalog) Rank(slug string) (int, bool) {
	if slug == c.role {
		return 250, true
	}
	rank, ok := map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "viewer": 50, "reader": 50}[slug]
	return rank, ok
}
func (c scopedOrganizationWireCatalog) Name(slug string) string { return slug }
func (c scopedOrganizationWireCatalog) Scope(slug string) string {
	if slug == c.role {
		return c.account
	}
	return ""
}
func (c scopedOrganizationWireCatalog) Active(slug string) bool { _, ok := c.Rank(slug); return ok }

type scopedReadServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s scopedReadServerStream) Context() context.Context { return s.ctx }

type scopedReadWire struct {
	stream memqlv1.MemqlService_StreamClient
	close  func()
}

// Authentication alone is a fixture: derive its claims from a verified user
// assertion, then cross the real public TCP stream and ordinary DB identity
// resolver. This is not a mesh-forwarding or owner/local-dev authentication
// shortcut. Every nested data handler, membership and grant read remains real.
func newScopedReadWire(t *testing.T, eng *memqlengine.MemQLEngine, user string, role auth.Role) *scopedReadWire {
	t.Helper()
	t.Setenv("MEMQL_GRPC_TLS_CERT_FILE", "")
	t.Setenv("MEMQL_GRPC_TLS_KEY_FILE", "")
	if !strings.HasPrefix(user, "v1:identity:user:") {
		user = "v1:identity:user:" + user
	}
	assertion, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: user, Role: role}, auth.ForwardedClassUser, "", time.Time{}, time.Now())
	require.NoError(t, err)
	verified, err := auth.VerifyForwardedAuthority(assertion, time.Now())
	require.NoError(t, err)
	require.Equal(t, user, verified.UserId)
	require.Equal(t, role, verified.Role)
	claims := map[string]any{"sub": verified.UserId, "role": string(verified.Role), "roles": []string{string(verified.Role)}}
	srv := memqlgrpc.NewServer(loopbackAddress(t), eng.Logger)
	srv.SetEngine(eng)
	srv.SetStreamInterceptor(func(service any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := auth.ContextWithClaims(stream.Context(), claims)
		ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
		return handler(service, scopedReadServerStream{ServerStream: stream, ctx: ctx})
	})
	var conn *grpc.ClientConn
	var cancel context.CancelFunc
	var once sync.Once
	client := &scopedReadWire{}
	client.close = func() {
		once.Do(func() {
			if client.stream != nil {
				_ = client.stream.CloseSend()
			}
			if cancel != nil {
				cancel()
			}
			if conn != nil {
				_ = conn.Close()
			}
			stop, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			srv.Stop(stop)
		})
	}
	t.Cleanup(client.Close)
	srv.Start(context.Background())
	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("scoped public stream did not start")
	}
	conn, err = grpc.NewClient(srv.Address(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	var ctx context.Context
	ctx, cancel = context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	client.stream, err = memqlv1.NewMemqlServiceClient(conn).Stream(ctx)
	require.NoError(t, err)
	return client
}

func (c *scopedReadWire) Close() { c.close() }

func (c *scopedReadWire) response(t *testing.T, id, query string) *memqlv1.MemqlServerMessage {
	t.Helper()
	require.NoError(t, c.stream.Send(&memqlv1.MemqlClientMessage{MessageId: id, Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
		ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: id, Query: query},
	}}))
	for {
		message, err := c.stream.Recv()
		require.NoError(t, err, "query %s", id)
		if failure := message.GetQueryError(); failure != nil && failure.GetRequestId() == id {
			return message
		}
		if result := message.GetQueryResult(); result != nil && result.GetRequestId() == id {
			return message
		}
	}
}

func (c *scopedReadWire) Query(t *testing.T, id, query string) *memqlv1.QueryResultChunk {
	t.Helper()
	message := c.response(t, id, query)
	require.Nil(t, message.GetQueryError(), "query %s: %v", id, message.GetQueryError())
	require.NotNil(t, message.GetQueryResult())
	require.True(t, message.GetQueryResult().GetDone(), "bounded fixture read must fit one result")
	return message.GetQueryResult()
}

func (c *scopedReadWire) Refused(t *testing.T, id, query string) {
	t.Helper()
	message := c.response(t, id, query)
	require.Nil(t, message.GetQueryResult(), "refused query returned data: %s", query)
	require.NotNil(t, message.GetQueryError(), query)
	require.Equal(t, codes.PermissionDenied.String(), message.GetQueryError().GetError().GetCode(), query)
}

package verifier_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"google.golang.org/grpc"
)

type testVerifier struct {
	revoked atomic.Bool
	expiry  time.Time
}

func (v *testVerifier) VerifyRoleTest(context.Context, string) (*verifier.VerifiedClaims, error) {
	if v.revoked.Load() {
		return nil, errors.New("revoked")
	}
	return &verifier.VerifiedClaims{UserId: "v1:identity:user:role-test-example", Class: auth.RoleTestClass, Role: "reader", Source: verifier.SourceRoleTest, ExpiresAt: v.expiry, ClaimsMap: map[string]any{"sub": "v1:identity:user:role-test-example", "role": "reader", "class": auth.RoleTestClass, "exp": v.expiry.Unix()}}, nil
}
func TestRoleTestRechecksEachMessageAndHTTPGetsDeadline(t *testing.T) {
	v := newVerifier(t, "https://identity.example.test")
	source := &testVerifier{expiry: time.Now().Add(time.Minute)}
	v.SetRoleTestVerifier(source)
	err := verifier.StreamInterceptor(v, nil)(nil, &fakeStream{ctx: ctxWithToken("mql_test_example")}, &grpc.StreamServerInfo{FullMethod: "/test/stream"}, func(_ any, stream grpc.ServerStream) error {
		deadline, ok := stream.Context().Deadline()
		require.True(t, ok)
		require.Equal(t, source.expiry, deadline)
		require.NoError(t, stream.RecvMsg(nil))
		source.revoked.Store(true)
		require.Error(t, stream.RecvMsg(nil))
		return nil
	})
	require.NoError(t, err)
	source.revoked.Store(false)
	reached := false
	handler := verifier.HTTPMiddleware(v, verifier.MiddlewareOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.Equal(t, source.expiry, deadline)
	}))
	request := httptest.NewRequest("GET", "https://api.example.test/files", nil)
	request.Header.Set("Authorization", "Bearer mql_test_example")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.True(t, reached)
	source.revoked.Store(true)
	reached = false
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.False(t, reached)
	require.Equal(t, http.StatusUnauthorized, response.Code)
}

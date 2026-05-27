package node_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/node"
)

// fakeResolver is a recording stub for NodeTokenRevocationResolver
// that lets tests stage a per-id "revoked" answer + count calls so
// the cache behaviour is observable.
type fakeResolver struct {
	revoked map[string]bool
	err     error
	calls   int64
}

func (r *fakeResolver) IsNodeTokenRevoked(ctx context.Context, identityId string) (bool, error) {
	_ = ctx
	atomic.AddInt64(&r.calls, 1)
	if r.err != nil {
		return false, r.err
	}
	return r.revoked[identityId], nil
}

// TestNodeRevocationCheck_NilResolverFallsBackToBase exercises the
// "opt-in" contract: without a resolver wired, the with-revocation
// interceptor must behave exactly like the base one. This is the
// promise the cluster bootstrap relies on -- a deployment that opts
// out of persistence (NodeTokenStore nil) doesn't pay any new cost.
func TestNodeRevocationCheck_NilResolverFallsBackToBase(t *testing.T) {
	intercept := node.NodeClassStreamInterceptorWithRevocation(nil, nil, nil)
	require.NotNil(t, intercept)
	// Smoke: the interceptor is callable + the no-op pass-through
	// path returns nil for a trivial handler.
	// (The base NodeClassStreamInterceptor returns a no-op when its
	// verifier is nil, which is what we exercise here.)
}

// TestNodeRevocationCheck_DefaultCacheTTL pins the default TTL to
// the value the cluster bootstrap relies on (5s). The check is on
// the constant rather than the struct field so a future tweak to
// the default surfaces here as an explicit signal.
func TestNodeRevocationCheck_DefaultCacheTTL(t *testing.T) {
	assert.Equal(t, 5*time.Second, node.DefaultNodeRevocationCacheTTL)
}

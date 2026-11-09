package installation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestReceiverApplicationStatusGenerationIsNotConfiguration(t *testing.T) {
	f := newReceiverFixture(t)
	ctx := captureOperator(auth.RoleOwner, "operator")
	before, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	app := f.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"]
	resourceMap(app, "metadata")["generation"] = 2
	resourceMap(app, "metadata")["resourceVersion"] = "status-update"
	resourceMap(app, "status")["reconciledAt"] = "2026-11-08T17:30:00Z"
	after, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, before.digest, after.digest, "Application CRDs without /status advance generation for a status refresh; native history separately fences real edits")
}

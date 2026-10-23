package packages

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/secret"
)

func TestQueueCredentialsKeepObservationSeparateFromUserWrites(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"push":true}}`)
	i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	ctx := context.Background()
	user, installation, err := i.RepositoryQueueUserToken(ctx, grantCredentialId, grantOwner, "acme", "widget")
	require.NoError(t, err)
	require.Equal(t, int64(42), installation)
	require.Equal(t, grantUserToken, user)
	require.Zero(t, hub.hits("/app/installations/42/access_tokens"), "queue writes must not borrow installation authority")
	read, _, err := i.RepositoryQueueObservationToken(ctx, grantCredentialId, grantOwner, "acme", "widget")
	require.NoError(t, err)
	require.Equal(t, grantInstallToken, read)
	var scope struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	bodies := hub.bodies()
	require.NoError(t, json.Unmarshal([]byte(bodies[len(bodies)-1]), &scope))
	require.Equal(t, []string{"widget"}, scope.Repositories)
	require.Len(t, scope.Permissions, 7)
	for _, level := range scope.Permissions {
		require.Equal(t, "read", level)
	}
	hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"pull":true}}`)
	user, _, err = i.RepositoryQueueUserToken(ctx, grantCredentialId, grantOwner, "acme", "widget")
	require.Equal(t, "repository_write_not_authorized", RefusalCode(err))
	require.Empty(t, user)
	require.Equal(t, 1, hub.hits("/app/installations/42/access_tokens"))
}

func TestReviewCheckCredentialIsNarrowAndRevalidatesTheSourceOwner(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"push":true}}`)
	i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	ctx := context.Background()
	token, appID, err := i.RepositoryReviewCheckToken(ctx, grantCredentialId, grantOwner, "acme", "widget")
	require.NoError(t, err)
	require.Equal(t, grantInstallToken, token)
	require.Equal(t, int64(1234567), appID)
	var scope struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	bodies := hub.bodies()
	require.NoError(t, json.Unmarshal([]byte(bodies[len(bodies)-1]), &scope))
	require.Equal(t, []string{"widget"}, scope.Repositories)
	require.Equal(t, map[string]string{"checks": "write", "metadata": "read"}, scope.Permissions)
	hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"pull":true}}`)
	token, appID, err = i.RepositoryReviewCheckToken(ctx, grantCredentialId, grantOwner, "acme", "widget")
	require.Equal(t, "repository_write_not_authorized", RefusalCode(err))
	require.Empty(t, token)
	require.Zero(t, appID)
	require.Equal(t, 1, hub.hits("/app/installations/42/access_tokens"))
}

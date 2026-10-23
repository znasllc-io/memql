package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/workertoken"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// Exercise the actual mint handler and outbound message: checking only a
// response builder would miss a canonical ID introduced after persistence.
func TestCreateWorkerTokenWireIDsAndStoredAuthority(t *testing.T) {
	db := openWireTestDB(t)
	_, err := memqlengine.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	ownerShort := fmt.Sprintf("worker-wire-owner-%d", time.Now().UnixNano())
	ownerID := "v1:identity:user:" + ownerShort
	seedUserRow(context.Background(), t, db, ownerID)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", ownerID).Exec(context.Background())
	})

	for _, tc := range []struct {
		name   string
		expiry string
	}{
		{name: "nonexpiring"},
		{name: "explicit-deadline", expiry: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, stream := accountTestSession(t, eng, ownerID)
			request := &memqlv1.MemqlClientMessage{
				MessageId: tc.name,
				Payload: &memqlv1.MemqlClientMessage_CreateWorkerToken{
					CreateWorkerToken: &memqlv1.CreateWorkerTokenMsg{
						RequestId: tc.name, Name: "wire fixture", ExpiresAt: tc.expiry,
					},
				},
			}
			require.NoError(t, session.handleCreateWorkerToken(request, request.GetCreateWorkerToken()))
			messages := stream.snapshot()
			require.Len(t, messages, 1)
			response := messages[0]
			minted := response.GetCreateWorkerTokenResult()
			require.NotNil(t, minted)
			require.True(t, minted.GetSuccess(), "mint failed: %s", minted.GetErrorCode())
			require.NotEmpty(t, minted.GetPlainToken())
			require.NotEmpty(t, minted.GetIdentityId())
			identityID := workertoken.CanonicalId(minted.GetIdentityId())
			t.Cleanup(func() {
				_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", identityID).Exec(context.Background())
			})

			// The SDK receives bare IDs while persistence retains canonical
			// ownership and the exact requested expiry, including no expiry.
			scanNoCanonicalIds(t, tc.name, response)
			require.False(t, strings.Contains(minted.GetIdentityId(), ":"), "identity ID must be bare")
			require.Equal(t, ownerShort, minted.GetOwnerUserId())
			require.Equal(t, tc.name, response.GetCorrelateTo())
			stored := accountPayload(t, db, identityID)
			require.Equal(t, ownerID, stored["userId"])
			require.Equal(t, "worker_token", stored["identityType"])
			require.Equal(t, true, stored["active"])
			require.Equal(t, false, stored["usableByAgents"])
			credentials, ok := stored["credentials"].(map[string]any)
			require.True(t, ok, "stored credential metadata is missing")
			require.Equal(t, ownerID, credentials["registeredBy"])
			require.Equal(t, tc.expiry, credentials["expiresAt"])
			require.Equal(t, "wire fixture", credentials["name"])
			require.True(t, credentials["keyHash"] == workertoken.Hash(minted.GetPlainToken()), "stored digest must match the returned token")
		})
	}
}

package installation

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// Exercise the real PostgreSQL encoding and a different DB connection. Keep
// TestMain and journalDB's ordinary database requirement intact.
func TestPreparationReservedIntentSurvivesReplicaJSONB(t *testing.T) {
	db, peerDB := journalDB(t)
	first, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	// A float64 round trip would lose this generation; hashing must retain the
	// exact native integer alongside JSONB's reordering of the full spec.
	scope.Intent.BeforeGeneration = (1 << 53) + 17
	var formatted bytes.Buffer
	require.NoError(t, json.Indent(&formatted, scope.Intent.BeforeSpec, "", "  "))
	scope.Intent.BeforeSpec = append(json.RawMessage(nil), formatted.Bytes()...)
	wantIntent, err := scope.Intent.Digest()
	require.NoError(t, err)
	wantBody, wantScope, err := scope.canonical()
	require.NoError(t, err)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	record, err := first.reserve(ctx, scope)
	require.NoError(t, err)
	recovered, err := peer.getByRequest(ctx, scope.InstallationID, scope.RequestID)
	require.NoError(t, err)
	require.Equal(t, record, recovered)
	require.Equal(t, wantScope, recovered.ID)
	require.Equal(t, scope.Intent.BeforeGeneration, recovered.Scope.Intent.BeforeGeneration)
	gotIntent, err := recovered.Scope.Intent.Digest()
	require.NoError(t, err)
	require.Equal(t, wantIntent, gotIntent)
	gotBody, gotScope, err := recovered.Scope.canonical()
	require.NoError(t, err)
	require.Equal(t, wantBody, gotBody)
	require.Equal(t, wantScope, gotScope)
	require.NoError(t, promotionIntentMatches(recovered.Scope, preparedPlan{Intent: scope.Intent}))
	// Reusing the same request with a different native operation is forbidden
	// even if source, target, operator and publication are otherwise identical.
	changed := scope
	changed.Intent.RequestID = "replacement-operation"
	_, err = peer.reserve(ctx, changed)
	require.ErrorContains(t, err, "reused with changed inputs")
	require.Error(t, promotionIntentMatches(recovered.Scope, preparedPlan{Intent: changed.Intent}))
	stillOriginal, err := first.get(ctx, scope.InstallationID, record.ID)
	require.NoError(t, err)
	require.Equal(t, recovered, stillOriginal)
}

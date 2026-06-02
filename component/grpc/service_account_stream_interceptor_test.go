package memql

import (
	"testing"

	"github.com/stretchr/testify/assert"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestIsServiceAccountPayload_Allowlist pins the surface-pinning contract for
// #691 (deployment-v2 Phase 3): a class="service_account" token may run the
// read/query surface + control frames + a synthetic agent turn, but NEVER a
// credential/identity/delegation/worker-token/invite mutation. A leaked
// synthetic credential therefore can't escalate.
func TestIsServiceAccountPayload_Allowlist(t *testing.T) {
	allowed := []any{
		&memqlv1.MemqlClientMessage_ClientHello{},
		&memqlv1.MemqlClientMessage_Ack{},
		&memqlv1.MemqlClientMessage_Unsubscribe{},
		&memqlv1.MemqlClientMessage_CancelRequest{},
		&memqlv1.MemqlClientMessage_ExecuteQuery{},
		&memqlv1.MemqlClientMessage_Subscribe{},
		&memqlv1.MemqlClientMessage_ConceptsList{},
		&memqlv1.MemqlClientMessage_ConceptsSubscribe{},
		&memqlv1.MemqlClientMessage_MyAccess{},
		&memqlv1.MemqlClientMessage_EvaluatePolicy{},
		&memqlv1.MemqlClientMessage_AgentGenerateTurn{},
	}
	for _, p := range allowed {
		assert.Truef(t, isServiceAccountPayload(p), "%T must be admitted", p)
	}
	// Credential / admin mutations must be rejected -- this is the whole point.
	rejected := []any{
		&memqlv1.MemqlClientMessage_IdentityCreate{},
		&memqlv1.MemqlClientMessage_IdentityUpdate{},
		&memqlv1.MemqlClientMessage_CreateWorkerToken{},
		&memqlv1.MemqlClientMessage_RevokeWorkerToken{},
		&memqlv1.MemqlClientMessage_DelegationCreate{},
		&memqlv1.MemqlClientMessage_SendGuestInvite{},
		&memqlv1.MemqlClientMessage_RotateAuth{},
		&memqlv1.MemqlClientMessage_RevokeAllSessions{},
		// Off-surface (other credential families' message types):
		&memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{},
	}
	for _, p := range rejected {
		assert.Falsef(t, isServiceAccountPayload(p), "%T must be rejected", p)
	}
}

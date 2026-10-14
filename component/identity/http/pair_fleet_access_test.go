package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/workerpairing"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type pairFleetEngine struct {
	t      *testing.T
	role   string
	active bool
	writes []string
}

func (e *pairFleetEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	nodes := &badgeStubEngine{}
	switch {
	case strings.Contains(q, "userByIdSystem"):
		require.True(e.t, memqlengine.FreshReadFromContext(ctx))
		return nodes.node(map[string]any{"id": "v1:identity:user:kiosk-7", "role": e.role, "active": e.active})
	case strings.Contains(q, "workerPairingCodeByHash"):
		return nodes.node(map[string]any{"id": "v1:identity:workerPairingCode:pair-test", "ownerUserId": "v1:identity:user:kiosk-7", "clusterURL": "https://api.test", "expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)})
	case strings.HasPrefix(q, "mutation "):
		e.writes = append(e.writes, q)
		return nil, nil
	default:
		e.t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
}
func TestPairingChecksCurrentFleetAccessAtBothEnds(t *testing.T) {
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
	for _, role := range []string{"owner", "developer", "admin", "writer", "reader"} {
		t.Run(role, func(t *testing.T) {
			s := newBadgeGrantTestServer(t, &badgeStubEngine{})
			e := &pairFleetEngine{t: t, role: role, active: true}
			s.Store.Engine = e
			// Even an old owner JWT cannot overrule a fresh lower directory role.
			bearer := mintTerminalBearer(t, s, "owner")
			req := httptest.NewRequest(http.MethodPost, "https://identity.test/pair/codes", strings.NewReader(`{"clusterUrl":"https://api.test"}`))
			req.Header.Set("Authorization", "Bearer "+bearer)
			response := httptest.NewRecorder()
			s.handlePairCreate(response, req)
			allowed := role == "owner" || role == "developer"
			if !allowed {
				require.Equal(t, http.StatusForbidden, response.Code)
				require.Empty(t, e.writes)
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, e.writes, 1)
			var code PairCreateResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &code))
			require.NotEmpty(t, code.PlainCode)
			// The redeem request has no browser session and may reach another node.
			// A downgrade between issue and redemption must prevent a token write.
			e.role = "reader"
			e.writes = nil
			redeem := httptest.NewRequest(http.MethodPost, "https://identity.test/pair/redeem", strings.NewReader(`{}`))
			redeem.Header.Set("Authorization", "Pair "+code.PlainCode)
			denied := httptest.NewRecorder()
			s.handlePairRedeem(denied, redeem)
			require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
			require.Empty(t, e.writes)
			e.role = role
			success := httptest.NewRecorder()
			s.handlePairRedeem(success, redeem)
			require.Equal(t, http.StatusOK, success.Code, success.Body.String())
			require.Len(t, e.writes, 2)
			var result PairRedeemResponse
			require.NoError(t, json.Unmarshal(success.Body.Bytes(), &result))
			require.True(t, result.Success)
			require.NotEmpty(t, result.PlainToken)
			require.Equal(t, "v1:identity:user:kiosk-7", result.OwnerUserId)
		})
	}
}
func TestPairingCannotEnrollAsSomeoneElse(t *testing.T) {
	s := newBadgeGrantTestServer(t, &badgeStubEngine{})
	e := &pairFleetEngine{t: t, role: "owner", active: true}
	s.Store.Engine = e
	req := httptest.NewRequest(http.MethodPost, "https://identity.test/pair/codes", strings.NewReader(`{"clusterUrl":"https://api.test","ownerUserId":"another-person"}`))
	req.Header.Set("Authorization", "Bearer "+mintTerminalBearer(t, s, "owner"))
	response := httptest.NewRecorder()
	s.handlePairCreate(response, req)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Empty(t, e.writes)
	// A previously issued code must also stop when the person is disabled.
	plain, _, err := workerpairing.Mint()
	require.NoError(t, err)
	e.active = false
	req = httptest.NewRequest(http.MethodPost, "https://identity.test/pair/redeem", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Pair "+plain)
	response = httptest.NewRecorder()
	s.handlePairRedeem(response, req)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Empty(t, e.writes)
}

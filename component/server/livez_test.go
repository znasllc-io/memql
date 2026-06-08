package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/core/common"
)

// stoppedDep is a common.Dependency whose IsRunning() always reports false --
// the steady-state shape of a dependency that has not (re)started yet or has
// stopped. /healthz folds this into a 503; /livez must ignore it.
type stoppedDep struct{}

func (stoppedDep) Start(context.Context)               {}
func (stoppedDep) Stop(context.Context)                {}
func (stoppedDep) IsRunning() bool                     { return false }
func (stoppedDep) Order() int                          { return 0 }
func (stoppedDep) ComponentName() common.ComponentName { return "stopped" }
func (stoppedDep) Ready() <-chan struct{}              { return nil }

func doLivez(t *testing.T) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	LivezHandler().ServeHTTP(rec, req)

	var body struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	return rec.Code, body.Status
}

// TestLivez_AlwaysAliveWhenDraining is the core #1117 contract: while the node
// is draining (a state that flips /healthz -- and thus readiness -- to 503),
// /livez still reports 200. Liveness must answer only "is this process
// wedged?", so a draining-but-alive pod is NOT liveness-killed; readiness on
// /healthz is what de-routes it.
func TestLivez_AlwaysAliveWhenDraining(t *testing.T) {
	t.Cleanup(func() { SetDraining(false) })

	SetDraining(true)
	// /healthz would 503 here...
	_, is503 := buildHealthResponse().(GetHealthz503JSONResponse)
	require.True(t, is503, "precondition: /healthz is 503 while draining")

	// ...but /livez stays 200.
	code, status := doLivez(t)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "alive", status)
}

// TestLivez_AlwaysAliveWhenDependencyDown pins the other half: a dependency
// reporting IsRunning()==false (a restarting mesh component, a DB blip) flips
// /healthz to 503, but /livez stays 200 so the pod is left to recover in place
// instead of being liveness-killed -- the exact restart-storm trigger #1117
// removes.
func TestLivez_AlwaysAliveWhenDependencyDown(t *testing.T) {
	SetHealthDependencies([]common.Dependency{stoppedDep{}})
	t.Cleanup(func() { SetHealthDependencies(nil); SetDraining(false) })
	SetDraining(false)

	// /healthz would 503 because a dependency is not running...
	_, is503 := buildHealthResponse().(GetHealthz503JSONResponse)
	require.True(t, is503, "precondition: /healthz is 503 when a dep is down")

	// ...but /livez stays 200.
	code, status := doLivez(t)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "alive", status)
}

// TestLivez_RejectsNonGET keeps the method contract aligned with the other
// probe handlers.
func TestLivez_RejectsNonGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/livez", nil)
	rec := httptest.NewRecorder()
	LivezHandler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doReadyz drives the handler and returns the status code + decoded body.
func doReadyz(t *testing.T) (int, readyzResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	ReadyzHandler().ServeHTTP(rec, req)

	var body readyzResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	return rec.Code, body
}

// TestReadyz_AllChecksPass is the happy path: every registered invariant holds,
// so /readyz reports 200 "ready".
func TestReadyz_AllChecksPass(t *testing.T) {
	ResetReadinessChecks()
	t.Cleanup(func() { ResetReadinessChecks(); SetDraining(false) })
	SetDraining(false)

	RegisterReadinessCheck("schema", func(context.Context) error { return nil })

	code, body := doReadyz(t)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ready", body.Status)
	require.Len(t, body.Checks, 1)
	assert.True(t, body.Checks[0].Ready)
}

// TestReadyz_FailingCheckReturns503 is the #657 regression: a violated
// invariant (e.g. a missing critical table) makes /readyz report 503 with the
// failure surfaced, so the deep-smoke gate fails the deploy.
func TestReadyz_FailingCheckReturns503(t *testing.T) {
	ResetReadinessChecks()
	t.Cleanup(func() { ResetReadinessChecks(); SetDraining(false) })
	SetDraining(false)

	RegisterReadinessCheck("schema", func(context.Context) error {
		return errors.New("required table \"automation_execution_claims\" is missing (to_regclass IS NULL)")
	})

	code, body := doReadyz(t)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "not_ready", body.Status)
	require.Len(t, body.Checks, 1)
	assert.False(t, body.Checks[0].Ready)
	assert.Contains(t, body.Checks[0].Error, "automation_execution_claims")
}

// TestReadyz_DrainingReturns503 mirrors the /healthz contract: a pod in its
// shutdown window is not ready regardless of schema state.
func TestReadyz_DrainingReturns503(t *testing.T) {
	ResetReadinessChecks()
	t.Cleanup(func() { ResetReadinessChecks(); SetDraining(false) })

	RegisterReadinessCheck("schema", func(context.Context) error { return nil })
	SetDraining(true)

	code, body := doReadyz(t)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "draining", body.Status)
}

// TestReadyz_RegisterReplacesByName ensures a re-wire during bootstrap/tests
// doesn't duplicate a check under the same name.
func TestReadyz_RegisterReplacesByName(t *testing.T) {
	ResetReadinessChecks()
	t.Cleanup(func() { ResetReadinessChecks(); SetDraining(false) })
	SetDraining(false)

	RegisterReadinessCheck("schema", func(context.Context) error { return errors.New("first") })
	RegisterReadinessCheck("schema", func(context.Context) error { return nil })

	code, body := doReadyz(t)
	assert.Equal(t, http.StatusOK, code)
	require.Len(t, body.Checks, 1, "same-named check should replace, not append")
}

// TestReadyz_RejectsNonGET pins the method guard.
func TestReadyz_RejectsNonGET(t *testing.T) {
	ResetReadinessChecks()
	t.Cleanup(func() { ResetReadinessChecks() })

	req := httptest.NewRequest(http.MethodPost, "/readyz", nil)
	rec := httptest.NewRecorder()
	ReadyzHandler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

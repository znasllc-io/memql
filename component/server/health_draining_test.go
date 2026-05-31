package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDraining_FlipsHealthTo503 pins the #552 contract: when the node is
// draining (shutdown signal received), /healthz reports 503 with status
// "draining" so a readiness probe / LB stops routing new traffic. When not
// draining and all deps are up, it is 200 "ok".
func TestDraining_FlipsHealthTo503(t *testing.T) {
	// No dependencies registered -> the dependency loop leaves allRunning
	// true, isolating the draining flag as the only failing condition.
	SetHealthDependencies(nil)
	t.Cleanup(func() { SetDraining(false) })

	SetDraining(false)
	assert.False(t, IsDraining())
	ok, isOK := buildHealthResponse().(GetHealthz200JSONResponse)
	require.True(t, isOK, "expected 200 when not draining and deps healthy")
	assert.Equal(t, "ok", ok.Status)

	SetDraining(true)
	assert.True(t, IsDraining())
	degraded, is503 := buildHealthResponse().(GetHealthz503JSONResponse)
	require.True(t, is503, "expected 503 while draining")
	assert.Equal(t, "draining", degraded.Status)
}

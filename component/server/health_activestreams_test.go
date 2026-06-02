package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestActiveStreams_CountsOpenSessions pins the #616 (blue/green BFF) drain
// primitive: StreamOpened / StreamClosed move the active-stream count, and
// /healthz surfaces it (always, including 0) so the cutover script can poll a
// draining OLD-color pod until it reaches zero existing connections.
func TestActiveStreams_CountsOpenSessions(t *testing.T) {
	SetHealthDependencies(nil)
	t.Cleanup(func() {
		// Wind the counter back to whatever it was so we don't leak across
		// tests in the package (the var is package-global).
		for ActiveStreams() > 0 {
			StreamClosed()
		}
		SetDraining(false)
	})

	require.Equal(t, int64(0), ActiveStreams())

	assert.Equal(t, int64(1), StreamOpened())
	assert.Equal(t, int64(2), StreamOpened())
	assert.Equal(t, int64(2), ActiveStreams())

	// /healthz reports the live count.
	ok, isOK := buildHealthResponse().(GetHealthz200JSONResponse)
	require.True(t, isOK)
	assert.Equal(t, int64(2), ok.ActiveStreams)

	assert.Equal(t, int64(1), StreamClosed())
	assert.Equal(t, int64(0), StreamClosed())
	assert.Equal(t, int64(0), ActiveStreams())

	// Even at zero, the field is present (serialized non-omitempty) so a
	// drain watcher sees an explicit 0 rather than a missing key.
	ok2 := buildHealthResponse().(GetHealthz200JSONResponse)
	assert.Equal(t, int64(0), ok2.ActiveStreams)
}

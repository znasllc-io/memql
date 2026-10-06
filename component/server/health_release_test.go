package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/core/buildinfo"
)

// healthzWire renders a /healthz answer the way the handler writes it and
// decodes the body into a bare map, so a test reads the WIRE -- names and
// omissions -- and not the Go struct that happens to produce it.
func healthzWire(t *testing.T, resp GetHealthzResponseObject) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	require.NoError(t, resp.VisitGetHealthzResponse(rec))
	var body map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	return rec.Code, body
}

// TestHealthzReportsTheReleaseItRuns pins what verify-rollout (epic
// memql#5480) reads from outside, through the front door: the release this
// binary was cut from and its short revision, on every /healthz answer. The
// 503 of a draining or degraded node carries them as the 200 does, because a
// rollout is judged exactly while nodes are coming and going.
func TestHealthzReportsTheReleaseItRuns(t *testing.T) {
	SetHealthDependencies(nil)
	t.Cleanup(func() { SetDraining(false) })

	for _, tc := range []struct {
		name     string
		draining bool
		status   int
	}{
		{"healthy", false, http.StatusOK},
		{"draining", true, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetDraining(tc.draining)
			code, body := healthzWire(t, buildHealthResponse())
			require.Equal(t, tc.status, code)

			assert.Equal(t, buildinfo.Version(), body["version"])
			if commit := buildinfo.ShortCommit(); commit != "" {
				assert.Equal(t, commit, body["commit"])
			} else {
				assert.NotContains(t, body, "commit", "an unknown commit is omitted, not sent empty")
			}
		})
	}
}

// A test binary carries no revision -- the go command stamps one for `go build`
// of a main package, not for a test binary -- so the test above can only ever
// see the commit as unknown, and would pass against a handler that never set
// it. This is the positive control: given a release and a commit, BOTH answers
// carry both, under the names verify-rollout reads, and an unknown commit is
// left out rather than sent empty.
func TestHealthzCarriesTheReleaseAndCommitItIsGiven(t *testing.T) {
	SetHealthDependencies(nil)
	t.Cleanup(func() { SetDraining(false) })

	for _, draining := range []bool{false, true} {
		SetDraining(draining)

		code, body := healthzWire(t, buildHealthResponseFor("v0.19.1", "0123456789ab"))
		want := http.StatusOK
		if draining {
			want = http.StatusServiceUnavailable
		}
		require.Equal(t, want, code)
		assert.Equal(t, "v0.19.1", body["version"], "draining=%v", draining)
		assert.Equal(t, "0123456789ab", body["commit"], "draining=%v", draining)

		_, body = healthzWire(t, buildHealthResponseFor("v0.19.1", ""))
		assert.Equal(t, "v0.19.1", body["version"], "draining=%v", draining)
		assert.NotContains(t, body, "commit", "draining=%v: an unknown commit is omitted", draining)
	}
}

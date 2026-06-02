package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

// ReadinessCheck is a named server-side invariant asserted by GET /readyz.
// Checks are registered at bootstrap (see app wiring) and run on every probe.
// A check returns a non-nil error when the invariant is violated; /readyz then
// reports 503 so a deploy gate (scripts/deploy/staging-smoke-test.sh, deep
// profile) can prove critical state -- e.g. required schema presence -- WITHOUT
// database credentials (the staging DB is firewalled to in-cluster egress).
// This closes the gap that let #624 ship a broken schema behind a green deploy
// and is the runtime counterpart to the #671 migrate gate (#657).
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

var (
	readinessLock   sync.RWMutex
	readinessChecks []ReadinessCheck
)

// RegisterReadinessCheck adds (or replaces, by name) a readiness invariant.
// Replace-by-name keeps a re-wire during bootstrap or tests from duplicating a
// check. A nil check or empty name is ignored.
func RegisterReadinessCheck(name string, check func(ctx context.Context) error) {
	if name == "" || check == nil {
		return
	}

	readinessLock.Lock()
	defer readinessLock.Unlock()

	for i := range readinessChecks {
		if readinessChecks[i].Name == name {
			readinessChecks[i].Check = check
			return
		}
	}
	readinessChecks = append(readinessChecks, ReadinessCheck{Name: name, Check: check})
}

// ResetReadinessChecks clears all registered checks. Test helper.
func ResetReadinessChecks() {
	readinessLock.Lock()
	defer readinessLock.Unlock()
	readinessChecks = nil
}

func snapshotReadinessChecks() []ReadinessCheck {
	readinessLock.RLock()
	defer readinessLock.RUnlock()

	if len(readinessChecks) == 0 {
		return nil
	}

	out := make([]ReadinessCheck, len(readinessChecks))
	copy(out, readinessChecks)
	return out
}

type readyCheckResult struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

type readyzResponse struct {
	Status string             `json:"status"`
	Checks []readyCheckResult `json:"checks,omitempty"`
}

// buildReadyzResponse runs every registered check and reports overall readiness
// plus the per-check breakdown. Returns the HTTP status to emit alongside it.
func buildReadyzResponse(ctx context.Context) (int, readyzResponse) {
	// Draining wins: a pod in its shutdown window is not ready (mirrors the
	// /healthz contract so the two endpoints don't disagree during a rollout).
	if IsDraining() {
		return http.StatusServiceUnavailable, readyzResponse{Status: "draining"}
	}

	checks := snapshotReadinessChecks()
	results := make([]readyCheckResult, 0, len(checks))
	allReady := true

	for _, c := range checks {
		if c.Check == nil {
			continue
		}

		err := c.Check(ctx)
		res := readyCheckResult{Name: c.Name, Ready: err == nil}
		if err != nil {
			res.Error = err.Error()
			allReady = false
		}
		results = append(results, res)
	}

	if !allReady {
		return http.StatusServiceUnavailable, readyzResponse{Status: "not_ready", Checks: results}
	}
	return http.StatusOK, readyzResponse{Status: "ready", Checks: results}
}

// ReadyzHandler serves GET /readyz: 200 when every registered readiness
// invariant holds, 503 otherwise (or while draining). It is public +
// unauthenticated (see PublicPaths) so an external deploy gate without DB
// credentials can probe critical schema presence.
func ReadyzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}

		status, body := buildReadyzResponse(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

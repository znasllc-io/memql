package server

import (
	"encoding/json"
	"net/http"
)

// LivezHandler serves GET /livez: a pure process-liveness probe.
//
// Unlike /healthz (dependency- and drain-aware, used for READINESS) this
// endpoint returns 200 as long as this process can serve an HTTP request --
// regardless of dependency health AND regardless of draining. That is the
// whole point: liveness must answer only "is this process wedged?", never
// "are my mesh peers / DB / parent currently reachable?".
//
// Why this exists (#1117 / epic #1115): the node deployments historically
// pointed BOTH the readiness and the liveness probe at /healthz. Because
// /healthz reports 503 whenever any dependency lifecycle is not-running (or
// while draining), a routine rollout's mesh churn would flip /healthz to 503
// on otherwise-healthy neighbors and k8s would LIVENESS-KILL them -- turning a
// one-restart rollout into a cluster-wide restart storm and a multi-minute
// chat/WebSocket outage. Readiness on /healthz is correct (de-route a degraded
// pod); liveness must be this dumb 200 so a transient dependency blip lets the
// pod recover in place instead of being killed.
//
// A draining pod deliberately still reports 200 here: it is alive and finishing
// in-flight work; /healthz (readiness) is what flips to 503 to de-route it.
// Public + unauthenticated (see PublicPaths) so the kubelet probe needs no JWT.
func LivezHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
		}{Status: "alive"})
	}
}

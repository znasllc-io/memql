package app

import (
	"log/slog"
	"os"

	"github.com/znasllc-io/memql/component/node"
)

// workerPeerSeedsFromEnv reads MEMQL_WORKER_PEERS and returns the dial seeds it
// names, warning about every entry that produced none.
//
// Every WorkerDialer construction goes through here. There are three of them
// (bff, cognition/planner, agent-with-remote-workbench) and each one used to
// call node.ParseWorkerPeers directly on the raw env var, which discarded
// anything it could not use without a word (memql#3450).
func (a *App) workerPeerSeedsFromEnv() []node.WorkerTarget {
	return workerPeerSeeds(os.Getenv("MEMQL_WORKER_PEERS"), a.Logger)
}

// workerPeerSeeds is the env-free half, so the warning behaviour is testable
// without touching the process environment.
func workerPeerSeeds(raw string, logger *slog.Logger) []node.WorkerTarget {
	seeds, issues := node.ParseWorkerPeers(raw)
	if logger != nil {
		for _, issue := range issues {
			// WARN, not Debug: this is a configuration the operator wrote and
			// the node is about to ignore. A silently-dropped entry looks
			// identical to a working one from the outside -- the whole defect
			// in memql#3450 was that the documented cluster-mode workbench
			// seed produced no peer and no complaint.
			logger.Warn("MEMQL_WORKER_PEERS: ignoring entry",
				"entry", issue.Entry,
				"reason", issue.Reason,
			)
		}
	}
	return seeds
}

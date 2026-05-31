package server

import (
	"sync"
	"sync/atomic"

	"github.com/znasllc-io/memql/core/common"
)

type dependencyHealthStatus struct {
	Component string `json:"component"`
	Running   bool   `json:"running"`
}

// nodeIdentityInfo holds the node identity fields returned in /healthz.
// Set once during cluster bootstrap via SetNodeIdentity.
type nodeIdentityInfo struct {
	NodeId      string
	NodeType    string
	Flavor      string
	GRPCAddress string
}

var (
	dependenciesLock sync.RWMutex
	healthDeps       []common.Dependency

	identityLock sync.RWMutex
	nodeIdentity *nodeIdentityInfo

	// draining flips true when the process has received a shutdown
	// signal and entered its drain window (graceful shutdown, #552).
	// While draining, /healthz reports 503 so a load balancer / k8s
	// readiness probe stops routing NEW traffic to this pod BEFORE the
	// servers tear down -- closing the SIGTERM-vs-endpoint-removal race
	// that otherwise drops in-flight connections on every rollout.
	draining atomic.Bool
)

// SetDraining marks the node as draining (or clears it). Called from the
// shutdown path (app.Run's BeginDrain hook) the moment a SIGTERM/SIGINT
// arrives, before the dependency Stop sweep. Idempotent + concurrency-safe.
func SetDraining(d bool) { draining.Store(d) }

// IsDraining reports whether the node is in its shutdown drain window.
func IsDraining() bool { return draining.Load() }

// SetNodeIdentity stores the node identity so /healthz can include it.
// Called from app/cluster.go after the node identity is created.
func SetNodeIdentity(id, nodeType, flavor, grpcAddr string) {
	identityLock.Lock()
	defer identityLock.Unlock()
	nodeIdentity = &nodeIdentityInfo{
		NodeId:      id,
		NodeType:    nodeType,
		Flavor:      flavor,
		GRPCAddress: grpcAddr,
	}
}

func getNodeIdentity() *nodeIdentityInfo {
	identityLock.RLock()
	defer identityLock.RUnlock()
	return nodeIdentity
}

func SetHealthDependencies(deps []common.Dependency) {
	dependenciesLock.Lock()
	defer dependenciesLock.Unlock()

	if len(deps) == 0 {
		healthDeps = nil
		return
	}

	healthDeps = append([]common.Dependency(nil), deps...)
}

func snapshotDependencies() []common.Dependency {
	dependenciesLock.RLock()
	defer dependenciesLock.RUnlock()

	if len(healthDeps) == 0 {
		return nil
	}

	snapshot := make([]common.Dependency, len(healthDeps))
	copy(snapshot, healthDeps)
	return snapshot
}

func buildHealthResponse() GetHealthzResponseObject {
	deps := snapshotDependencies()

	statuses := make([]dependencyHealthStatus, 0, len(deps))
	allRunning := true

	for _, dep := range deps {
		if dep == nil {
			continue
		}

		running := dep.IsRunning()
		if !running {
			allRunning = false
		}

		component := string(dep.ComponentName())
		if component == "" {
			component = "unknown"
		}

		statuses = append(statuses, dependencyHealthStatus{
			Component: component,
			Running:   running,
		})
	}

	id := getNodeIdentity()

	// Return 503 while draining (shutdown signal received) OR when any
	// dependency is unhealthy. Draining wins the status label so an
	// operator can tell "rolling out" apart from a genuine fault.
	if IsDraining() || !allRunning {
		status := "degraded"
		if IsDraining() {
			status = "draining"
		}
		resp := GetHealthz503JSONResponse{
			Status:       status,
			Dependencies: statuses,
		}
		if id != nil {
			resp.NodeId = id.NodeId
			resp.NodeType = id.NodeType
			resp.Flavor = id.Flavor
			resp.GRPCAddress = id.GRPCAddress
		}
		return resp
	}

	resp := GetHealthz200JSONResponse{
		Status:       "ok",
		Dependencies: statuses,
	}
	if id != nil {
		resp.NodeId = id.NodeId
		resp.NodeType = id.NodeType
		resp.Flavor = id.Flavor
		resp.GRPCAddress = id.GRPCAddress
	}
	return resp
}

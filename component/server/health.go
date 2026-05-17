package server

import (
	"sync"

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
)

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

	// Return 503 when any dependency is unhealthy
	if !allRunning {
		resp := GetHealthz503JSONResponse{
			Status:       "degraded",
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

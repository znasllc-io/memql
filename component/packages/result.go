package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// resultConcept is the ephemeral node kind an integration capability returns
// its payload on. Never persisted -- it is the wire envelope for a return
// value, the same one component/sitepublish uses.
const resultConcept = "integration:packages:result"

// resultNode wraps a capability's return payload.
func resultNode(payload map[string]any) []memorynodes.MemoryNode {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("packages:%d", time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}
}

// actorFromContext resolves who is asking.
//
// IsClusterOwner is read from the verified access context and never from an
// argument: the D9 gate turns on it, and a gate whose input a caller can send
// is not a gate. An absent access context yields the zero Actor -- no user id
// and not a cluster owner -- which is the fail-closed direction: a DSL-carrying
// package is refused rather than admitted.
func actorFromContext(ctx context.Context) Actor {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok {
		return Actor{}
	}
	return Actor{UserId: ac.UserId, IsClusterOwner: ac.IsClusterOwner()}
}

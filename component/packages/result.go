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
	return Actor{
		UserId: ac.UserId,
		// auth.CanAuthor IS this decision, already named, with the policy in
		// its own doc comment: "Engineering power: owner or developer only --
		// admin does NOT gain authoring (#1529 section 4)". Five other sites
		// call it (the MCP define surface, the MCP tool tiers, arming an
		// event-email rule, arming a routing rule), and a sixth hand-rolled
		// copy of its body is how those five come to disagree.
		//
		// Deploying a DSL domain IS authoring constructs -- it is authoring a
		// whole domain of them at once -- so this is the same authority
		// question, asked at a coarser grain.
		//
		// No IsClusterOwner disjunct. An empty installed catalog would answer
		// false here for every role including the owner, but that is fixed
		// where it happens -- component/memql's ReloadCapabilityCatalog
		// refuses to install a catalog with no roles -- rather than papered
		// over at this one gate, which would leave the owner able to deploy a
		// whole DSL DOMAIN while refused a single construct at the five
		// sibling sites.
		MayDeployDsl: auth.CanAuthor(auth.UserContext{Role: ac.Role}),
	}
}

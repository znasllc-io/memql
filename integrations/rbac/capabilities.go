// Package rbac exposes the relational governance decision
// (component/auth/rbac_governance.go) to the MemQL DSL as builtin
// capabilities. The DSL governance logic (dsl/rbac/logic.memql) resolves the
// actor + target ranks from the role catalog and hands them here; this
// integration performs the rank arithmetic the DSL grammar cannot express and
// returns the boolean decision.
//
// Epic 1, E1.3 (memql#2071). Pure + DB-free: the handlers take ranks + ids +
// verb as args, so the integration needs nothing from the engine beyond the
// plugin context.
package rbac

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Integration is the DSL-callable governance surface.
type Integration struct{}

// New constructs the rbac governance integration.
func New() *Integration { return &Integration{} }

// IntegrationName returns the stable identifier.
func (i *Integration) IntegrationName() string { return "rbac" }

// Capabilities returns the governance builtins.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "governPrincipal",
			Description: "Relational governance decision over (actor, target, verb) for the principal resource. Returns {allowed: bool}.",
			Handler:     i.handleGovernPrincipal,
			ArgsSchema: map[string]string{
				"actorUserId":    "string - actor's user id (for the self-edit check)",
				"actorRank":      "int (required) - actor's role rank (higher == more privileged)",
				"actorIsOwner":   "bool - whether the actor holds the owner rank",
				"targetUserId":   "string - target principal's user id",
				"targetRank":     "int (required) - target principal's role rank",
				"targetRoleSlug": "string - target principal's role slug (owner short-circuit)",
				"verb":           "string (required) - read | create | update | delete",
			},
		},
		{
			Name:        "canCreatePrincipal",
			Description: "The create != edit split: may an actor at actorRank create a principal at newRank? Returns {allowed: bool}.",
			Handler:     i.handleCanCreatePrincipal,
			ArgsSchema: map[string]string{
				"actorRank":    "int (required) - actor's role rank",
				"actorIsOwner": "bool - whether the actor holds the owner rank",
				"newRank":      "int (required) - rank the new principal would carry",
			},
		},
	}
}

func (i *Integration) handleGovernPrincipal(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	verb := stringArg(args, "verb")
	if verb == "" {
		return nil, fmt.Errorf("rbac.governPrincipal: verb is required")
	}

	actor := componentAuth.Principal{
		UserId:  stringArg(args, "actorUserId"),
		Rank:    intArg(args, "actorRank"),
		IsOwner: boolArg(args, "actorIsOwner"),
	}
	target := componentAuth.Principal{
		UserId:  stringArg(args, "targetUserId"),
		Rank:    intArg(args, "targetRank"),
		IsOwner: ownerFromArgs(args),
	}

	allowed := componentAuth.GovernPrincipal(actor, target, componentAuth.GovernVerb(verb))
	return decisionNode("governPrincipal", allowed), nil
}

func (i *Integration) handleCanCreatePrincipal(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor := componentAuth.Principal{
		Rank:    intArg(args, "actorRank"),
		IsOwner: boolArg(args, "actorIsOwner"),
	}
	allowed := componentAuth.CanCreatePrincipal(actor, intArg(args, "newRank"))
	return decisionNode("canCreatePrincipal", allowed), nil
}

// ownerFromArgs derives whether the TARGET is an owner: either the explicit
// targetRoleSlug is "owner", or (defensively) the target rank matches the
// owner rank is NOT assumed here -- the slug is authoritative so a custom rank
// named differently is never mistaken for an owner.
func ownerFromArgs(args map[string]any) bool {
	return stringArg(args, "targetRoleSlug") == "owner"
}

// decisionNode wraps a boolean decision in the single-node shape the DSL
// builtin call returns. The DSL reads `.First().payload.allowed`.
func decisionNode(name string, allowed bool) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(map[string]any{"allowed": allowed})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("integration:rbac:%s", name),
		Concept:   "integration:rbac:decision",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}

// --- arg coercion helpers (source-agnostic: DSL int64, JSON float64) -------

func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case int32:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	default:
		return 0
	}
}

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

	"github.com/uptrace/bun"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

// Integration is the DSL-callable governance and role-authoring surface.
//
// The governance half is PURE -- rank arithmetic over values the DSL resolved
// -- and needed nothing from the plugin context. The role builtins (epic
// memql#5166) write rows and count holders, so the integration now carries the
// engine and the pooled database handle. Both are optional: a node wired
// without them serves the governance builtins and refuses the authoring ones
// with a typed code, which is the honest answer rather than a panic.
type Integration struct {
	engine roleEngine
	bunDB  func() *bun.DB
}

// roleEngine is the ONE method the role builtins use, declared narrowly rather
// than taken as memql.IntegrationEngineAccess.
//
// The wide interface carries the AI, tool and skill surfaces as well, and a
// test double for this file would have to stub every one of them to assert a
// rendered mutation. Narrow here means the guards -- which are the whole
// product of roles.go -- are reachable by a test that fakes one method.
// memql.IntegrationEngineAccess satisfies it, so the plugin factory passes
// PluginContext.Engine straight through.
type roleEngine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// New constructs the rbac integration. Both arguments may be nil: a node wired
// without them serves the governance builtins and refuses the authoring ones
// with a typed code.
func New(engine roleEngine, bunDB func() *bun.DB) *Integration {
	return &Integration{engine: engine, bunDB: bunDB}
}

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
			Name: "roleCreate",
			Description: "Create a custom role with its grants, in one internal-origin write, " +
				"under the rank-below-caller, rank-not-taken, slug-not-taken and " +
				"grants-a-subset-of-the-caller's guards. Returns {ok, slug, code, message}.",
			Handler: i.handleRoleCreate,
			ArgsSchema: map[string]string{
				"slug":        "string (required) - stable kebab-case identifier, ^[a-z][a-z0-9-]{1,39}$",
				"name":        "string (required) - display name",
				"rank":        "int (required) - strictly below the caller's rank, and equal to no existing rung",
				"description": "string - what the role is for",
				"accountId":   "string - scope the role to one account; empty for a global role",
				"grants":      "array (required) - [{verb, resource}], every pair one the caller holds",
			},
		},
		{
			Name: "roleUpdate",
			Description: "Edit a custom role's name, description, rank, scope or grants. A rank " +
				"change additionally requires the caller to outrank every current holder. " +
				"Returns {ok, slug, code, message}.",
			Handler: i.handleRoleUpdate,
			ArgsSchema: map[string]string{
				"slug":        "string (required) - the role to edit; never itself editable",
				"name":        "string - new display name; absent leaves it unchanged",
				"description": "string - new description; absent leaves it unchanged",
				"rank":        "int - new rank; absent leaves it unchanged",
				"accountId":   "string - new scope; absent leaves it unchanged, empty clears it",
				"grants":      "array - the grant set AS A WHOLE; absent leaves it unchanged",
			},
		},
		{
			Name: "roleDeactivate",
			Description: "Retire a custom role. Refused while any active user holds it or any " +
				"pending invitation names it, with the count. Returns {ok, slug, code, message}.",
			Handler: i.handleRoleDeactivate,
			ArgsSchema: map[string]string{
				"slug": "string (required) - the role to retire",
			},
		},
		{
			Name: "grantSet",
			Description: "Write one v1:rbac:grant -- a person's or a group's allow or deny over one " +
				"(verb, resourceType) -- under the four governance rules: update on principal, the " +
				"capability held by the caller, the subject ranking no higher, never yourself. " +
				"Returns {ok, grantId, code, message}.",
			Handler: i.handleGrantSet,
			ArgsSchema: map[string]string{
				"subjectKind":  "string (required) - user | group",
				"subjectId":    "string (required) - the user's or group's id",
				"verb":         "string (required) - read | create | update | delete | execute",
				"resourceType": "string (required) - the resource, e.g. app:deployables or app:deployables/publish",
				"effect":       "string (required) - allow | deny",
			},
		},
		{
			Name: "grantRevoke",
			Description: "Revoke one v1:rbac:grant by id, writing active:false as a new version under the " +
				"same four rules a set applies. Returns {ok, grantId, code, message}.",
			Handler: i.handleGrantRevoke,
			ArgsSchema: map[string]string{
				"grantId": "string (required) - the grant's derived row id",
			},
		},
		{
			Name: "effectiveCapabilities",
			Description: "The CALLER's resolved capability set with provenance: one entry per (verb, resource) " +
				"the cluster knows, each allow or deny and which level answered (role, group or user). " +
				"Takes no subject; every signed-in person reads their own. Returns {ok, role, userId, entries}.",
			Handler:    i.handleEffectiveCapabilities,
			ArgsSchema: map[string]string{},
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

// intArg reads a numeric capability arg.
//
// SATURATES out of range (memql#4779). Every value read here is a privilege
// RANK feeding `CanCreatePrincipal`, which is `targetRank < actor.Rank` -- so
// a narrowing that flips the sign flips an authorization decision. This is the
// twin of component/memql's intWithOkFromAny; two packages decode the same
// rank, and they must not disagree about what an absurd one means.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	case int32:
		return int(v)
	case float64:
		return num.ClampFloat64(v)
	case float32:
		return num.ClampFloat64(float64(v))
	default:
		return 0
	}
}

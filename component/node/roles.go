package node

// roles.go -- the node roles as data (memql#5727).
//
// A node role is one engine binary: a Go build tag (app/build_<type>.go), an
// image (ENGINE_NODE_TYPES in scripts/lib/engine_build_args.sh), a Deployment
// under deploy/k8s, and a service in the architecture model (arch.yaml). The
// set used to be spelled by hand in each of those places, plus ValidNodeTypes
// below them, with nothing but convention tying them together. This table is
// the one Go reading of it: ValidNodeTypes is derived from it, cmd/platformgraph
// writes it into the embedded platform graph, and roles_test.go holds the build
// files, ENGINE_NODE_TYPES and arch.yaml to it.
//
// MESH is the one fact that is not a name. A mesh role joins the peer mesh:
// it discovers and dials a parent, relays broadcast events, and is what the
// worker dialer and the event bridge address. identity and edge are real roles
// that are NOT mesh workers -- identity is the node-token issuer and has no
// node token of its own to dial with, and nothing dials an edge -- which is why
// ValidNodeTypes, the mesh-dialable subset, has always left them out.

// NodeRole is one role a node can play in the cluster.
type NodeRole struct {
	// Type is the role's name: its build tag, its image suffix and its
	// MEMQL_NODE_TYPE value.
	Type NodeType
	// Description says what the role does, in one sentence.
	Description string
	// Mesh is true for the roles that join the peer mesh (ValidNodeTypes).
	Mesh bool
}

// nodeRoles is the closed role set, in the order ENGINE_NODE_TYPES lists it.
var nodeRoles = []NodeRole{
	{
		Type:        NodeTypeIdentity,
		Description: "The identity service: sign-in, the OAuth token endpoints, the JWKS feed and the node-token issuer.",
		Mesh:        false,
	},
	{
		Type:        NodeTypeBFF,
		Description: "The client edge: the gRPC and WebSocket surface every client dials, and the mesh root.",
		Mesh:        true,
	},
	{
		Type:        NodeTypeMCP,
		Description: "The Model Context Protocol head that exposes the deployment's tool surface to external MCP hosts.",
		Mesh:        true,
	},
	{
		Type:        NodeTypeAgent,
		Description: "Task execution, AI work and tool calling, and the gateway workers connect to.",
		Mesh:        true,
	},
	{
		Type:        NodeTypePlanner,
		Description: "Task planning and orchestration.",
		Mesh:        true,
	},
	{
		Type:        NodeTypeWorkbench,
		Description: "The sandboxed per-plan working environment agents run commands and write files in.",
		Mesh:        true,
	},
	{
		Type:        NodeTypeEdge,
		Description: "Serves the cluster's hosted sites, resolving a request host to a site row.",
		Mesh:        false,
	},
}

// Roles returns the closed role set, in ENGINE_NODE_TYPES order. The slice is a
// copy; callers may reorder it.
func Roles() []NodeRole {
	return append([]NodeRole(nil), nodeRoles...)
}

// RoleFor returns the role named t, and false when t names no role.
func RoleFor(t NodeType) (NodeRole, bool) {
	for _, r := range nodeRoles {
		if r.Type == t {
			return r, true
		}
	}
	return NodeRole{}, false
}

// meshNodeTypes is the mesh subset of the role table, as a set.
func meshNodeTypes() map[NodeType]bool {
	out := make(map[NodeType]bool, len(nodeRoles))
	for _, r := range nodeRoles {
		if r.Mesh {
			out[r.Type] = true
		}
	}
	return out
}

// Package node provides the distributed node identity, peer management, and
// NodeService gRPC server for inter-node communication in a MemQL cluster.
package node

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/core/id"
)

// NodeType identifies the role a node plays in the cluster.
type NodeType string

const (
	// NodeTypeAgent performs task execution and AI work.
	NodeTypeAgent NodeType = "agent"

	// NodeTypePlanner handles task planning and orchestration.
	NodeTypePlanner NodeType = "planner"

	// NodeTypeBFF serves domain-specific frontends.
	// Default when no build tag is specified.
	NodeTypeBFF NodeType = "bff"

	// NodeTypeWorkbench hosts the sandboxed per-Plan Linux working
	// environment. Receives WorkbenchForwardRequest envelopes on
	// NodeService.Stream and dispatches to the local workbench
	// integration. Agent nodes are the typical client.
	NodeTypeWorkbench NodeType = "workbench"

	// NodeTypeIdentity is the identity / auth service -- the node-token
	// ISSUER, not a mesh worker (so it is intentionally absent from
	// ValidNodeTypes). It runs the `/node/bootstrap` endpoint that every
	// other node calls to mint its class="node" JWT; it must never call
	// that endpoint on itself (see EnsureBearerToken). memql#570.
	NodeTypeIdentity NodeType = "identity"

	// NodeTypeMCP is the MCP (Model Context Protocol) server -- the protocol
	// head that exposes a deployment's tool surface to external MCP hosts
	// (Claude Desktop / Claude Code). A first-class node role selected by the
	// `mcp` build tag (epic memql#1529). It shares the cluster database and
	// joins the mesh like any other role; the tool surface it serves over the
	// wire lands in later phases (#1531+).
	NodeTypeMCP NodeType = "mcp"

	// NodeTypeEdge serves this cluster's hosted web surfaces, resolving a
	// request Host to a v1:platform:site row (epic memql#3700). Like
	// identity it is a real node role that is NOT a mesh worker, so it is
	// deliberately absent from ValidNodeTypes -- nothing dials an edge.
	NodeTypeEdge NodeType = "edge"
)

// ValidNodeTypes is the set of MESH node types: the roles that join the peer
// mesh. Derived from the role table in roles.go (the Mesh column), so the two
// cannot disagree; identity and edge are roles outside it.
var ValidNodeTypes = meshNodeTypes()

// Identity holds the runtime identity of this node.
type Identity struct {
	// ID is a unique identifier for this node instance, generated at startup.
	ID string

	// Type is the node's role in the cluster (from MEMQL_NODE_TYPE env var).
	Type NodeType

	// Version is the service version string.
	Version string

	// Address is the advertised NodeService gRPC address that peers use to
	// connect to this node (from MEMQL_NODE_ADDRESS env var).
	Address string

	// ParentAddress is the NodeService address of the peer that bootstrapped
	// this node. Empty for root nodes and standalone nodes.
	ParentAddress string

	// Flavor is the domain flavor within the node type. Empty for single-flavor types like cognition.
	// Read from MEMQL_NODE_FLAVOR env var.
	Flavor string

	// Labels contains arbitrary metadata key-value pairs.
	Labels map[string]string

	// BearerToken is the class="node" JWT this binary presents on
	// every outbound NodeService.Stream dial. Read from
	// MEMQL_NODE_TOKEN at startup; empty when no auth is required
	// (single-node dev / clusters that haven't rolled tokens out).
	// The peerConnection wraps its context with this token before
	// opening the stream so the remote NodeServer's class-pin
	// interceptor can verify. See #105.
	//
	// Mutated at runtime by the auth-failure re-mint path (memql#1521):
	// when identity's signing key rotates and a dial is rejected, the
	// reconnect loop re-fetches a freshly-signed token and stores it
	// here so every subsequent dial presents the live token. Guard all
	// access with tokenMu via BearerTokenValue / setBearerToken; direct
	// field reads from a goroutine race with the re-mint write.
	BearerToken string

	// tokenMu guards BearerToken now that it is mutated post-startup by
	// the re-mint path. Connections shared across goroutines read it on
	// every dial.
	tokenMu sync.RWMutex
}

// CompiledNodeType returns the node type this binary was built for: the one
// named by its `-tags <type>` build tag, or NodeTypeBFF for an untagged build.
// Set via the build-tagged compiled_*.go files, one per node type.
func CompiledNodeType() NodeType {
	return compiledNodeType
}

// CompiledNodeTypeIsTagged reports whether CompiledNodeType() came from a BUILD
// TAG rather than from the untagged default.
//
// The distinction is the whole precedence rule (memql#5115) and it cannot be
// recovered from the type alone: an untagged build compiles as bff, so
// `CompiledNodeType() == NodeTypeBFF` is true both for `go build .` -- which
// has no opinion and lets MEMQL_NODE_TYPE choose -- and for
// `go build -tags bff .`, which does and does not.
func CompiledNodeTypeIsTagged() bool {
	return compiledNodeTypeTagged
}

// resolveNodeType decides this binary's node type from the three things that
// can name one: the compiled type, whether that type came from a build tag,
// and MEMQL_NODE_TYPE. It returns the resolved type and whether an explicit
// MEMQL_NODE_TYPE was OVERRIDDEN by the tag -- a disagreement worth reporting,
// never worth obeying.
//
// A build tag selects which app/build_<type>.go runs, and therefore which
// integrations and transport layers the binary wired up. Those decisions are
// made at compile time and an environment variable cannot revisit them, so a
// tagged binary IS its tag and the env cannot say otherwise. Before memql#5115
// the env won for any mesh type, which meant a `-tags agent` binary whose
// manifest said `bff` reported NodeTypeBFF: an agent by every wiring decision
// in app/build_agent.go, a bff to the gate in app/cluster.go that starts the
// worker mesh's WorkerDialer.
//
// An UNTAGGED build has no such opinion, so MEMQL_NODE_TYPE selects -- and it
// is honoured VERBATIM, mesh type or not. `go build .` plus an env var is how
// one binary plays another role, and a value outside ValidNodeTypes must not
// silently fall back to bff: falling back would pass that same WorkerDialer
// gate and dial every peer tokenless. See #430.
func resolveNodeType(compiled NodeType, tagged bool, envType NodeType) (NodeType, bool) {
	if tagged {
		return compiled, envType != "" && envType != compiled
	}
	if envType != "" {
		return envType, false
	}
	return NodeTypeBFF, false
}

// NewIdentity creates a node Identity from environment variables.
//
// For tagged binaries (built with -tags agent/planner/bff/workbench/mcp/
// identity/edge) the compiled node type takes precedence over
// MEMQL_NODE_TYPE, which is reported and ignored when the two disagree.
// MEMQL_NODE_TYPE selects the type for untagged builds only. See
// resolveNodeType for why that split is the one the rest of the engine
// already assumes.
//
// Environment variables:
//   - MEMQL_NODE_TYPE: node type (default: bff)
//   - MEMQL_NODE_ADDRESS: advertised NodeService address
//   - MEMQL_PARENT_ADDRESS: parent peer's NodeService address
//   - MEMQL_NODE_ID: explicit node ID (default: generated UUID)
//   - MEMQL_NODE_LABELS: comma-separated key=value pairs
func NewIdentity(version string) *Identity {
	compiled := CompiledNodeType()
	envType := NodeType(strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_NODE_TYPE"))))

	nodeType, envOverridden := resolveNodeType(compiled, CompiledNodeTypeIsTagged(), envType)
	if envOverridden {
		// Warned, not refused. The value is now correct either way -- the
		// binary runs as what it was built as -- so refusing the boot would
		// trade a reported misconfiguration for an outage. What it means is
		// that a Deployment and the image it runs disagree, which is worth
		// a line naming both.
		slog.Warn("node type: MEMQL_NODE_TYPE disagrees with this binary's build tag; the build tag wins",
			"compiled_node_type", string(compiled),
			"memql_node_type", string(envType))
	}

	nodeId := strings.TrimSpace(os.Getenv("MEMQL_NODE_ID"))
	if nodeId == "" {
		// Derive the node id from the container/host hostname when the
		// operator left MEMQL_NODE_ID unset. This is the fallback for the
		// k8s `fieldRef: metadata.name` override the manifests use
		// (deploy/k8s/base/bff.yaml, local + staging + prod): under
		// `replicas: N` every replica shares the same env, so a static
		// MEMQL_NODE_ID would collide and PeerManager would key distinct
		// replicas under one id -- the chat-outage shape from memql#1042
		// (shared MEMQL_NODE_ID=bff-local). Every pod gets a unique
		// hostname, so os.Hostname() yields a stable, per-replica id with
		// zero static config, letting the local multi-replica cluster
		// reproduce cross-node fan-out bugs (memql#1212 / #1217). Falls
		// back to a random short id only when the hostname is unavailable
		// (it never is under Docker/k8s).
		if host, err := os.Hostname(); err == nil {
			nodeId = strings.TrimSpace(host)
		}
		if nodeId == "" {
			nodeId = id.NewShortId()
		}
	}

	labels := parseLabels(os.Getenv("MEMQL_NODE_LABELS"))

	return &Identity{
		ID:            nodeId,
		Type:          nodeType,
		Version:       version,
		Address:       strings.TrimSpace(os.Getenv("MEMQL_NODE_ADDRESS")),
		ParentAddress: strings.TrimSpace(os.Getenv("MEMQL_PARENT_ADDRESS")),
		Flavor:        strings.TrimSpace(os.Getenv("MEMQL_NODE_FLAVOR")),
		Labels:        labels,
		BearerToken:   strings.TrimSpace(os.Getenv("MEMQL_NODE_TOKEN")),
	}
}

// EnsureBearerToken fills in BearerToken when the operator left
// MEMQL_NODE_TOKEN empty but opted into self-bootstrap (set
// MEMQL_NODE_BOOTSTRAP_TOKEN + MEMQL_IDENTITY_VERIFIER_BASE_URL on this
// binary and the identity service). Calls the identity service's
// `/node/bootstrap` endpoint with the shared secret to mint a fresh
// class="node" JWT, then assigns it to id.BearerToken so the
// peerConnection's outbound dials present it.
//
// No-op when MEMQL_NODE_TOKEN was already set (operator-provisioned
// tokens win) or when the bootstrap preconditions aren't met
// (legacy "empty bearer token" behaviour preserved -- some single-
// node dev paths intentionally run without auth).
//
// Returns a non-nil error only when the operator opted into
// bootstrap (secret + identity URL both set) AND the mint call
// failed. The caller can choose whether to block startup
// (production-grade) or log + proceed; app/cluster.go logs + proceeds
// so an identity outage during boot doesn't deadlock the whole
// cluster startup, matching the lenient posture the empty-token
// branch has carried forward from #105's original design.
//
// memql#338.
func (id *Identity) EnsureBearerToken(ctx context.Context, logger *slog.Logger) error {
	if id == nil {
		return nil
	}
	if strings.TrimSpace(id.BearerTokenValue()) != "" {
		return nil
	}
	// The identity service is the node-token ISSUER -- it hosts the
	// `/node/bootstrap` endpoint other nodes call. It must NEVER
	// self-bootstrap: the bootstrap POST would target this node's own
	// `/node/bootstrap` (MEMQL_IDENTITY_VERIFIER_BASE_URL defaults to the
	// identity service) BEFORE its HTTP listener is up, so it retries
	// the full ~90s budget and blocks the listener from starting --
	// tripping the container healthcheck and, transitively, every other
	// node's bootstrap (which also targets identity). The identity node
	// runs as the mesh root with no outbound NodeService.Stream dials
	// that need a bearer token, so leaving it empty is correct. memql#570.
	if id.Type == NodeTypeIdentity {
		return nil
	}
	token, ok, err := maybeBootstrapNodeToken(ctx, logger, id.ID, string(id.Type))
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	id.setBearerToken(token)
	return nil
}

// BearerTokenValue returns the current node JWT under the token lock. Use
// this (not the bare field) from any goroutine: the re-mint path (memql#1521)
// writes BearerToken at runtime, so a direct read races the write.
func (id *Identity) BearerTokenValue() string {
	if id == nil {
		return ""
	}
	id.tokenMu.RLock()
	defer id.tokenMu.RUnlock()
	return id.BearerToken
}

// setBearerToken stores a freshly-acquired node JWT under the token lock.
func (id *Identity) setBearerToken(tok string) {
	if id == nil {
		return
	}
	id.tokenMu.Lock()
	defer id.tokenMu.Unlock()
	id.BearerToken = tok
}

// CanRemintBearerToken reports whether this node can re-fetch its node token
// on an auth failure (memql#1521): the self-bootstrap mint path must be
// configured (MEMQL_NODE_BOOTSTRAP_TOKEN + MEMQL_IDENTITY_VERIFIER_BASE_URL). When
// the token was provisioned out-of-band via MEMQL_NODE_TOKEN there is nothing
// to re-mint, so connections leave their reauth hook unset and fall back to
// the legacy reconnect-with-the-same-token behaviour. The identity node never
// re-mints (it is the issuer, not a client).
func (id *Identity) CanRemintBearerToken() bool {
	if id == nil || id.Type == NodeTypeIdentity {
		return false
	}
	return bootstrapMintConfigured()
}

// RemintBearerToken re-acquires a fresh class="node" JWT from identity and
// stores it on the Identity (memql#1521). Called by a connection's reauth
// hook after an Unauthenticated / unknown-kid rejection -- identity now
// serves the rotated signing key, so the new token verifies where the old one
// looped forever. Returns the new token (also stored) or an error if the
// re-mint failed (identity unreachable, wrong secret, etc.).
func (id *Identity) RemintBearerToken(ctx context.Context, logger *slog.Logger) (string, error) {
	if id == nil {
		return "", nil
	}
	tok, err := remintNodeToken(ctx, logger, id.ID, string(id.Type))
	if err != nil {
		return "", err
	}
	id.setBearerToken(tok)
	return tok, nil
}

// NodeId returns the node's unique identifier.
func (id *Identity) NodeId() string {
	return id.ID
}

// NodeAddress returns the node's advertised address.
func (id *Identity) NodeAddress() string {
	return id.Address
}

// IsStandalone is deprecated -- standalone mode no longer exists.
// Always returns false. All nodes are one of: cognition, agent, planner, bff.
func (id *Identity) IsStandalone() bool {
	return false
}

// HasParent returns true if this node has a parent peer.
func (id *Identity) HasParent() bool {
	return id.ParentAddress != ""
}

// parseLabels parses "key1=val1,key2=val2" into a map.
func parseLabels(raw string) map[string]string {
	labels := make(map[string]string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return labels
	}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if k, v, ok := strings.Cut(pair, "="); ok {
			labels[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return labels
}

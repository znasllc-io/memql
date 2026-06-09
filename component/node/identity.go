// Package node provides the distributed node identity, peer management, and
// NodeService gRPC server for inter-node communication in a memQL cluster.
package node

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

// NodeType identifies the role a node plays in the cluster.
type NodeType string

const (
	// NodeTypeCognition handles voice turn-taking and conversation management.
	NodeTypeCognition NodeType = "cognition"

	// NodeTypeAgent performs task execution and SI work.
	NodeTypeAgent NodeType = "agent"

	// NodeTypePlanner handles task planning and orchestration.
	NodeTypePlanner NodeType = "planner"

	// NodeTypeBFF serves domain-specific frontends.
	// Default when no build tag is specified.
	NodeTypeBFF NodeType = "bff"

	// NodeTypeVoice handles the audio I/O pipeline (ASR, TTS, LiveKit).
	NodeTypeVoice NodeType = "voice"

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
)

// ValidNodeTypes is the set of recognized node types.
var ValidNodeTypes = map[NodeType]bool{
	NodeTypeCognition: true,
	NodeTypeAgent:     true,
	NodeTypePlanner:   true,
	NodeTypeBFF:       true,
	NodeTypeVoice:     true,
	NodeTypeWorkbench: true,
}

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
	BearerToken string
}

// CompiledNodeType returns the node type this binary was built for.
// Default builds (no tag) return NodeTypeBFF. Tagged builds (e.g. -tags cognition)
// return their specific type. Set via build-tagged compiled_*.go files.
func CompiledNodeType() NodeType {
	return compiledNodeType
}

// NewIdentity creates a node Identity from environment variables.
//
// For tagged binaries (built with -tags cognition/agent/planner/bff),
// the compiled node type takes precedence over MEMQL_NODE_TYPE.
// MEMQL_NODE_TYPE can override the default for untagged builds.
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

	var nodeType NodeType
	if ValidNodeTypes[compiled] {
		// Tagged binary: compiled type wins
		nodeType = compiled
	}
	if ValidNodeTypes[envType] {
		// Environment variable override (for untagged binaries)
		nodeType = envType
	} else if envType != "" {
		// The operator set an explicit MEMQL_NODE_TYPE that isn't a
		// mesh worker/bff type (e.g. "identity" for the auth service).
		// Honor it verbatim rather than silently falling back to the
		// compiled bff default -- defaulting to bff would (wrongly)
		// pass the `Type == NodeTypeBFF` gate in app/cluster.go and
		// start the worker-mesh WorkerDialer, which then dials every
		// worker tokenless (the identity service has no node token),
		// spamming "node auth: token extraction failed" every 30s.
		// A non-bff type fails that gate, so no dialer is started.
		nodeType = envType
	}
	if nodeType == "" {
		nodeType = NodeTypeBFF
	}

	nodeId := strings.TrimSpace(os.Getenv("MEMQL_NODE_ID"))
	if nodeId == "" {
		// Derive the node id from the container/host hostname when the
		// operator left MEMQL_NODE_ID unset. This is the Docker-Compose
		// equivalent of the k8s `fieldRef: metadata.name` override the
		// staging manifests use (deploy/k8s/base/bff.yaml): under
		// `deploy.replicas: N` every replica shares the same env, so a
		// static MEMQL_NODE_ID would collide and PeerManager would key
		// distinct replicas under one id -- the chat-outage shape from
		// memql#1042 (shared MEMQL_NODE_ID=bff-local). Compose assigns
		// each replica a unique container hostname
		// (`<project>-<service>-<n>`), so os.Hostname() yields a stable,
		// per-replica id with zero static config, letting the local
		// multi-replica cluster reproduce cross-node fan-out bugs
		// (memql#1212 / #1217). Falls back to a random short id only when
		// the hostname is unavailable (it never is under Docker/k8s).
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
// MEMQL_NODE_BOOTSTRAP_TOKEN + IDENTITY_VERIFIER_BASE_URL on this
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
	if strings.TrimSpace(id.BearerToken) != "" {
		return nil
	}
	// The identity service is the node-token ISSUER -- it hosts the
	// `/node/bootstrap` endpoint other nodes call. It must NEVER
	// self-bootstrap: the bootstrap POST would target this node's own
	// `/node/bootstrap` (IDENTITY_VERIFIER_BASE_URL defaults to the
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
	id.BearerToken = token
	return nil
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

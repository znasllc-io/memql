package app

import (
	"context"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/server"
)

// nodeTokenRevocationResolver bridges the node package's
// NodeTokenRevocationResolver port to a live *identity.Store. Lives
// in the app/ layer so the node package stays free of any identity-
// store dependency -- node sits below identity in the dependency
// graph; this bridge connects them at wire time.
//
// "Not found" is treated as not-revoked: an operator-CLI-minted token
// from before memql#347's persistence shipped has no row to look up,
// but its JWT signature is still valid. Only an explicit row state
// (Active == false from a /admin/tokens revoke) stops it. memql#349.
type nodeTokenRevocationResolver struct {
	Store *identity.Store
}

// IsNodeTokenRevoked implements node.NodeTokenRevocationResolver.
// Looks up the v1:identity:identity[node_token] row by
// (nodeType, nodeId) via the store API memql#347 shipped. Returns
// true when the row exists AND Active == false (operator-revoked).
// Lookup failures surface as errors so the interceptor logs +
// rejects rather than silently admitting traffic on a partial check.
func (r *nodeTokenRevocationResolver) IsNodeTokenRevoked(ctx context.Context, nodeType, nodeId string) (bool, error) {
	if r == nil || r.Store == nil {
		return false, nil
	}
	row, err := r.Store.LookupNodeTokenIdentityByBinding(ctx, identity.NodeTokenBinding{
		NodeType: nodeType,
		NodeId:   nodeId,
	})
	if err != nil {
		return false, err
	}
	if row == nil {
		// No persisted row -- operator-CLI mint that pre-dates #347
		// persistence, or single-node dev. Treat as not-revoked; the
		// JWT signature is still the authority.
		return false, nil
	}
	return !row.Active, nil
}

// cluster wires distributed node components via bootstrap strategy and emits
// the system.startup event with infrastructure metadata for automations.
func (a *App) cluster() {
	nodeIdentity := node.NewIdentity(a.Version)

	// Self-bootstrap a node-class JWT when MEMQL_NODE_TOKEN is empty
	// but the operator opted into self-bootstrap via
	// MEMQL_NODE_BOOTSTRAP_TOKEN + IDENTITY_VERIFIER_BASE_URL (see
	// memql#338 and component/node/bootstrap_token.go). No-op when
	// MEMQL_NODE_TOKEN was provisioned out-of-band; logs + proceeds
	// on bootstrap failure rather than blocking cluster startup
	// (the empty-token path is the legacy fallback every dev / unit-
	// test setup has been running under for months).
	if err := nodeIdentity.EnsureBearerToken(context.Background(), a.Logger); err != nil {
		if a.Logger != nil {
			a.Logger.Warn("node bootstrap failed; proceeding with empty BearerToken (every NodeService.Stream call will fail with 'authorization header missing' until MEMQL_NODE_TOKEN is set out-of-band or the bootstrap path is fixed)",
				"error", err.Error(),
				"node_type", nodeIdentity.Type,
				"node_id", nodeIdentity.ID,
			)
		}
	}

	// DB-based peer discovery: query v1:cluster:node for an existing
	// healthy peer to connect to. If found, set identity.ParentAddress
	// so ParentConnector will dial that peer. First node in a fresh
	// cluster finds nothing and becomes the mesh root.
	bootCtx := node.BootstrapContext{
		Identity: nodeIdentity,
		Logger:   a.Logger,
		EventBus: a.eventBus,
		Engine:   a.engine,
		Version:  a.Version,
		Wiring:   a.wiring,
	}
	node.DiscoverPeerAddress(bootCtx)

	bootstrap := node.BootstrapFor(nodeIdentity.Type)
	nodeDeps, err := bootstrap.NodeDependencies(bootCtx)
	if err != nil {
		a.fatal("failed to create node dependencies", "error", err)
	}
	a.Dependencies = append(a.Dependencies, nodeDeps...)

	// Wire QueryProxy for cross-node query routing. The proxy maps
	// concept domain prefixes to owning node types. When the gRPC
	// handler receives a query for a concept not in the local registry,
	// it logs the routing target (actual forwarding via NodeService is
	// a follow-up).
	//
	// Wire AI/voice forwarding across the cluster:
	//   * On BFF binaries: install the outbound SIForwardRouter on the
	//     gRPC server (so AI handlers proxy to workers), the
	//     WorkerDialer to open one outbound NodeService stream per
	//     worker type (seeded by MEMQL_WORKER_PEERS, then reconciled
	//     against v1:cluster:node via event + 30s ticker), and the
	//     response sinks on every inbound channel the BFF might receive
	//     SIForwardResponse over (NodeServer, ParentConnector, the
	//     dialer's own streams).
	//   * On worker binaries: install the SIForwardHandler shim on
	//     NodeServer so each inbound SIForwardRequest dispatches into
	//     the local grpcServer's AI handlers as if the client had
	//     connected directly.
	var peerMgr *node.PeerManager
	var nodeServer *node.NodeServer
	var parentConnector *node.ParentConnector
	for _, dep := range nodeDeps {
		switch d := dep.(type) {
		case *node.PeerManager:
			peerMgr = d
		case *node.NodeServer:
			nodeServer = d
		case *node.ParentConnector:
			parentConnector = d
		}
	}

	// Install the class="node" JWT enforcement interceptor on
	// NodeService.Stream (#105) + the memql#349 revocation gate that
	// rides alongside. When the verifier isn't wired (single-node dev
	// / binaries with no identity) the interceptor is a no-op pass-
	// through; otherwise NodeService.Stream rejects every non-node-
	// class bearer AND every NodeService.Stream open from a node
	// whose v1:identity:identity[node_token] row is Active == false
	// (operator has revoked via /admin/tokens). The revocation check
	// short-circuits to a 5s in-process cache so a healthy peer
	// pinging every ~30s costs at most one DB read per cache window.
	if nodeServer != nil && a.identityVerifier != nil {
		var revCheck *node.NodeRevocationCheck
		if a.engine != nil {
			revCheck = &node.NodeRevocationCheck{
				Resolver: &nodeTokenRevocationResolver{
					Store: &identity.Store{Engine: a.engine, Logger: a.Logger},
				},
			}
		}
		nodeServer.SetAuthInterceptor(node.NodeClassStreamInterceptorWithRevocation(a.identityVerifier, revCheck, a.Logger))
	}

	if peerMgr != nil {
		if nodeIdentity.Type == node.NodeTypeBFF {
			// BFF-side forwarding: create the router, plug it into the
			// gRPC AI handlers, and point the parent connector at it
			// so it can dispatch inbound SIForwardResponse messages.
			forwarder := memqlgrpc.NewAiForwardRouter(peerMgr, a.Logger)
			if a.grpcServer != nil {
				a.grpcServer.SetAiForwarder(forwarder)
			}
			if nodeServer != nil {
				nodeServer.SetAiForwardResponseSink(forwarder)
			}
			if parentConnector != nil {
				parentConnector.SetAiForwardResponseSink(forwarder)
			}

			// Install the WorkerDialer so the BFF opens outbound
			// NodeService streams to each worker. Without this the
			// forwarder has no *peerConnection to Send on -- accepting
			// inbound streams (workers dialing us) would only give us
			// a server-side send handle, and NodeClientMessage is a
			// client->server message. Seeds from MEMQL_WORKER_PEERS
			// cover first-boot before v1:cluster:node is populated;
			// DB discovery (via engine query + event subscription)
			// takes over once the cluster has registered its nodes.
			seeds := node.ParseWorkerPeers(os.Getenv("MEMQL_WORKER_PEERS"))
			if dialer := node.NewWorkerDialer(
				nodeIdentity,
				peerMgr,
				a.engine,
				a.eventBus,
				seeds,
				a.Logger,
			); dialer != nil {
				dialer.SetAiForwardResponseSink(forwarder)
				a.Dependencies = append(a.Dependencies, dialer)
			}
		} else {
			// Worker-side: install the shim handler on NodeServer so
			// inbound SIForwardRequest messages route into the local
			// gRPC AI handlers.
			if nodeServer != nil && a.grpcServer != nil {
				nodeServer.SetAiForwardHandler(a.grpcServer.SIForwardHandler())
			}

			// Cognition + Planner both originate AgentGenerateTurnMsg
			// forwards to agent peers (cognition for chat-driven
			// turns, planner for Plan-execution dispatch). Each gets
			// its own SIForwardRouter + WorkerDialer narrowed to
			// agent peers. The router is also exposed to the
			// integration of the same name so it can Forward() from
			// its handlers.
			if nodeIdentity.Type == node.NodeTypeCognition || nodeIdentity.Type == node.NodeTypePlanner {
				forwarder := memqlgrpc.NewAiForwardRouter(peerMgr, a.Logger)
				a.agentForwarder = forwarder
				if nodeServer != nil {
					nodeServer.SetAiForwardResponseSink(forwarder)
				}
				if parentConnector != nil {
					parentConnector.SetAiForwardResponseSink(forwarder)
				}

				seeds := node.ParseWorkerPeers(os.Getenv("MEMQL_WORKER_PEERS"))
				if dialer := node.NewWorkerDialer(
					nodeIdentity,
					peerMgr,
					a.engine,
					a.eventBus,
					seeds,
					a.Logger,
				); dialer != nil {
					dialer.SetDialTypes(node.NodeTypeAgent)
					dialer.SetAiForwardResponseSink(forwarder)
					a.Dependencies = append(a.Dependencies, dialer)
				}

				// Inject the freshly-created forwarder onto the
				// integration of this node-type. Done here (not in
				// the integration phase) because the forwarder
				// doesn't exist until the cluster phase has the
				// PeerManager + node topology resolved. Each helper
				// is build-tagged for its node type and reads back
				// the integration via type assertion.
				switch nodeIdentity.Type {
				case node.NodeTypeCognition:
					a.attachAgentForwarderToCognition(forwarder)
				case node.NodeTypePlanner:
					a.attachAgentForwarderToPlanner(forwarder)
				}
			}

			// Workbench forwarding (cluster-mode optional). Mirrors the
			// cognition/planner -> agent pattern but for workbench peers:
			//
			//   * On agent binaries when MEMQL_WORKBENCH_REMOTE is set:
			//     create a workbench.ForwardRouter, install it as the
			//     response sink on every inbound channel, install the
			//     dialer narrowed to NodeTypeWorkbench peers, and wire
			//     the router into the local workbench integration so its
			//     dispatch handler delegates to it.
			//
			//   * On workbench binaries: install the workbench-side
			//     ForwardHandler on NodeServer so inbound
			//     WorkbenchForwardRequest envelopes dispatch into the
			//     local Integration's exec/fs/http handlers.
			//
			// All wiring is best-effort -- a missing integration / engine
			// / nodeServer logs and continues without breaking the boot
			// path. The default (single-node) mode keeps working because
			// the integration falls back to local dispatch when the
			// router returns ErrNoWorkbenchPeer.
			a.wireWorkbenchForwarding(nodeIdentity, peerMgr, nodeServer, parentConnector)
		}
	}

	// Store node identity for startup event emission.
	a.nodeIdentity = nodeIdentity

	// Expose node identity in /healthz so load balancers can route by type.
	server.SetNodeIdentity(
		nodeIdentity.ID,
		string(nodeIdentity.Type),
		nodeIdentity.Flavor,
		os.Getenv("MEMQL_GRPC_ADDRESS"),
	)

	a.Logger.Info("cluster node initialized",
		"node_id", nodeIdentity.ID,
		"node_type", string(nodeIdentity.Type),
		"node_address", nodeIdentity.Address,
		"parent_address", nodeIdentity.ParentAddress,
		"bootstrap", bootstrap.Description(),
	)
}

// EmitSystemStartup publishes the system.startup event with infrastructure metadata.
// Called from main.go after all dependencies have started. Waits briefly for the
// automation scheduler to finish registering event subscriptions.
func (a *App) EmitSystemStartup() {
	if a.eventBus == nil || a.nodeIdentity == nil {
		return
	}

	// Wait for automation scheduler to finish registering event subscriptions.
	// Dependencies start asynchronously, so the scheduler may still be loading
	// automations when this is called. A brief delay ensures subscriptions are registered.
	time.Sleep(500 * time.Millisecond)

	nodePayload := map[string]any{
		"id":      a.nodeIdentity.ID,
		"type":    string(a.nodeIdentity.Type),
		"address": a.nodeIdentity.Address,
		"version": a.Version,
		"labels":  a.nodeIdentity.Labels,
	}
	if a.nodeIdentity.Flavor != "" {
		nodePayload["flavor"] = a.nodeIdentity.Flavor
	}
	payload := map[string]any{
		"node": nodePayload,
	}

	// Parse database info from DSN.
	if dbInfo := parseDatabaseInfo(); dbInfo != nil {
		payload["database"] = dbInfo
	}

	// Include identity-provider info (non-secret fields only).
	if idpInfo := parseIdentityProviderInfo(); idpInfo != nil {
		payload["identityProvider"] = idpInfo
	}

	// Include environment/region.
	payload["environment"] = firstNonEmptyStr(os.Getenv("MEMQL_ENVIRONMENT"), os.Getenv("MEMQL_REGION"), "development")
	payload["region"] = firstNonEmptyStr(os.Getenv("MEMQL_REGION"), "local")

	a.eventBus.Publish(events.Event{
		Topic:   events.TopicSystemStartup,
		Kind:    events.KindSystemStartup,
		Payload: payload,
	})

	a.Logger.Info("system.startup event emitted",
		"node_type", string(a.nodeIdentity.Type),
		"node_id", a.nodeIdentity.ID,
	)
}

// EmitSystemShutdown publishes the system.shutdown event.
func (a *App) EmitSystemShutdown() {
	if a.eventBus == nil || a.nodeIdentity == nil {
		return
	}

	a.eventBus.Publish(events.Event{
		Topic: events.TopicSystemShutdown,
		Kind:  events.KindSystemShutdown,
		Payload: map[string]any{
			"node": map[string]any{
				"id":   a.nodeIdentity.ID,
				"type": string(a.nodeIdentity.Type),
			},
		},
	})
}

// parseDatabaseInfo extracts non-secret database metadata from the DSN env var.
func parseDatabaseInfo() map[string]any {
	dsn := os.Getenv("MEMORY_NODES_DATABASE_DSN")
	if dsn == "" {
		return nil
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return nil
	}

	info := map[string]any{
		"host":   u.Hostname(),
		"port":   firstNonEmptyStr(u.Port(), "5432"),
		"dbName": strings.TrimPrefix(u.Path, "/"),
		"engine": "postgresql",
	}

	// Extract sslmode from query params.
	if sslMode := u.Query().Get("sslmode"); sslMode != "" {
		info["sslMode"] = sslMode
	}

	return info
}

// parseIdentityProviderInfo extracts non-secret identity-service
// metadata for the v1:cluster:identityProvider row stamped on
// system.startup. Returns nil when no verifier is configured (dev /
// no-auth) so the cluster row is omitted rather than carrying a
// half-populated entry.
func parseIdentityProviderInfo() map[string]any {
	baseURL := strings.TrimRight(os.Getenv("IDENTITY_VERIFIER_BASE_URL"), "/")
	if baseURL == "" {
		// Identity binaries set IDENTITY_BASE_URL instead (they don't
		// verify against a remote JWKS — they own the keys). Fall back
		// to that so the row populates correctly on identity nodes.
		baseURL = strings.TrimRight(os.Getenv("IDENTITY_BASE_URL"), "/")
	}
	if baseURL == "" {
		return nil
	}

	info := map[string]any{
		"name":         firstNonEmptyStr(os.Getenv("IDENTITY_BRAND_NAME"), "memQL Identity"),
		"providerType": "oidc",
		"issuerUrl":    firstNonEmptyStr(os.Getenv("IDENTITY_VERIFIER_EXPECTED_ISSUER"), baseURL),
	}
	if aud := firstNonEmptyStr(os.Getenv("IDENTITY_VERIFIER_AUDIENCE"), os.Getenv("IDENTITY_JWT_AUDIENCE")); aud != "" {
		info["acceptedAudiences"] = []string{aud}
	}
	if jwksURL := firstNonEmptyStr(os.Getenv("IDENTITY_VERIFIER_JWKS_URL"), os.Getenv("IDENTITY_JWKS_URL")); jwksURL != "" {
		info["jwksUrl"] = jwksURL
	}
	return info
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}


package app

import (
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/visionarys-io/memql/component/events"
	memqlgrpc "github.com/visionarys-io/memql/component/grpc"
	"github.com/visionarys-io/memql/component/node"
	"github.com/visionarys-io/memql/component/server"
)

// cluster wires distributed node components via bootstrap strategy and emits
// the system.startup event with infrastructure metadata for automations.
func (a *App) cluster() {
	nodeIdentity := node.NewIdentity(a.Version)

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
	//   * On BFF binaries: install the outbound AiForwardRouter on the
	//     gRPC server (so AI handlers proxy to workers), the
	//     WorkerDialer to open one outbound NodeService stream per
	//     worker type (seeded by MEMQL_WORKER_PEERS, then reconciled
	//     against v1:cluster:node via event + 30s ticker), and the
	//     response sinks on every inbound channel the BFF might receive
	//     AiForwardResponse over (NodeServer, ParentConnector, the
	//     dialer's own streams).
	//   * On worker binaries: install the AiForwardHandler shim on
	//     NodeServer so each inbound AiForwardRequest dispatches into
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

	if peerMgr != nil {
		qp := node.NewQueryProxy(nodeIdentity, peerMgr, nil, a.Logger)
		if a.grpcServer != nil {
			a.grpcServer.SetQueryProxy(qp)
		}

		if nodeIdentity.Type == node.NodeTypeBFF {
			// BFF-side forwarding: create the router, plug it into the
			// gRPC AI handlers, and point the parent connector at it
			// so it can dispatch inbound AiForwardResponse messages.
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
			// inbound AiForwardRequest messages route into the local
			// gRPC AI handlers.
			if nodeServer != nil && a.grpcServer != nil {
				nodeServer.SetAiForwardHandler(a.grpcServer.AiForwardHandler())
			}

			// Cognition + Planner both originate AgentGenerateTurnMsg
			// forwards to agent peers (cognition for chat-driven
			// turns, planner for Plan-execution dispatch). Each gets
			// its own AiForwardRouter + WorkerDialer narrowed to
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

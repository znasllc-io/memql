package app

import (
	"context"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/automations"
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
	// MEMQL_NODE_BOOTSTRAP_TOKEN + MEMQL_IDENTITY_VERIFIER_BASE_URL (see
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
	// Only real mesh node types participate in the peer mesh. A node
	// running an explicit non-mesh MEMQL_NODE_TYPE (e.g. the identity
	// auth service, which has no node token) must NOT discover + dial a
	// parent: the ParentConnector would dial it tokenless and spam
	// "node auth: token extraction failed" on the target every retry.
	if node.ValidNodeTypes[nodeIdentity.Type] {
		node.DiscoverPeerAddress(bootCtx)
	}

	bootstrap := node.BootstrapFor(nodeIdentity.Type)
	nodeDeps, err := bootstrap.NodeDependencies(bootCtx)
	if err != nil {
		a.fatal("failed to create node dependencies", "error", err)
	}
	a.Dependencies = append(a.Dependencies, nodeDeps...)

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
	var eventBridge *node.EventBridge
	for _, dep := range nodeDeps {
		switch d := dep.(type) {
		case *node.PeerManager:
			peerMgr = d
		case *node.NodeServer:
			nodeServer = d
		case *node.ParentConnector:
			parentConnector = d
		case *node.EventBridge:
			eventBridge = d
		}
	}

	// Phase 1 of epic memql#1259 (memql#1264): route the chat-reply path
	// (utterance / presence / canvasState) through the durable DeliverySubstrate
	// (memql#1263), fixing the live cross-replica drop where a reply produced on
	// a worker never reached the bff replica that owns the user's WebSocket.
	//
	//   - Producer side (every mesh node): on a locally-produced chat-reply
	//     event, ChatReplyDelivery durably Publishes a Deliverable keyed by
	//     space:<id>. The durable outbox row is the cross-replica guarantee.
	//   - Consumer side (bff only): the bff Subscribes per space and fans the
	//     durable stream back onto its local bus, where the browser's gRPC
	//     subscription already consumes it -- so it gets the reply regardless of
	//     which node produced it. EventBridge then SUPPRESSES the inbound mesh
	//     copy of these topics on the bff (SuppressInboundChatReply) so the
	//     browser receives them exactly once via the substrate.
	//
	// The mesh forward itself is left fully intact: it still carries the human
	// utterance to the cognition/agent worker that produces the reply (the chat
	// TRIGGER path), and still publishes these topics on worker local buses.
	// Only the bff's redundant inbound copy is dropped.
	//
	// memql#1289 wires the EventBridge as the substrate's mesh FAST-PATH (the
	// low-latency half deferred from #1264): on a durable Publish the substrate
	// also broadcasts a best-effort hint over the mesh, and the owning replica's
	// inbound handler feeds it to HandleFastPath so a cross-node subscriber wakes
	// INSTANTLY instead of falling back to the 250ms durable-poll floor. The
	// durable outbox remains the delivery GUARANTEE -- the hint only short-
	// circuits latency, never advances a cursor, and dedups by EventID against
	// the durable pull so the two paths collapse to exactly one delivery (ADR
	// 4.5). This is the prerequisite for the #1266 streaming cutover. RPC
	// dispatch = #1265, retirement of the old machinery = #1267 -- out of scope.
	if node.ValidNodeTypes[nodeIdentity.Type] && a.eventBus != nil {
		dbGetter := func() *bun.DB {
			if a.db == nil {
				return nil
			}
			return a.db.BunDB()
		}
		// Build the durable delivery substrate ONCE and stash it on the App so
		// every migrated path shares it: chat-reply (memql#1264) here, and the
		// client-tool RPC return leg (memql#1265) in the forwarder-wiring block
		// below.
		//
		// memql#1289 constructs it WITH the EventBridge fast-path when the mesh is
		// up: on a durable Publish the substrate also broadcasts a best-effort hint
		// so a cross-node subscriber wakes instantly instead of at the 250ms
		// durable-poll floor. The sink (HandleFastPath) is wired symmetrically so
		// this node also consumes inbound hints for the spaces it owns. When
		// EventBridge is absent (single-node / non-mesh binary) the substrate runs
		// durable-only -- a supported mode (ADR 4.5 rule 3).
		var substrate *node.Substrate
		if eventBridge != nil {
			substrate = node.NewSubstrateWithMeshFastPath(dbGetter, eventBridge, a.Logger)
			eventBridge.SetFastPathSink(substrate.HandleFastPath)
		} else {
			substrate = node.NewSubstrateWithStore(dbGetter, a.Logger)
		}
		a.deliverySubstrate = substrate
		// Streaming over the substrate (memql#1266): hand the substrate to the
		// gRPC server so the token-streaming + audio-streaming handlers produce
		// (worker) / consume (WS-owning bff) ordered chunks over it, surviving a
		// mid-stream replica switch. Set on every mesh node; the handlers pick the
		// producer vs consumer role from the node type at call time.
		if a.grpcServer != nil {
			a.grpcServer.SetDeliverySubstrate(substrate, nodeIdentity.ID)
		}
		// Retention sweep for the substrate's tables (memql#1328): mesh_outbox
		// rows older than the MEMQL_MESH_OUTBOX_RETENTION watermark (default
		// 24h; 0/negative disables) and mesh_cursor rows stale past the same
		// watermark are deleted on an hourly per-node ticker, mirroring the
		// automation_execution_claims pruning precedent (memql#561). Deletes
		// are idempotent, so every replica sweeping concurrently is harmless --
		// the same multi-node posture as the #561 prune. mesh_key_seq is
		// deliberately not swept (see pgOutboxStore.SweepOlderThan).
		a.Dependencies = append(a.Dependencies, node.NewOutboxRetention(dbGetter, a.Logger))

		isBFF := nodeIdentity.Type == node.NodeTypeBFF
		if crd := node.NewChatReplyDelivery(nodeIdentity, substrate, a.eventBus, isBFF, a.Logger); crd != nil {
			if isBFF && eventBridge != nil {
				eventBridge.SuppressInboundChatReply(true)
			}
			a.Dependencies = append(a.Dependencies, crd)
			a.Logger.Info("chat-reply path routed through durable delivery substrate",
				"node_id", nodeIdentity.ID, "node_type", string(nodeIdentity.Type), "is_bff", isBFF)
		}

		// Plan-lifecycle path on the durable substrate (memql#1495): plan
		// graph events (graph.node.created/updated.v1:planner:plan) get a real
		// at-least-once delivery leg, mirroring ChatReplyDelivery. Before this
		// they rode the EventBridge mesh broadcast ONLY -- a broadcast dropped
		// while the planner was routeless (the #1388 peer-decay incident)
		// stranded the Plan, recovered only by the #1389 DB-poll watchdog (a
		// backstop, not a guarantee). Now:
		//   - Producer side (every mesh node): on a locally-produced plan
		//     graph event, PlanDelivery durably Publishes a Deliverable keyed
		//     by the fixed plan:lifecycle key. The BFF writes new Plans and the
		//     planner writes status transitions, so both run the producer.
		//   - Consumer side (planner only): the planner Subscribes once and
		//     fans the durable stream back onto its local bus, where the
		//     PlannerAgentLoop subscribers already consume it -- catching up on
		//     cursor-pull replay after a route gap.
		// The mesh broadcast is left fully intact as the latency fast-path; the
		// inbound copy is NOT suppressed (unlike chat-reply) because the
		// planner's consumers are already idempotent -- the per-plan once-guard
		// (memql#1155) + the cross-replica ClusterExecutionGuard
		// (planId@startedAt, memql#1363) make a duplicate (mesh + replay)
		// collapse downstream, so PlanDelivery can only ever ADD a missed
		// delivery, never double-execute.
		isPlanner := nodeIdentity.Type == node.NodeTypePlanner
		if pd := node.NewPlanDelivery(nodeIdentity, substrate, a.eventBus, isPlanner, a.Logger); pd != nil {
			a.Dependencies = append(a.Dependencies, pd)
			a.Logger.Info("plan-lifecycle path routed through durable delivery substrate",
				"node_id", nodeIdentity.ID, "node_type", string(nodeIdentity.Type), "is_planner", isPlanner)
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

	// Wire the authored-runtime cluster owner gate (epic memql#954, #961 / #959):
	// distribute owners across the live mesh so an owner's authored automations
	// fire on exactly one node cluster-wide rather than once per replica. The
	// membership view is self + the live peer set, read live so a membership
	// change rebalances without a restart. Single-node (peerMgr nil or empty
	// mesh) keeps the default where this node owns every owner.
	if a.authoredScheduler != nil && peerMgr != nil {
		ownerGate := automations.NewAuthoredOwnerGate(nodeIdentity.ID, func() []string {
			ids := []string{nodeIdentity.ID}
			for _, p := range peerMgr.AllPeers() {
				if p != nil && p.Info != nil && p.Info.NodeId != "" {
					ids = append(ids, p.Info.NodeId)
				}
			}
			return ids
		})
		a.authoredScheduler.SetOwnerGate(ownerGate.OwnsOwner)
		a.Logger.Info("authored runtime cluster owner gate wired", "self_node", nodeIdentity.ID)
	}

	if peerMgr != nil {
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

			// Agent-side client-tool RPC return channel (memql#1265): the agent
			// serves each in-flight turn's logical key so a ClientToolResult
			// cognition publishes there fires the local waiter over the durable
			// substrate, surviving cognition<->agent connection churn (the #1245
			// fix). Wired only on agent binaries where the substrate exists; the
			// firer resolves to the lazily-built grpc service.
			if nodeIdentity.Type == node.NodeTypeAgent && a.deliverySubstrate != nil && a.grpcServer != nil {
				if ctrs := node.NewClientToolResultServer(
					a.deliverySubstrate,
					a.grpcServer.ClientToolResultFirer(),
					nodeIdentity.ID,
					a.Logger,
				); ctrs != nil {
					a.grpcServer.SetClientToolResultServer(ctrs)
					a.Logger.Info("agent: client-tool RPC return channel served over delivery substrate",
						"node_id", nodeIdentity.ID)
				}
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

				// Cognition-side client-tool RPC return leg (memql#1265): replace
				// the churn-fragile ForwardContinuation with a substrate-RPC Call
				// addressed to the agent turn's logical key. Wired only on
				// cognition (the relay's host) where the substrate exists.
				if nodeIdentity.Type == node.NodeTypeCognition && a.deliverySubstrate != nil {
					if client := node.NewClientToolResultClient(a.deliverySubstrate, nodeIdentity.ID, a.Logger); client != nil {
						a.attachClientToolResultClientToCognition(client)
						a.Logger.Info("cognition: client-tool RPC return leg routed over delivery substrate",
							"node_id", nodeIdentity.ID)
					}
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

	// Capture this node's lifecycle state machine (Starting/Ready/Draining/
	// Stopped, memql#1268) and bridge it to the readiness surface: the moment
	// the node enters Draining (or the terminal Stopped) the readiness probe
	// (/healthz, /readyz) must flip to 503 so k8s/LB de-route it -- while
	// /livez stays 200 (readiness != liveness). The observer fires only on an
	// actual transition. The advertised gossip health rides the heartbeats
	// (peerConnection healthFn / server heartbeat), so peers route around a
	// Draining node at once rather than after a missed-heartbeat timeout.
	// node sits below component/server in the import graph, so the bridge is
	// wired here in app/ (same pattern as BeginDrain -> server.SetDraining).
	if peerMgr != nil {
		a.nodeLifecycle = peerMgr.Lifecycle()
		a.nodeLifecycle.SetObserver(func(_, newState node.LifecycleState) {
			if newState == node.LifecycleDraining || newState == node.LifecycleStopped {
				server.SetDraining(true)
			}
			a.Logger.Info("node lifecycle transition",
				"node_id", nodeIdentity.ID,
				"state", newState.String(),
			)
		})

		// Operator maintenance trigger (memql#1270): bring up the
		// operator-drain channel app.Run's wait path selects on, and wire
		// the owner/admin-gated gRPC NodeMaintenanceMsg handler to it via
		// the grpcServer. The handler resolves the caller's AccessContext,
		// rejects non-owner/admin, and calls RequestOperatorDrain on a
		// drain request -- funneling into the SAME #1269 drain sequence
		// SIGTERM uses. Only on mesh binaries with a lifecycle; non-mesh
		// builds have no maintenance entrypoint (the handler reports
		// unavailable). A node's NodeMaintenanceMsg targets the node that
		// receives it: operators address a specific node directly (its
		// gRPC address / a port-forward), matching how the ordered-rollout
		// driver drives one replica at a time.
		a.opDrain = make(chan struct{})
		if a.grpcServer != nil {
			a.grpcServer.SetNodeMaintenanceHandler(&appNodeMaintenanceHandler{app: a})
		}

		// Active topology reconciliation / fast reaper (epic memql#1871,
		// memql#1874). Every mesh replica starts the loop; a Postgres advisory
		// lock elects ONE cluster-wide leader that reconciles, so two replicas
		// never double-retire the same node. It drives topology freshness to
		// seconds by retiring (a) nodes whose deployment is superseded/failed/
		// rolled_back (supersededDeployments, immediate) and (b) nodes
		// continuously absent from the live mesh past a short grace window --
		// the crash/OOM/forced-replace case the gossip status writer misses and
		// the 30-min prune (pruneStaleClusterNodes) only mops up lazily.
		// The prune stays as the backstop; both write the same idempotent
		// terminal health="stopped", so they never conflict. Wired here in app/
		// because the reconciler needs the PeerManager (mesh view), the engine
		// (queries + the retire mutation), and the cluster DB (the advisory
		// lock) all resolved -- exactly the cluster-phase neighborhood the
		// WorkerDialer is wired in.
		if a.engine != nil {
			// memql#1930: the reconciler's leader-election advisory lock is
			// session-scoped and held for the loop's LIFETIME, so it rides the
			// DIRECT (non-pooled) endpoint -- a transaction-mode pooler would
			// recycle the backend out from under the held lock. Falls back to
			// the main pool when DIRECT_DSN is unset.
			a.Dependencies = append(a.Dependencies, node.NewTopologyReconciler(
				nodeIdentity, peerMgr, a.engine, a.directDBGetter(), a.Logger,
			))
		}
	}

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

// MarkNodeReady flips this node's lifecycle from Starting to Ready
// (memql#1268), advertised on the next gossip heartbeat and consulted by the
// readiness handler. Called from the Run lifecycle once every dependency has
// started (EmitSystemStartup), i.e. the node is actually able to serve.
// No-op on non-mesh binaries (no PeerManager / lifecycle). Idempotent.
func (a *App) MarkNodeReady() {
	if a.nodeLifecycle == nil {
		return
	}
	if err := a.nodeLifecycle.MarkReady(); err != nil {
		// A node already past Ready (e.g. a drain raced startup) is fine --
		// don't regress it. Log and move on.
		a.Logger.Warn("node lifecycle: could not mark ready", "error", err)
		return
	}
	a.Logger.Info("node marked ready", "node_id", a.nodeIdentity.ID)
}

// BeginNodeDrain flips this node's lifecycle to Draining (memql#1268) -- the
// mechanism a graceful SIGTERM drain (memql#1269) and the operator trigger
// (memql#1270) drive. The graceful-drain BEHAVIOUR (finish in-flight work
// bounded by a grace period, then MarkNodeStopped + the dependency Stop sweep)
// is orchestrated by app.Run (memql#1269); the operator-triggered drain is
// memql#1270. Flipping to Draining advertises DRAINING in gossip (peers route
// around at once) and flips readiness to 503 (LB/k8s de-route) via the #1268
// observer, while /livez stays 200. No-op on non-mesh binaries.
func (a *App) BeginNodeDrain() {
	if a.nodeLifecycle == nil {
		return
	}
	if err := a.nodeLifecycle.MarkDraining(); err != nil {
		a.Logger.Warn("node lifecycle: could not mark draining", "error", err)
	}
}

// MarkNodeStopped flips this node's lifecycle to the terminal Stopped state
// (memql#1268), advertised as STOPPED in gossip. Called by app.Run at the END
// of the graceful SIGTERM drain (memql#1269) -- after in-flight work has
// finished (or the grace deadline fired) and immediately before the dependency
// Stop sweep tears the transport down -- so the node leaves the mesh as cleanly
// Stopped rather than vanishing mid-Draining. Stopped is terminal and a no-op
// if already there; readiness already reports 503 from the Draining transition,
// so this does not change the readiness surface. No-op on non-mesh binaries.
func (a *App) MarkNodeStopped() {
	if a.nodeLifecycle == nil {
		return
	}
	if err := a.nodeLifecycle.MarkStopped(); err != nil {
		a.Logger.Warn("node lifecycle: could not mark stopped", "error", err)
	}
}

// RequestOperatorDrain is the on-demand operator entrypoint into the
// graceful drain (memql#1270, epic memql#1259 decision #4: "a
// maintenance/Draining state triggered two ways from ONE mechanism --
// automatically on deploy (SIGTERM) and on-demand by an operator
// command"). An owner/admin operator's gRPC NodeMaintenanceMsg lands in
// the handler, which calls this. It closes the opDrain channel exactly
// once; app.Run's wait path selects on that channel alongside the OS
// shutdown signal, so the operator trigger runs the IDENTICAL drain
// sequence #1269 runs on SIGTERM -- BeginNodeDrain (Draining advertised
// in gossip + readiness 503) -> drain delay -> in-flight wait bounded by
// MEMQL_SHUTDOWN_GRACE_PERIOD -> MarkNodeStopped -> dependency Stop sweep.
// There is no separate drain code path: the operator path is a second
// TRIGGER into the one mechanism.
//
// Idempotent: a second call (or a SIGTERM that races it) is a no-op
// because the underlying drain is already under way. Returns true if THIS
// call initiated the drain, false if it was already requested. A nil
// opDrain (non-mesh binary, or a node that didn't bring the channel up)
// returns false -- such a node has no operator drain entrypoint and the
// handler reports it as unavailable rather than pretending to drain.
func (a *App) RequestOperatorDrain(reason string) bool {
	if a == nil || a.opDrain == nil {
		return false
	}
	initiated := false
	a.opDrainOnce.Do(func() {
		if reason == "" {
			reason = "operator maintenance"
		}
		a.opDrainReason.Store(reason)
		a.Logger.Warn("operator maintenance drain requested",
			"reason", reason,
			"node_id", a.operatorNodeID(),
		)
		close(a.opDrain)
		initiated = true
	})
	return initiated
}

// OperatorDrainSignal returns the channel app.Run's wait path selects on
// (closed by RequestOperatorDrain). Nil on non-mesh binaries / nodes with
// no operator entrypoint, in which case Run waits on the OS signal only.
func (a *App) OperatorDrainSignal() <-chan struct{} {
	if a == nil {
		return nil
	}
	return a.opDrain
}

// OperatorDrainReason returns the reason recorded by the operator drain
// trigger (empty if none was set). Used for shutdown log context.
func (a *App) OperatorDrainReason() string {
	if a == nil {
		return ""
	}
	if v, ok := a.opDrainReason.Load().(string); ok {
		return v
	}
	return ""
}

// operatorNodeID is a nil-safe helper for log context.
func (a *App) operatorNodeID() string {
	if a == nil || a.nodeIdentity == nil {
		return ""
	}
	return a.nodeIdentity.ID
}

// appNodeMaintenanceHandler bridges the grpc package's
// NodeMaintenanceHandler port (memql#1270) to the App so the
// owner/admin-gated NodeMaintenanceMsg handler can drive THIS node's
// lifecycle. Lives in app/ (rather than grpc/) so the grpc layer stays
// free of any App / node-lifecycle dependency -- it depends on the narrow
// port shape, the wiring layer satisfies it. The handler's auth gate
// (owner/admin) is enforced in component/grpc before this is called.
type appNodeMaintenanceHandler struct {
	app *App
}

// NodeID reports the local node's id so the operator's reply / ordered-
// rollout driver can confirm which node it actually drained.
func (h *appNodeMaintenanceHandler) NodeID() string {
	if h == nil {
		return ""
	}
	return h.app.operatorNodeID()
}

// CurrentState reports the node's current lifecycle state (lowercase:
// starting/ready/draining/stopped) so the reply echoes it.
func (h *appNodeMaintenanceHandler) CurrentState() string {
	if h == nil || h.app == nil || h.app.nodeLifecycle == nil {
		return "unknown"
	}
	return h.app.nodeLifecycle.State().String()
}

// BeginDrain runs the operator-initiated drain by funneling into the
// SAME #1269 sequence SIGTERM uses (RequestOperatorDrain). Returns
// whether THIS call initiated it (false = drain already under way, which
// the handler reports as already-draining rather than an error).
func (h *appNodeMaintenanceHandler) BeginDrain(reason string) bool {
	if h == nil || h.app == nil {
		return false
	}
	return h.app.RequestOperatorDrain(reason)
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
	environment := firstNonEmptyStr(os.Getenv("MEMQL_ENVIRONMENT"), os.Getenv("MEMQL_REGION"), "development")
	payload["environment"] = environment
	payload["region"] = firstNonEmptyStr(os.Getenv("MEMQL_REGION"), "local")

	// Include the deploy provider / cloud target (#1872) so cluster
	// bootstrap can stamp it on the v1:cluster:cluster row. Explicit
	// MEMQL_DEPLOY_PROVIDER wins; otherwise derive from the environment
	// (local Docker for development, AKS/azure for staging + prod).
	payload["provider"] = deployProvider(environment)

	// Include the deployment id (#1873) so registerNode can stamp it
	// (plus provider/environment/region, already on this payload) on the
	// v1:cluster:node row -- tying every node to the deployment that created
	// it. MEMQL_DEPLOYMENT_ID is injected per-deploy by the rollout (k8s
	// downward API from the pod/rollout template hash). Falls back to
	// "local" in dev so the node row always carries a non-empty deploymentId
	// (a single logical local deployment), satisfying the #1873 acceptance
	// criterion and the orphan-detection contract (nodesNotInDeployment).
	payload["deploymentId"] = firstNonEmptyStr(os.Getenv("MEMQL_DEPLOYMENT_ID"), "local")

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
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
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
	baseURL := strings.TrimRight(os.Getenv("MEMQL_IDENTITY_VERIFIER_BASE_URL"), "/")
	if baseURL == "" {
		// Identity binaries set MEMQL_IDENTITY_BASE_URL instead (they don't
		// verify against a remote JWKS — they own the keys). Fall back
		// to that so the row populates correctly on identity nodes.
		baseURL = strings.TrimRight(os.Getenv("MEMQL_IDENTITY_BASE_URL"), "/")
	}
	if baseURL == "" {
		return nil
	}

	info := map[string]any{
		"name":         firstNonEmptyStr(os.Getenv("MEMQL_IDENTITY_BRAND_NAME"), "memQL Identity"),
		"providerType": "oidc",
		"issuerUrl":    firstNonEmptyStr(os.Getenv("MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER"), baseURL),
	}
	if aud := firstNonEmptyStr(os.Getenv("MEMQL_IDENTITY_VERIFIER_AUDIENCE"), os.Getenv("MEMQL_IDENTITY_JWT_AUDIENCE")); aud != "" {
		info["acceptedAudiences"] = []string{aud}
	}
	if jwksURL := firstNonEmptyStr(os.Getenv("MEMQL_IDENTITY_VERIFIER_JWKS_URL"), os.Getenv("MEMQL_IDENTITY_JWKS_URL")); jwksURL != "" {
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

// deployProvider resolves the deploy target / cloud provider stamped on
// the v1:cluster:cluster row and the system.startup payload (#1872).
// MEMQL_DEPLOY_PROVIDER is the explicit override; otherwise it derives
// from the environment: local Docker ("docker-local") for development,
// AKS ("azure") for staging + production.
func deployProvider(environment string) string {
	if p := strings.TrimSpace(os.Getenv("MEMQL_DEPLOY_PROVIDER")); p != "" {
		return p
	}
	if strings.EqualFold(strings.TrimSpace(environment), "development") {
		return "docker-local"
	}
	return "azure"
}

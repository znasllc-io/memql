// Package mcp implements the memQL MCP (Model Context Protocol) server --
// the protocol head that lets external MCP hosts (Claude Desktop / Claude
// Code and others) talk to a memQL deployment's tool surface.
//
// It is the engine side of a new `mcp` node role (epic memql#1529): a
// transport/protocol head over the engine's existing tool surface, selected
// by the `mcp` build tag, mirroring the other node roles. This Phase 0
// (memql#1530) stands up the protocol skeleton -- a node that builds, boots,
// connects to the engine, and serves an (initially empty) MCP surface. The
// reflected tool surface, the capability-tier / role gates, authoring, and
// resources/prompts land in later phases (#1531-#1536).
//
// The core Server here is transport-agnostic: it reads newline-delimited
// JSON-RPC 2.0 messages from an io.Reader and writes responses to an
// io.Writer. The stdio binding (the local Claude Desktop / Claude Code path)
// lives in stdio.go; an HTTP/SSE binding is a later, env-flagged option (the
// transport-order open item on the epic).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// ComponentName identifies the MCP server in logs.
const ComponentName = common.ComponentName("mcpServer")

// DefaultProtocolVersion is the MCP protocol revision this server advertises
// when a client does not request a specific one. The server echoes the
// client's requested version back when present (best-effort interop), and
// falls back to this otherwise.
const DefaultProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 standard error codes (subset used by the protocol head).
const (
	codeParseError    = -32700
	codeInvalidReq    = -32600
	codeMethodNotFnd  = -32601
	codeInvalidParams = -32602
)

// Server is the transport-agnostic MCP protocol head. It dispatches
// JSON-RPC messages against the engine's tool surface.
type Server struct {
	logger  *slog.Logger
	name    string
	version string

	// engine is the in-process memQL engine handle this head serves. It is
	// the "connection to the engine" Phase 0 stands up. Stored as any (Phase 0
	// decoupling); the Phase 1 tool surface adapts it to the narrow Engine
	// interface via asEngine().
	engine any

	// cfg carries the two MCP authz gates: the acting role (Gate B; #1531,
	// from MEMQL_MCP_ROLE -- empty -> IsAllowedForRole's "specialist" default)
	// and the capability tier (Gate A; #1532, from MEMQL_MCP_MODE).
	cfg Config

	// session is the per-connection, owner-scoped, NON-DURABLE authored-construct
	// registry (Phase 3 #1533). One MCP stdio connection is one session, so a
	// single registry per Server is the session boundary; it is dropped when the
	// process exits (the session GC). define registers into it; ExecuteAuthored
	// resolves from it; promote lifts a construct out of it into the durable
	// shared registries.
	session *memql.AuthoredRuntimeRegistry

	// autoRunner drives run_automation + @mcp-automation tools (Phase 4 #1534).
	// Injected from the app bootstrap (it needs the automations Loader + a manual
	// Executor + the dry-run sandbox). nil-safe: those tools report unavailable.
	autoRunner AutomationRunner

	writeMu sync.Mutex
	out     io.Writer // the active connection's output, for proactive notifications

	// subs holds the active resource subscriptions for this connection (Phase 6
	// #1536), keyed by resource uri -> the engine unsubscribe func. Torn down
	// when the connection closes.
	subMu sync.Mutex
	subs  map[string]func()
}

// SetAutomationRunner injects the automation runner the MCP node's app bootstrap
// builds (Phase 4 #1534). Wired before the server starts serving.
func (s *Server) SetAutomationRunner(r AutomationRunner) { s.autoRunner = r }

// NewServer constructs an MCP protocol head. name/version populate the
// MCP serverInfo block; engine is the in-process engine handle (may be nil in
// tests, where only the protocol surface is exercised); cfg carries the acting
// role + capability tier the session enforces.
func NewServer(logger *slog.Logger, name, version string, engine any, cfg Config) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{logger: logger, name: name, version: version, engine: engine, cfg: cfg, session: memql.NewAuthoredRuntimeRegistry()}
}

// Engine returns the in-process engine handle this head serves. Used by the
// reflected tool surface (#1531); nil-safe for callers.
func (s *Server) Engine() any { return s.engine }

// rpcRequest is an inbound JSON-RPC 2.0 message. A request carries an id; a
// notification omits it. id is kept as RawMessage so a present-but-null id
// and an absent id are distinguishable, and so it can be echoed back
// verbatim.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *rpcRequest) isNotification() bool {
	return len(bytes.TrimSpace(r.ID)) == 0 || string(bytes.TrimSpace(r.ID)) == "null"
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads newline-delimited JSON-RPC messages from in and writes
// responses to out until the context is cancelled or in reaches EOF.
// Notifications produce no response. A blocking read on in cannot be
// interrupted by ctx; cancellation takes effect on the next message
// boundary (the stdio dependency bounds teardown with the shutdown ctx).
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	// Make the connection's writer available to proactive notifications (the
	// resource-subscription pushes, Phase 6 #1536) and tear down any
	// subscriptions when the connection ends.
	s.writeMu.Lock()
	s.out = out
	s.writeMu.Unlock()
	defer s.closeSubscriptions()

	reader := bufio.NewReader(in)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := s.handleMessage(ctx, line); resp != nil {
				if err := s.writeMessage(out, resp); err != nil {
					return fmt.Errorf("mcp: write response: %w", err)
				}
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("mcp: read: %w", readErr)
		}
	}
}

// Dispatch parses and handles a single inbound JSON-RPC message and returns
// the marshaled response bytes. The bool is false for notifications and
// client->server responses (no reply is expected). It is the
// transport-agnostic entry point the HTTP head (http.go) uses; the stdio
// Serve loop calls handleMessage directly and frames the reply itself.
//
// One Server == one MCP session, so callers MUST serialise Dispatch per
// Server (the authoring registry + subscription map are not safe for
// concurrent mutation).
func (s *Server) Dispatch(ctx context.Context, raw []byte) ([]byte, bool) {
	resp := s.handleMessage(ctx, raw)
	if resp == nil {
		return nil, false
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		// A result that won't marshal is a server bug; surface a protocol
		// error envelope rather than dropping the reply on the floor.
		payload, _ = json.Marshal(errorResponse(resp.ID, codeInvalidReq, "response marshal error"))
	}
	return payload, true
}

// SetOutput registers the writer that proactive notifications (the resource
// subscription pushes, Phase 6 #1536) are written to. The stdio transport
// sets this implicitly in Serve; the HTTP transport sets it when a client
// opens the GET SSE channel and clears it (nil) when that channel closes.
func (s *Server) SetOutput(w io.Writer) {
	s.writeMu.Lock()
	s.out = w
	s.writeMu.Unlock()
}

// Close tears down any per-session state (active resource subscriptions).
// Idempotent. The HTTP transport calls this when a session is reaped or
// explicitly deleted; the stdio Serve loop tears subscriptions down on EOF.
func (s *Server) Close() { s.closeSubscriptions() }

// writeMessage encodes resp as a single line of JSON followed by a newline
// (the MCP stdio framing: one message per line, no embedded newlines).
func (s *Server) writeMessage(out io.Writer, resp *rpcResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = out.Write(payload)
	return err
}

// handleMessage parses and dispatches one raw JSON-RPC message, returning the
// response bytes to send (nil for notifications, which get no reply).
func (s *Server) handleMessage(ctx context.Context, raw []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, codeParseError, "parse error")
	}

	if req.isNotification() {
		// Notifications (e.g. notifications/initialized) are fire-and-forget.
		return nil
	}

	switch req.Method {
	case "initialize":
		return successResponse(req.ID, s.initializeResult(req.Params))
	case "tools/list":
		// Reflect the engine's DSL tools (role-gated) + the generic dispatchers
		// + @mcp-promoted query/mutation (Phase 4 #1534). @mcp-promoted
		// automations come from the runner (the engine does not own automations).
		tools := listMCPTools(asEngine(s.engine), s.cfg.ActingRole, s.cfg.Tier)
		if s.autoRunner != nil {
			tools = append(tools, s.autoRunner.PromotedAutomationTools()...)
		}
		return successResponse(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params)
	case "resources/list":
		// Phase 6 (#1536): concept schemas as MCP resources.
		return successResponse(req.ID, map[string]any{"resources": conceptResources(asResourceEngine(s.engine))})
	case "resources/read":
		return s.handleResourcesRead(ctx, req.ID, req.Params)
	case "resources/subscribe":
		return s.handleResourcesSubscribe(req.ID, req.Params)
	case "resources/unsubscribe":
		return s.handleResourcesUnsubscribe(req.ID, req.Params)
	case "prompts/list":
		// Phase 6 (#1536): DSL prompts as MCP prompts.
		return successResponse(req.ID, map[string]any{"prompts": promptDefinitions(asResourceEngine(s.engine))})
	case "prompts/get":
		return s.handlePromptsGet(ctx, req.ID, req.Params)
	case "ping":
		return successResponse(req.ID, map[string]any{})
	default:
		return errorResponse(req.ID, codeMethodNotFnd, "method not found: "+req.Method)
	}
}

// handleToolsCall dispatches an MCP tools/call. Tool-level failures are
// reported inside the result (isError:true) per the MCP convention; only
// malformed requests produce a JSON-RPC protocol error.
func (s *Server) handleToolsCall(ctx context.Context, id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return errorResponse(id, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if strings.TrimSpace(p.Name) == "" {
		return errorResponse(id, codeInvalidParams, "tools/call requires a tool name")
	}
	// Attach the per-connection authoring session (owner + non-durable registry)
	// so define/promote and authored call-by-name resolve against it, plus the
	// automation runner for run_automation + @mcp automations (Phase 4 #1534).
	ctx = withMCPSession(ctx, s.cfg.ActingUser, s.session)
	ctx = withMCPAutomationRunner(ctx, s.autoRunner)
	result := callMCPTool(ctx, asEngine(s.engine), s.cfg.ActingRole, s.cfg.Tier, p.Name, p.Arguments)
	return successResponse(id, result)
}

// initializeResult builds the MCP initialize result, echoing the client's
// requested protocolVersion when present.
func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	protocol := DefaultProtocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.ProtocolVersion != "" {
			protocol = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": protocol,
		// Phase 0 advertises the tools capability; the list is empty until
		// #1531. listChanged stays false until a phase wires change
		// notifications.
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
			// Phase 6 (#1536): concept resources (with live subscribe) + DSL prompts.
			"resources": map[string]any{"subscribe": true, "listChanged": false},
			"prompts":   map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    s.name,
			"version": s.version,
		},
	}
}

func successResponse(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: result}
}

func errorResponse(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Error: &rpcError{Code: code, Message: msg}}
}

// normalizeID returns a JSON null when no id was supplied, so the response
// always carries the required "id" field.
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(id)) == 0 {
		return json.RawMessage("null")
	}
	return id
}

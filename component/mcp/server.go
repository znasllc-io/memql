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

	writeMu sync.Mutex
}

// NewServer constructs an MCP protocol head. name/version populate the
// MCP serverInfo block; engine is the in-process engine handle (may be nil in
// tests, where only the protocol surface is exercised); cfg carries the acting
// role + capability tier the session enforces.
func NewServer(logger *slog.Logger, name, version string, engine any, cfg Config) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{logger: logger, name: name, version: version, engine: engine, cfg: cfg}
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
		// Phase 1 (#1531): reflect the engine's DSL tools (role-gated) + the
		// generic run_query / run_mutation / run_automation dispatchers.
		return successResponse(req.ID, map[string]any{"tools": listMCPTools(asEngine(s.engine), s.cfg.ActingRole, s.cfg.Tier)})
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params)
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

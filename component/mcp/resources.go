package mcp

// resources.go -- the MCP resources + prompts surface (epic memql#1529 Phase 6
// #1536, design §6.4). Concept schemas are exposed as resources (read runs a
// rows query); resources/subscribe rides the engine event bus and pushes
// notifications/resources/updated; DSL prompts map onto MCP prompts.

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/component/memql"
)

// ResourceEngine is the narrow engine surface the resources/prompts handlers
// need. *memql.MemQLEngine satisfies it; tests fake it. A nil engine yields an
// empty surface so the protocol head still answers.
type ResourceEngine interface {
	MCPConceptResources() []map[string]any
	MCPReadConceptResource(ctx context.Context, uri string) (string, error)
	MCPSubscribeConceptResource(uri string, onUpdate func()) (func(), error)
	MCPPromptDefinitions() []map[string]any
	MCPGetPrompt(name string, args map[string]any) (string, error)
}

// asResourceEngine adapts the engine handle to ResourceEngine, returning nil
// when the handle does not provide the method set (the protocol-only tests).
func asResourceEngine(engine any) ResourceEngine {
	if engine == nil {
		return nil
	}
	if r, ok := engine.(ResourceEngine); ok {
		return r
	}
	return nil
}

func conceptResources(eng ResourceEngine) []map[string]any {
	if eng == nil {
		return []map[string]any{}
	}
	if r := eng.MCPConceptResources(); r != nil {
		return r
	}
	return []map[string]any{}
}

func promptDefinitions(eng ResourceEngine) []map[string]any {
	if eng == nil {
		return []map[string]any{}
	}
	if p := eng.MCPPromptDefinitions(); p != nil {
		return p
	}
	return []map[string]any{}
}

// uriParam extracts the "uri" string from a params blob.
func uriParam(params json.RawMessage) string {
	var p struct {
		URI string `json:"uri"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	return p.URI
}

// handleResourcesRead reads a concept resource: it runs the rows query the uri
// names and returns the rows as a JSON text content block.
func (s *Server) handleResourcesRead(ctx context.Context, id json.RawMessage, params json.RawMessage) *rpcResponse {
	eng := asResourceEngine(s.engine)
	if eng == nil {
		return errorResponse(id, codeInvalidParams, "resources are unavailable: engine not connected")
	}
	uri := uriParam(params)
	if uri == "" {
		return errorResponse(id, codeInvalidParams, "resources/read requires a 'uri'")
	}
	text, err := eng.MCPReadConceptResource(ctx, uri)
	if err != nil {
		return errorResponse(id, codeInvalidParams, "resources/read failed: "+err.Error())
	}
	return successResponse(id, map[string]any{
		"contents": []map[string]any{
			{"uri": uri, "mimeType": "application/json", "text": text},
		},
	})
}

// handleResourcesSubscribe registers a live subscription for a concept resource:
// every create/update/delete of a row of that concept pushes a
// notifications/resources/updated for the uri. Re-subscribing replaces the prior
// subscription for the same uri.
func (s *Server) handleResourcesSubscribe(id json.RawMessage, params json.RawMessage) *rpcResponse {
	eng := asResourceEngine(s.engine)
	if eng == nil {
		return errorResponse(id, codeInvalidParams, "resource subscriptions are unavailable: engine not connected")
	}
	uri := uriParam(params)
	if uri == "" {
		return errorResponse(id, codeInvalidParams, "resources/subscribe requires a 'uri'")
	}
	unsub, err := eng.MCPSubscribeConceptResource(uri, func() {
		s.writeNotification("notifications/resources/updated", map[string]any{"uri": uri})
	})
	if err != nil {
		return errorResponse(id, codeInvalidParams, "resources/subscribe failed: "+err.Error())
	}
	s.subMu.Lock()
	if s.subs == nil {
		s.subs = make(map[string]func())
	}
	if prev, ok := s.subs[uri]; ok && prev != nil {
		prev()
	}
	s.subs[uri] = unsub
	s.subMu.Unlock()
	return successResponse(id, map[string]any{})
}

// handleResourcesUnsubscribe tears down a resource subscription.
func (s *Server) handleResourcesUnsubscribe(id json.RawMessage, params json.RawMessage) *rpcResponse {
	uri := uriParam(params)
	if uri == "" {
		return errorResponse(id, codeInvalidParams, "resources/unsubscribe requires a 'uri'")
	}
	s.subMu.Lock()
	unsub, ok := s.subs[uri]
	delete(s.subs, uri)
	s.subMu.Unlock()
	if ok && unsub != nil {
		unsub()
	}
	return successResponse(id, map[string]any{})
}

// handlePromptsGet renders a DSL prompt with the given arguments and returns it
// as a single user message (the MCP prompts/get shape).
func (s *Server) handlePromptsGet(ctx context.Context, id json.RawMessage, params json.RawMessage) *rpcResponse {
	eng := asResourceEngine(s.engine)
	if eng == nil {
		return errorResponse(id, codeInvalidParams, "prompts are unavailable: engine not connected")
	}
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return errorResponse(id, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if p.Name == "" {
		return errorResponse(id, codeInvalidParams, "prompts/get requires a 'name'")
	}
	text, err := eng.MCPGetPrompt(p.Name, p.Arguments)
	if err != nil {
		return errorResponse(id, codeInvalidParams, "prompts/get failed: "+err.Error())
	}
	return successResponse(id, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": map[string]any{"type": "text", "text": text}},
		},
	})
}

// writeNotification writes a JSON-RPC notification (no id) to the active
// connection. Safe to call from a subscription goroutine -- it serialises on
// writeMu with the response writer.
func (s *Server) writeNotification(method string, params any) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.out == nil {
		return
	}
	_, _ = s.out.Write(payload)
}

// closeSubscriptions tears down every active resource subscription. Called when
// the connection ends.
func (s *Server) closeSubscriptions() {
	s.subMu.Lock()
	subs := s.subs
	s.subs = nil
	s.subMu.Unlock()
	for _, unsub := range subs {
		if unsub != nil {
			unsub()
		}
	}
}

// ensure the concrete engine satisfies ResourceEngine at compile time.
var _ ResourceEngine = (*memql.MemQLEngine)(nil)

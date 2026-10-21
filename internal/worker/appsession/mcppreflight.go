package appsession

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// preflightMCP checks the authenticated session's entire tool contract before
// starting a paid app turn. The server validates the delegated tool set; an
// unavailable tool is an error, never a silently shortened menu.
func preflightMCP(ctx context.Context, client *http.Client, endpoint, credential string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if client == nil {
		client = http.DefaultClient
	}
	session := ""
	request := func(id int, method string, params any) (json.RawMessage, error) {
		message := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		if id != 0 {
			message["id"] = id
		}
		body, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("MCP endpoint is invalid")
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("MCP readiness request failed: %w", err)
		}
		defer resp.Body.Close()
		if id == 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, nil
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("MCP readiness returned HTTP %d", resp.StatusCode)
		}
		if value := resp.Header.Get("Mcp-Session-Id"); value != "" {
			session = value
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, fmt.Errorf("MCP readiness response failed: %w", err)
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			scanner := bufio.NewScanner(bytes.NewReader(data))
			scanner.Buffer(nil, 4<<20)
			data = nil
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "data: ") {
					data = []byte(strings.TrimPrefix(scanner.Text(), "data: "))
					break
				}
			}
		}
		var reply struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &reply) != nil {
			return nil, fmt.Errorf("MCP readiness returned an invalid response")
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("MCP readiness: %s", reply.Error.Message)
		}
		if len(reply.Result) == 0 {
			return nil, fmt.Errorf("MCP readiness returned no result")
		}
		return reply.Result, nil
	}
	defer func() {
		if session == "" {
			return
		}
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer closeCancel()
		req, err := http.NewRequestWithContext(closeCtx, http.MethodDelete, endpoint, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Mcp-Session-Id", session)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	if _, err := request(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "memql-cockpit-preflight", "version": "1"}}); err != nil {
		return nil, err
	}
	if _, err := request(0, "notifications/initialized", map[string]any{}); err != nil {
		return nil, err
	}
	raw, err := request(2, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil, fmt.Errorf("MCP readiness returned an invalid tool list")
	}
	seen := map[string]bool{}
	names := []string{}
	for _, tool := range list.Tools {
		if tool.Name == "" || seen[tool.Name] {
			return nil, fmt.Errorf("MCP readiness returned an invalid tool name")
		}
		seen[tool.Name] = true
		names = append(names, tool.Name)
	}
	if !seen["next_task"] || !seen["submit"] {
		return nil, fmt.Errorf("MCP readiness: endpoint does not expose this app session's protocol")
	}
	sort.Strings(names)
	return names, nil
}

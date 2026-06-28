package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// CapabilityDispatcher invokes a single external capability (the authored-
// action backend, memql#2218). The capability name selects the namespace
// (shell.* / fs.* / http.* / integration.* / mcp.*); args is the rendered
// argTemplate. It returns the capability's result (decoded to a Go value) or
// an error. A dispatcher runs on a SINGLE default surface for now -- surface
// resolution lands with the surface-aware trust ladder later.
type CapabilityDispatcher interface {
	Invoke(ctx context.Context, capability string, args map[string]any) (any, error)
}

// engineToolInvoker is the slice of the MemQL engine the default dispatcher
// needs for integration.* / mcp.* capabilities (they resolve to engine tools).
type engineToolInvoker interface {
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
}

// defaultDispatcher dispatches authored-action capabilities by namespace.
// fs.* / http.* run directly on the local surface; integration.* / mcp.*
// resolve to engine tools via ExecuteToolByName. It is deterministic given
// deterministic inputs + backend, which is what makes an authored action
// replay token-free and fingerprint-verifiable.
//
// shell.* runs a DETERMINISTIC capability SCRIPT invoked with structured params
// (the merged capability-script contract, #2221: `--name=value` argv flags in,
// one JSON envelope out), NOT an arbitrary rendered command string run through
// `sh -c` -- that would be a command-injection sink on author/event-derived
// params. The capability-script runner (capability_script.go, I8/#2222)
// launches an ALLOWLISTED script via an argv slice and parses its envelope
// through deploycontrol.ParseCapabilityResult.
type defaultDispatcher struct {
	engine engineToolInvoker
	script *capabilityScriptRunner
}

// newDefaultDispatcher builds the default capability dispatcher bound to the
// engine (for integration.* / mcp.*) with the capability-script runner wired in
// for shell.* (I8, #2222).
func newDefaultDispatcher(engine engineToolInvoker) *defaultDispatcher {
	return &defaultDispatcher{engine: engine, script: newCapabilityScriptRunner()}
}

func (d *defaultDispatcher) Invoke(ctx context.Context, capability string, args map[string]any) (any, error) {
	switch {
	case capability == "fs.readFile":
		return d.fsReadFile(args)
	case capability == "fs.writeFile":
		return d.fsWriteFile(args)
	case capability == "http.get":
		return d.httpRequest(ctx, http.MethodGet, args)
	case capability == "http.post":
		return d.httpRequest(ctx, http.MethodPost, args)
	case strings.HasPrefix(capability, "integration."), strings.HasPrefix(capability, "mcp."):
		return d.engineTool(ctx, capability, args)
	case strings.HasPrefix(capability, "shell."):
		// The capability-script runner (I8, #2222): a shell capability names an
		// allowlisted capability script via its `script` arg and is invoked with
		// structured params as argv flags, never a rendered command string.
		if d.script == nil {
			d.script = newCapabilityScriptRunner()
		}
		return d.script.Invoke(ctx, args)
	default:
		return nil, fmt.Errorf("capability %q is not supported by the default dispatcher (supported: fs.readFile, fs.writeFile, http.get, http.post, shell.* via the capability-script runner, integration.*, mcp.*)", capability)
	}
}

func (d *defaultDispatcher) fsReadFile(args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("fs.readFile requires a \"path\" argument")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fs.readFile %q: %w", path, err)
	}
	return map[string]any{"path": path, "content": string(b)}, nil
}

func (d *defaultDispatcher) fsWriteFile(args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("fs.writeFile requires a \"path\" argument")
	}
	content, _ := args["content"].(string)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("fs.writeFile %q: %w", path, err)
	}
	return map[string]any{"path": path, "bytesWritten": len(content)}, nil
}

func (d *defaultDispatcher) httpRequest(ctx context.Context, method string, args map[string]any) (any, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("%s requires a \"url\" argument", strings.ToLower(method))
	}
	var body io.Reader
	if b, ok := args["body"].(string); ok && b != "" {
		body = strings.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http %s %q: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return map[string]any{"status": resp.StatusCode, "body": decodeMaybeJSON(string(respBody))}, nil
}

func (d *defaultDispatcher) engineTool(ctx context.Context, capability string, args map[string]any) (any, error) {
	if d.engine == nil {
		return nil, fmt.Errorf("capability %q requires the MemQL engine, which is not configured", capability)
	}
	out, err := d.engine.ExecuteToolByName(ctx, capability, args)
	if err != nil {
		return nil, fmt.Errorf("capability %q: %w", capability, err)
	}
	return decodeMaybeJSON(out), nil
}

// decodeMaybeJSON returns the parsed JSON value when s is valid JSON, else the
// raw string. Keeps a structured result envelope structured (so its
// fingerprint is stable) without forcing every capability to emit JSON.
func decodeMaybeJSON(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	if t[0] != '{' && t[0] != '[' {
		return s
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return s
	}
	return v
}

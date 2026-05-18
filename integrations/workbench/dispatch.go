package workbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Default limits. Configurable per-call via args when the LLM has a
// reason; otherwise the defaults apply. Hard caps keep a runaway
// agent from eating the agent node.
const (
	defaultExecTimeout = 60 * time.Second
	maxExecTimeout     = 600 * time.Second
	maxExecOutputBytes = 1 << 20 // 1 MiB cap per exec; stdout + stderr each
	maxFSReadBytes     = 1 << 20 // 1 MiB cap per fs_read
	maxFSWriteBytes    = 1 << 24 // 16 MiB cap per fs_write
	maxFSListEntries   = 1000
	defaultHTTPTimeout = 30 * time.Second
	maxHTTPTimeout     = 120 * time.Second
	maxHTTPRespBytes   = 5 << 20 // 5 MiB cap on http_fetch response
)

// dispatchResult is the wire shape returned by every handler.
// Marshalled to JSON for the tool loop.
type dispatchResult struct {
	OK        bool   `json:"ok"`
	Action    string `json:"action"`
	Payload   any    `json:"payload,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
	ErrorMsg  string `json:"errorMessage,omitempty"`
}

// handleExec runs a shell command via /bin/sh -c inside the
// workspace root. Captures stdout + stderr separately up to the
// output cap. Honors a per-call timeout in [1s, maxExecTimeout].
func (i *Integration) handleExec(ctx context.Context, ws *workspace, args map[string]any) dispatchResult {
	cmd, _ := args["cmd"].(string)
	if strings.TrimSpace(cmd) == "" {
		return errResult("exec", "missing_arg", "exec requires `cmd`")
	}
	cwdArg, _ := args["cwd"].(string)
	cwd, err := ws.safeJoin(cwdArg)
	if err != nil {
		return errResult("exec", "invalid_path", err.Error())
	}
	timeout := pickTimeout(args["timeoutSec"], defaultExecTimeout, maxExecTimeout)
	envExtra := stringMap(args["env"])
	stdin, _ := args["stdin"].(string)

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, "/bin/sh", "-c", cmd)
	c.Dir = cwd
	if len(envExtra.pairs) > 0 {
		c.Env = append(os.Environ(), envExtra.pairs...)
	}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	stdout := &cappedBuffer{cap: maxExecOutputBytes}
	stderr := &cappedBuffer{cap: maxExecOutputBytes}
	c.Stdout = stdout
	c.Stderr = stderr

	runErr := c.Run()
	exitCode := 0
	signal := ""
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			if pstate := ee.ProcessState; pstate != nil {
				// On Unix, exited-by-signal: ExitCode is -1.
				if sys, ok := pstate.Sys().(interface{ Signal() any }); ok {
					if s := sys.Signal(); s != nil {
						signal = fmt.Sprintf("%v", s)
					}
				}
			}
		} else {
			return errResult("exec", "exec_failed", runErr.Error())
		}
	}
	if cctx.Err() == context.DeadlineExceeded {
		return errResult("exec", "timeout", fmt.Sprintf("exec exceeded %s timeout", timeout))
	}
	return dispatchResult{
		OK:     exitCode == 0,
		Action: "exec",
		Payload: map[string]any{
			"exitCode":      exitCode,
			"signal":        signal,
			"stdout":        stdout.String(),
			"stdoutTrunc":   stdout.truncated,
			"stderr":        stderr.String(),
			"stderrTrunc":   stderr.truncated,
			"durationMs":    0,
			"workspaceRoot": ws.rootPath,
			"cwd":           cwd,
		},
	}
}

// handleFSRead reads up to maxBytes from a file inside the workspace.
// Returns content as a string -- the LLM expects text; binary files
// are not first-class on this surface.
func (i *Integration) handleFSRead(_ context.Context, ws *workspace, args map[string]any) dispatchResult {
	pathArg, _ := args["path"].(string)
	if strings.TrimSpace(pathArg) == "" {
		return errResult("fs_read", "missing_arg", "fs_read requires `path`")
	}
	abs, err := ws.safeJoin(pathArg)
	if err != nil {
		return errResult("fs_read", "invalid_path", err.Error())
	}
	cap := intArg(args["maxBytes"], maxFSReadBytes)
	if cap > maxFSReadBytes {
		cap = maxFSReadBytes
	}
	f, err := os.Open(abs)
	if err != nil {
		return errResult("fs_read", "fs_read_failed", err.Error())
	}
	defer f.Close()
	buf := make([]byte, cap+1)
	n, err := io.ReadFull(f, buf)
	truncated := false
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return errResult("fs_read", "fs_read_failed", err.Error())
	}
	if n > cap {
		truncated = true
		n = cap
	}
	return dispatchResult{
		OK:     true,
		Action: "fs_read",
		Payload: map[string]any{
			"path":      pathArg,
			"content":   string(buf[:n]),
			"bytes":     n,
			"truncated": truncated,
		},
	}
}

// handleFSWrite writes content to a file inside the workspace.
// Parent directories are auto-created. content is taken as-is (string)
// up to the write cap.
func (i *Integration) handleFSWrite(_ context.Context, ws *workspace, args map[string]any) dispatchResult {
	pathArg, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if strings.TrimSpace(pathArg) == "" {
		return errResult("fs_write", "missing_arg", "fs_write requires `path`")
	}
	if len(content) > maxFSWriteBytes {
		return errResult("fs_write", "too_large", fmt.Sprintf("content exceeds %d bytes", maxFSWriteBytes))
	}
	abs, err := ws.safeJoin(pathArg)
	if err != nil {
		return errResult("fs_write", "invalid_path", err.Error())
	}
	mode := os.FileMode(0o644)
	if m, ok := args["mode"].(string); ok && m != "" {
		// Accept "0644" / "0755" / etc; ignore on parse failure.
		var parsed uint32
		if _, perr := fmt.Sscanf(m, "%o", &parsed); perr == nil {
			mode = os.FileMode(parsed)
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return errResult("fs_write", "fs_write_failed", err.Error())
	}
	if err := os.WriteFile(abs, []byte(content), mode); err != nil {
		return errResult("fs_write", "fs_write_failed", err.Error())
	}
	return dispatchResult{
		OK:     true,
		Action: "fs_write",
		Payload: map[string]any{
			"path":  pathArg,
			"bytes": len(content),
			"mode":  fmt.Sprintf("%o", mode),
		},
	}
}

// handleFSList lists entries in a directory inside the workspace,
// non-recursive by default. Each entry: { name, isDir, size }.
func (i *Integration) handleFSList(_ context.Context, ws *workspace, args map[string]any) dispatchResult {
	pathArg, _ := args["path"].(string)
	abs, err := ws.safeJoin(pathArg)
	if err != nil {
		return errResult("fs_list", "invalid_path", err.Error())
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return errResult("fs_list", "fs_list_failed", err.Error())
	}
	out := make([]map[string]any, 0, len(entries))
	truncated := false
	for _, e := range entries {
		if len(out) >= maxFSListEntries {
			truncated = true
			break
		}
		info, ierr := e.Info()
		size := int64(0)
		if ierr == nil {
			size = info.Size()
		}
		out = append(out, map[string]any{
			"name":  e.Name(),
			"isDir": e.IsDir(),
			"size":  size,
		})
	}
	return dispatchResult{
		OK:     true,
		Action: "fs_list",
		Payload: map[string]any{
			"path":      pathArg,
			"entries":   out,
			"count":     len(out),
			"truncated": truncated,
		},
	}
}

// handleFSStat returns size / mode / mtime / isDir / exists for a
// path inside the workspace. Non-existent paths return exists=false
// (not an error -- the agent often probes for file presence).
func (i *Integration) handleFSStat(_ context.Context, ws *workspace, args map[string]any) dispatchResult {
	pathArg, _ := args["path"].(string)
	abs, err := ws.safeJoin(pathArg)
	if err != nil {
		return errResult("fs_stat", "invalid_path", err.Error())
	}
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return dispatchResult{
			OK:     true,
			Action: "fs_stat",
			Payload: map[string]any{
				"path":   pathArg,
				"exists": false,
			},
		}
	}
	if err != nil {
		return errResult("fs_stat", "fs_stat_failed", err.Error())
	}
	return dispatchResult{
		OK:     true,
		Action: "fs_stat",
		Payload: map[string]any{
			"path":      pathArg,
			"exists":    true,
			"isDir":     info.IsDir(),
			"size":      info.Size(),
			"mode":      fmt.Sprintf("%o", info.Mode().Perm()),
			"modTime":   info.ModTime().UTC().Format(time.RFC3339),
		},
	}
}

// handleHTTPFetch performs an HTTP request. Body capped at
// maxHTTPRespBytes. Default method GET; explicit method allowed.
// Headers passed through. The workbench is the HTTP client -- the
// user's machine is not involved.
func (i *Integration) handleHTTPFetch(ctx context.Context, _ *workspace, args map[string]any) dispatchResult {
	url, _ := args["url"].(string)
	if strings.TrimSpace(url) == "" {
		return errResult("http_fetch", "missing_arg", "http_fetch requires `url`")
	}
	method := strings.ToUpper(strings.TrimSpace(stringArg(args["method"], "GET")))
	if method == "" {
		method = "GET"
	}
	body, _ := args["body"].(string)
	timeout := pickTimeout(args["timeoutSec"], defaultHTTPTimeout, maxHTTPTimeout)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, url, reqBody)
	if err != nil {
		return errResult("http_fetch", "invalid_request", err.Error())
	}
	for k, v := range stringMap(args["headers"]).asMap() {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errResult("http_fetch", "request_failed", err.Error())
	}
	defer resp.Body.Close()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxHTTPRespBytes+1))
	if readErr != nil {
		return errResult("http_fetch", "read_failed", readErr.Error())
	}
	truncated := false
	if len(bodyBytes) > maxHTTPRespBytes {
		bodyBytes = bodyBytes[:maxHTTPRespBytes]
		truncated = true
	}
	headers := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	return dispatchResult{
		OK:     resp.StatusCode < 400,
		Action: "http_fetch",
		Payload: map[string]any{
			"url":        url,
			"method":     method,
			"status":     resp.StatusCode,
			"headers":    headers,
			"body":       string(bodyBytes),
			"bytes":      len(bodyBytes),
			"truncated":  truncated,
		},
	}
}

// ----- helpers -------------------------------------------------------------

func errResult(action, code, msg string) dispatchResult {
	return dispatchResult{OK: false, Action: action, ErrorCode: code, ErrorMsg: msg}
}

// cappedBuffer is a bytes.Buffer that drops writes beyond `cap` and
// flips `truncated` to true. Used to bound exec stdout / stderr.
type cappedBuffer struct {
	bytes.Buffer
	cap       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.cap - c.Buffer.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.Buffer.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// pickTimeout pulls a timeoutSec arg out of the LLM args and clamps
// it to [1s, max]. Falls back to def when arg is absent or invalid.
func pickTimeout(arg any, def, max time.Duration) time.Duration {
	if arg == nil {
		return def
	}
	var secs float64
	switch v := arg.(type) {
	case float64:
		secs = v
	case int:
		secs = float64(v)
	case int64:
		secs = float64(v)
	default:
		return def
	}
	if secs < 1 {
		return def
	}
	d := time.Duration(secs) * time.Second
	if d > max {
		return max
	}
	return d
}

func intArg(arg any, def int) int {
	switch v := arg.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return def
	}
}

func stringArg(arg any, def string) string {
	if s, ok := arg.(string); ok && s != "" {
		return s
	}
	return def
}

// envMap wraps the env arg into a key=value slice suitable for
// exec.Cmd.Env. Order doesn't matter; duplicates are last-wins.
type envMap struct {
	pairs []string
	raw   map[string]string
}

func (e envMap) asMap() map[string]string { return e.raw }

// stringMap reads either a map[string]any or map[string]string out
// of an args field and returns an envMap whose pairs satisfy
// exec.Cmd.Env. Used for both env vars and HTTP headers.
func stringMap(arg any) envMap {
	out := envMap{raw: map[string]string{}}
	switch m := arg.(type) {
	case map[string]any:
		for k, v := range m {
			if s, ok := v.(string); ok {
				out.pairs = append(out.pairs, k+"="+s)
				out.raw[k] = s
			}
		}
	case map[string]string:
		for k, s := range m {
			out.pairs = append(out.pairs, k+"="+s)
			out.raw[k] = s
		}
	}
	return out
}


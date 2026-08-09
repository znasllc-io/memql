package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// capability_script.go is the I8 (#2222) capability-script RUNNER: the safe
// `shell.*` backend for the authored-action dispatcher. It is the seam where a
// deploy `action` (one `shell.script` capability) actually touches the world by
// invoking a NAMED I14 capability script (#2221) -- NOT an arbitrary rendered
// command string.
//
// INJECTION-SAFE BY CONSTRUCTION. I6 deliberately removed the raw
// `/bin/sh -c <string>` sink (CodeQL command-injection). This runner never
// reintroduces it:
//
//  1. No shell. The script is executed DIRECTLY via its shebang
//     (`exec.CommandContext(ctx, <allowlisted-path>, flags...)`) -- the command
//     is the constant allowlisted path, never the `bash` interpreter and never
//     `sh -c`. There is no shell word-splitting and no string interpolation of
//     any author/event value into a command line.
//  2. Allowlisted target. The action names a capability-script *id*
//     (e.g. "deploy.cloneRepo"); the runner maps that id to a repo-relative
//     path through the STATIC capabilityScriptAllowlist below. An author- or
//     event-derived `script` value can only select an allowlisted I14 script,
//     never an arbitrary path or binary. The resolved path is additionally
//     confined to the script root.
//  3. Params as argv flags, never a command line. Every other rendered arg is
//     passed as a single `--name=value` ELEMENT of the argv slice (the
//     contract's highest-precedence param channel, scripts/lib/capability.sh:
//     cap_parse_flags). Because each flag is its own argv element and there is
//     no shell, a value containing shell metacharacters (`;`, `$(...)`,
//     backticks) is inert -- it is the literal string the script's cap_param
//     returns, never something a shell re-parses. Non-string params (objects,
//     arrays, numbers, bools) are JSON-encoded into the flag value so the
//     script can decode them.
//
// The script writes its human logs to stderr and exactly one JSON result
// envelope to stdout; the runner parses that envelope through the canonical
// deploycontrol.ParseCapabilityResult (the Go side of the I14 contract) instead
// of scraping prose. A deterministic script + deterministic params yields a
// deterministic result, which is what lets an authored deploy action replay
// token-free and fingerprint-verifiably.

// capabilityScriptAllowlist maps a capability-script id (the `script` arg of a
// `shell.script` action) to its repo-relative path under scripts/. THIS IS THE
// SECURITY BOUNDARY: a shell action can only run a script listed here. Adding a
// new deploy capability is a deliberate edit to this map plus a contract-
// conformant script -- never an arbitrary path supplied at run time.
var capabilityScriptAllowlist = map[string]string{
	// Local k3d cluster lifecycle (I14 reference implementations).
	"k3d.up":          "scripts/k3d/up.sh",
	"k3d.down":        "scripts/k3d/down.sh",
	"k3d.dev":         "scripts/k3d/dev.sh",
	"k3d.bringup":     "scripts/k3d/bringup.sh",
	"k3d.scale":       "scripts/k3d/scale.sh",
	"k3d.status":      "scripts/k3d/status.sh",
	"k3d.seedSecrets": "scripts/k3d/seed-secrets.sh",
	"k3d.importImage": "scripts/k3d/import-image.sh",

	// Deploy-pack capability backends (I8, #2222).
	"deploy.cloneRepo":   "scripts/deploy/clone-repo.sh",
	"deploy.buildImage":  "scripts/deploy/build-image.sh",
	"deploy.gate":        "scripts/deploy/deploy-gate.sh",
	"deploy.notify":      "scripts/deploy/notify.sh",
	"overlay.pinDigests": "scripts/deploy/pin-overlay-digests.sh",
	"overlay.revert":     "scripts/deploy/revert-overlay.sh",
	"argocd.sync":        "scripts/deploy/argo-sync.sh",

	// Local-cluster install/uninstall substrate (epic #3357, install(11) /
	// #3368). Registration here is what makes an install script REACHABLE from
	// the in-engine action path -- an unregistered script still runs fine from
	// a human shell or the host executor, but the runner rejects its id before
	// exec, so the capability is silently inert on the engine path while
	// looking healthy everywhere else. install_allowlist_test.go walks
	// scripts/install/ and fails on any unregistered cap_init id, so adding an
	// install capability is a two-file change by construction.
	"install.refreshPins":       "scripts/install/refresh-tool-pins.sh",
	"install.detect":            "scripts/install/detect.sh",
	"install.binary":            "scripts/install/install-binary.sh",
	"install.hostsEntries":      "scripts/install/hosts-entries.sh",
	"install.mkcert":            "scripts/install/mkcert-setup.sh",
	"install.cloneStack":        "scripts/install/clone-stack.sh",
	"install.verifyProviderKey": "scripts/install/verify-provider-key.sh",
	"install.verifyFrontDoor":   "scripts/install/verify-frontdoor.sh",
	"install.magicLink":         "scripts/install/magic-link.sh",
	"install.removeArtifact":    "scripts/install/remove-artifact.sh",
}

// scriptParamKey is the reserved rendered-arg key that names the capability
// script. Every other rendered arg is a structured param for that script.
const scriptParamKey = "script"

// capabilityScriptRunner runs an allowlisted I14 capability script with
// structured params and parses its JSON result envelope. It is the backend for
// shell.* capabilities in the default dispatcher.
type capabilityScriptRunner struct {
	// root is the repository root containing scripts/. When empty it is
	// resolved lazily (MEMQL_DEPLOY_REPO_ROOT, then by walking up from the
	// working directory for scripts/lib/capability.sh). Tests inject it.
	root string

	// allow maps capability-script id -> repo-relative path. When nil the
	// process allowlist (capabilityScriptAllowlist) is used. Tests inject it.
	allow map[string]string

	rootOnce sync.Once
	rootVal  string
	rootErr  error
}

// newCapabilityScriptRunner builds a runner over the process allowlist; root is
// resolved lazily on first Invoke.
func newCapabilityScriptRunner() *capabilityScriptRunner {
	return &capabilityScriptRunner{}
}

func (r *capabilityScriptRunner) allowlist() map[string]string {
	if r.allow != nil {
		return r.allow
	}
	return capabilityScriptAllowlist
}

// resolveRoot finds the repository root that contains scripts/. Precedence:
// an explicitly injected root > MEMQL_DEPLOY_REPO_ROOT > the nearest
// ancestor of the working directory that holds scripts/lib/capability.sh.
func (r *capabilityScriptRunner) resolveRoot() (string, error) {
	r.rootOnce.Do(func() {
		if r.root != "" {
			r.rootVal = r.root
			return
		}
		if env := strings.TrimSpace(os.Getenv("MEMQL_DEPLOY_REPO_ROOT")); env != "" {
			r.rootVal = env
			return
		}
		wd, err := os.Getwd()
		if err != nil {
			r.rootErr = fmt.Errorf("capability-script runner: cannot determine working directory: %w", err)
			return
		}
		dir := wd
		for {
			if fileExists(filepath.Join(dir, "scripts", "lib", "capability.sh")) {
				r.rootVal = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		r.rootErr = fmt.Errorf("capability-script runner: could not locate the repository root (no scripts/lib/capability.sh above %q); set MEMQL_DEPLOY_REPO_ROOT", wd)
	})
	return r.rootVal, r.rootErr
}

// scriptPath resolves an allowlisted capability-script id to an absolute path
// confined to the script root. An unknown id or a path that escapes the root is
// rejected.
func (r *capabilityScriptRunner) scriptPath(id string) (string, error) {
	rel, ok := r.allowlist()[id]
	if !ok {
		return "", fmt.Errorf("capability-script %q is not in the allowlist -- a shell action may only run a registered capability script (component/automations/steps.capabilityScriptAllowlist)", id)
	}
	root, err := r.resolveRoot()
	if err != nil {
		return "", err
	}
	abs := filepath.Clean(filepath.Join(root, rel))
	// Defense in depth: confirm the resolved path stays under the root even
	// though rel comes from our own static map.
	rootClean := filepath.Clean(root)
	if abs != rootClean && !strings.HasPrefix(abs, rootClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("capability-script %q resolves outside the script root", id)
	}
	if !fileExists(abs) {
		return "", fmt.Errorf("capability-script %q backend not found at %s", id, abs)
	}
	return abs, nil
}

// Invoke runs the capability script named by args["script"], passing every
// other rendered arg as a `--name=value` argv flag, and returns the parsed
// result envelope. The capability id (shell.script / shell.exec ...) is
// informational here -- the `script` arg selects the backend.
func (r *capabilityScriptRunner) Invoke(ctx context.Context, args map[string]any) (any, error) {
	idVal, ok := args[scriptParamKey]
	if !ok {
		return nil, fmt.Errorf("shell capability requires a %q argument naming the capability script to run (the action's argTemplate must render %q)", scriptParamKey, scriptParamKey)
	}
	id, ok := idVal.(string)
	if !ok || strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("shell capability %q argument must be a non-empty string, got %T", scriptParamKey, idVal)
	}

	path, err := r.scriptPath(id)
	if err != nil {
		return nil, err
	}

	// Every arg except `script` becomes a single `--name=value` argv flag
	// (never interpolated into a command line). Keys are sorted so the argv --
	// and therefore the result fingerprint -- is deterministic for a given
	// input. Non-string values are JSON-encoded so the script can decode them.
	keys := make([]string, 0, len(args))
	for k := range args {
		if k == scriptParamKey {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	flags := make([]string, 0, len(keys))
	for _, k := range keys {
		ev, eerr := encodeFlagValue(args[k])
		if eerr != nil {
			return nil, fmt.Errorf("capability-script %q: encode param %q: %w", id, k, eerr)
		}
		flags = append(flags, "--"+k+"="+ev)
	}

	stdout, stderr, runErr := runCapabilityScript(ctx, path, flags)

	// The script emits its result envelope on stdout even on failure (the
	// contract's EXIT trap guarantees it). Parse stdout first; only when there
	// is genuinely no envelope do we surface the raw exec error + stderr.
	res, perr := deploycontrol.ParseCapabilityResult(stdout)
	if perr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("capability-script %q (%s) failed: %v\nstderr:\n%s", id, path, runErr, truncateForError(stderr))
		}
		return nil, fmt.Errorf("capability-script %q (%s): %v\nstderr:\n%s", id, path, perr, truncateForError(stderr))
	}
	if cerr := res.Err(); cerr != nil {
		return nil, cerr
	}

	return map[string]any{
		"ok":         res.OK,
		"changed":    res.Changed,
		"capability": res.Capability,
		"result":     decodeRawJSON(res.Result),
	}, nil
}

// encodeFlagValue renders a rendered-arg value as a capability-script flag
// value. Strings pass through verbatim (cap_param returns them literally);
// every other type is JSON-encoded so the script can decode structured params.
func encodeFlagValue(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// runCapabilityScript executes an allowlisted capability script DIRECTLY via
// its shebang -- `exec.CommandContext(ctx, path, flags...)`, where path is the
// constant allowlisted backend (never the `bash` interpreter) and each flag is
// its own argv element -- with stdin closed, and returns its stdout (the result
// envelope) and stderr (human logs) separately. The command is the script
// path, not a shell, so the tainted flag values are inert argv data: there is
// no `sh -c`, no word-splitting, and no metacharacter expansion. The exec error
// (e.g. a non-zero exit) is returned for context; the caller prefers the parsed
// envelope over the raw error.
func runCapabilityScript(ctx context.Context, path string, flags []string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, path, flags...)
	cmd.Stdin = nil // closed stdin: a conformant capability script never blocks
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// fileExists reports whether path names an existing (non-directory) file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// decodeRawJSON decodes a result's raw JSON into a Go value, or nil when empty.
func decodeRawJSON(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// truncateForError caps stderr in error messages so a chatty script does not
// bury the failure.
func truncateForError(b []byte) string {
	const max = 4 << 10
	s := strings.TrimRight(string(b), "\n")
	if len(s) > max {
		return s[:max] + "\n... (truncated)"
	}
	return s
}

# Local Cluster Install Substrate (Epic 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Linux, no-UI substrate that installs a local memQL cluster from nothing and completely reverses it — capability scripts, a validated step graph, a host executor, and a receipt.

**Architecture:** Every mechanical action is an I14 capability script (non-interactive, `--flag=value` in, one JSON envelope out, honest exit codes). A declarative step graph names those scripts, their dependencies, and a **verify predicate** per step. A host executor walks the graph, dispatches scripts, and appends an install receipt; uninstall walks the receipt backwards. Nothing decides anything outside the graph.

**Tech Stack:** Bash (capability scripts), Go 1.26.1 (graph validation + script behaviour tests), TypeScript/Node 20 (host executor, `node --test`), MemQL DSL (`dsl/install/`).

**Source spec:** `docs/superpowers/specs/2026-08-08-local-cluster-install-wizard-design.md`

## Global Constraints

- **Linux/amd64 only** in this epic. macOS is Phase 3; Windows is refused with a clear message.
- **Every script under `scripts/install/` sources `scripts/lib/capability.sh`** and therefore is auto-discovered by `scripts/lib/capability_contract_test.go`. That gate enforces: `cap_init` present, non-interactive (no `read -p` / `select`), valid bash, `--print-spec` emits a descriptor whose `capability` matches the `cap_init` id, no blocking on closed stdin, and **exit 2 + a failure envelope on any undeclared flag**.
- **`cap_param` has no environment tier.** Precedence is `--flag=value` > stdin JSON (opt-in) > default. Pass an env-resolved value *as the default*: `cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}"`.
- **Exit codes are load-bearing** and drive the wizard's error rendering: `0` ok, `2` bad param, `3` refused, `4` prerequisite missing, `5` operation failed.
- **Every script is idempotent**, calls `cap_changed` only when it actually mutated something, and calls `cap_ok` on every success path.
- **No `curl | bash` installs.** Binaries are downloaded to a temp path, checksum-verified against `scripts/install/tool-pins.env`, then moved into place.
- **Stage vs Phase:** "Stage" = a stage of one install run; "Phase" = a delivery phase. This plan is Epic 1 only.
- Commit messages end with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- Stage files by explicit path. Never `git add -A` or `git add .`.

---

## Implementation Decision: where the graph lives

The spec (§4.2) says the step graph is authored in `dsl/install/` and compiled to JSON. Implementing that literally would require new MemQL syntax for per-step `dependsOn`, `verify`, and `elevation` metadata — the existing `action` construct is deliberately *one capability call with typed args* and carries no DAG semantics (`dsl/deployment/actions.memql` header: "ACTIONS TOUCH THE WORLD, automations orchestrate").

**This plan authors the graph as two declarative JSON documents** (`scripts/install/graph/install.json`, `uninstall.json`) validated by Go tests, and keeps `dsl/install/actions.memql` as the in-engine callable form for Phase 5.

To prevent the two drifting into two sources of truth, **Task 14 adds a test asserting they agree**: every graph step's `script` id has a matching DSL action, and every DSL action appears in a graph. Inventing DSL syntax is deferred to Phase 5, where the in-engine executor actually needs it.

---

## File Structure

| Path | Responsibility |
|---|---|
| `scripts/install/tool-pins.env` | Pinned tool versions + sha256 digests (generated, committed) |
| `scripts/install/refresh-tool-pins.sh` | Regenerates the pins file from upstream releases |
| `scripts/install/detect.sh` | `install.detect` — OS + dependency inventory (read-only) |
| `scripts/install/install-binary.sh` | `install.binary` — k3d/kubectl/mkcert into `~/.memql/bin` |
| `scripts/install/hosts-entries.sh` | `install.hostsEntries` — add/remove `/etc/hosts` lines |
| `scripts/install/mkcert-setup.sh` | `install.mkcert` — CA trust + wildcard cert issuance |
| `scripts/install/clone-stack.sh` | `install.cloneStack` — clone memQL at a tag |
| `scripts/install/verify-provider-key.sh` | `install.verifyProviderKey` — one live AI provider call |
| `scripts/install/verify-frontdoor.sh` | `install.verifyFrontDoor` — DNS + TLS + gRPC reachability |
| `scripts/install/magic-link.sh` | `install.magicLink` — owner sign-in link from identity logs |
| `scripts/install/remove-artifact.sh` | `install.removeArtifact` — every uninstall reversal |
| `scripts/install/graph/install.json` | The install step graph |
| `scripts/install/graph/uninstall.json` | The uninstall step graph |
| `scripts/install/graph/graph.go` | Graph schema types + loader (package `graph`) |
| `scripts/install/graph/graph_test.go` | Structural validation: cycles, verify, allowlist, reversal completeness |
| `scripts/install/*_test.go` | Per-script behaviour tests (package `install`) |
| `component/automations/steps/capability_script.go` | Allowlist registration (modify) |
| `dsl/install/concepts.memql` | `installRun` / `installStep` records for Phase 5 replay |
| `dsl/install/actions.memql` | In-engine callable form of each step |
| `editors/vscode/src/install/graph.ts` | Graph loading + topological ordering (vscode-free) |
| `editors/vscode/src/install/runner.ts` | Capability-script dispatch + envelope parsing |
| `editors/vscode/src/install/receipt.ts` | Receipt append/read/reverse |
| `editors/vscode/src/install/executor.ts` | Walks the graph, drives runner + receipt |
| `editors/vscode/src/install/cli.ts` | Bare CLI harness (`node dist/install/cli.js`) |
| `editors/vscode/test/install*.test.ts` | Executor/receipt/graph unit tests |
| `.github/workflows/install-e2e.yml` | Clean-runner install → uninstall → baseline diff |

`src/install/` must stay **vscode-free** so it runs under bare `node --test` and, in Epic 2, behind a webview. Import nothing from `vscode` in that directory.

---

## Task 1: Scaffold `scripts/install/` and pin the tool downloads

**Files:**
- Create: `scripts/install/refresh-tool-pins.sh`
- Create: `scripts/install/tool-pins.env`
- Create: `scripts/install/pins_test.go`

**Interfaces:**
- Consumes: `scripts/lib/capability.sh` (`cap_init`, `cap_spec_param`, `cap_parse_flags`, `cap_param`, `cap_ok`, `cap_fail`, `cap_result_set`, `cap_changed`)
- Produces: `tool-pins.env` defining `K3D_VERSION`, `K3D_SHA256`, `KUBECTL_VERSION`, `KUBECTL_SHA256`, `MKCERT_VERSION`, `MKCERT_SHA256` — consumed by Task 3.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/pins_test.go`:

```go
// Package install holds behaviour tests for the local-install capability
// scripts. The contract-level gate lives in scripts/lib; these tests cover
// what each script actually DOES.
package install

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot resolves the repository root from scripts/install -> ../..
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// TestToolPinsAreCompleteAndWellFormed asserts the committed pins file names a
// version AND a 64-hex sha256 for every tool the installer downloads. A pin
// without a digest is worse than no pin: it looks deliberate while verifying
// nothing.
func TestToolPinsAreCompleteAndWellFormed(t *testing.T) {
	pins := filepath.Join(repoRoot(t), "scripts", "install", "tool-pins.env")
	f, err := os.Open(pins)
	if err != nil {
		t.Fatalf("open tool-pins.env: %v", err)
	}
	defer f.Close()

	got := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed line (want KEY=VALUE): %q", line)
		}
		got[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	reSHA := regexp.MustCompile(`^[0-9a-f]{64}$`)
	reVer := regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
	for _, tool := range []string{"K3D", "KUBECTL", "MKCERT"} {
		ver := got[tool+"_VERSION"]
		sha := got[tool+"_SHA256"]
		if !reVer.MatchString(ver) {
			t.Errorf("%s_VERSION = %q, want a semver like v1.2.3", tool, ver)
		}
		if !reSHA.MatchString(sha) {
			t.Errorf("%s_SHA256 = %q, want 64 lowercase hex chars", tool, sha)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestToolPinsAreCompleteAndWellFormed -v`
Expected: FAIL — `open tool-pins.env: no such file or directory`

- [ ] **Step 3: Write the pins generator**

Create `scripts/install/refresh-tool-pins.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/refresh-tool-pins.sh
# ====================================
#
# Capability: install.refreshToolPins -- regenerate scripts/install/tool-pins.env
# by downloading each pinned tool release and recording its sha256.
#
# WHY GENERATED, NOT HAND-WRITTEN. A digest cannot be known without fetching the
# artifact, and a hand-copied digest is a digest nobody verified. This script IS
# the procedure: run it, review the diff, commit the result. install-binary.sh
# then verifies every download against the committed digest and NEVER fetches a
# checksum at install time (a checksum fetched alongside the artifact proves
# nothing about the artifact).
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/refresh-tool-pins.sh
#   scripts/install/refresh-tool-pins.sh --k3d-version=v5.7.4
#   scripts/install/refresh-tool-pins.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (curl/sha256sum) |
#             5 a download or digest computation failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.refreshToolPins" "Regenerate the pinned tool versions + sha256 digests."
cap_spec_param "k3d-version"     "k3d release tag to pin"
cap_spec_param "kubectl-version" "kubectl release tag to pin"
cap_spec_param "mkcert-version"  "mkcert release tag to pin"
cap_spec_param "out"             "path of the pins file to write"

function check_prerequisites() {
    command -v curl      &>/dev/null || cap_fail 4 "curl is required to fetch release artifacts"
    command -v sha256sum &>/dev/null || cap_fail 4 "sha256sum is required to compute digests"
}

# digest_of <url> -> sha256 of the artifact at <url>, or fails 5.
function digest_of() {
    local url="$1" tmp
    tmp="$(mktemp)"
    cap_info "fetching ${url}"
    if ! curl -fsSL --retry 3 -o "$tmp" "$url"; then
        rm -f "$tmp"
        cap_fail 5 "download failed: ${url}"
    fi
    sha256sum "$tmp" | cut -d' ' -f1
    rm -f "$tmp"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    check_prerequisites

    local k3d_ver kubectl_ver mkcert_ver out
    k3d_ver="$(cap_param k3d-version "v5.7.4")"
    kubectl_ver="$(cap_param kubectl-version "v1.31.0")"
    mkcert_ver="$(cap_param mkcert-version "v1.4.4")"
    out="$(cap_param out "${SCRIPT_DIR}/tool-pins.env")"

    local k3d_sha kubectl_sha mkcert_sha
    k3d_sha="$(digest_of "https://github.com/k3d-io/k3d/releases/download/${k3d_ver}/k3d-linux-amd64")"
    kubectl_sha="$(digest_of "https://dl.k8s.io/release/${kubectl_ver}/bin/linux/amd64/kubectl")"
    mkcert_sha="$(digest_of "https://github.com/FiloSottile/mkcert/releases/download/${mkcert_ver}/mkcert-${mkcert_ver}-linux-amd64")"

    cat > "$out" <<EOF
# scripts/install/tool-pins.env -- GENERATED by refresh-tool-pins.sh. Do not
# hand-edit: a digest nobody fetched is a digest nobody verified.
#
# install-binary.sh verifies every download against these values and never
# fetches a checksum at install time.
K3D_VERSION="${k3d_ver}"
K3D_SHA256="${k3d_sha}"
KUBECTL_VERSION="${kubectl_ver}"
KUBECTL_SHA256="${kubectl_sha}"
MKCERT_VERSION="${mkcert_ver}"
MKCERT_SHA256="${mkcert_sha}"
EOF

    cap_changed
    cap_info "wrote ${out}"
    cap_result_set     out         "$out"
    cap_result_set     k3dVersion     "$k3d_ver"
    cap_result_set     kubectlVersion "$kubectl_ver"
    cap_result_set     mkcertVersion  "$mkcert_ver"
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Generate the pins file**

Run:
```bash
chmod +x scripts/install/refresh-tool-pins.sh
scripts/install/refresh-tool-pins.sh
cat scripts/install/tool-pins.env
```
Expected: a `tool-pins.env` with three versions and three 64-hex digests. Review the digests against the upstream release pages before committing.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./scripts/install/ -v && go test ./scripts/lib/ -run TestCapabilityScripts -v`
Expected: PASS. The `scripts/lib` run proves the new script satisfies the contract gate (spec descriptor, non-interactivity, unknown-flag rejection).

- [ ] **Step 6: Commit**

```bash
git add scripts/install/refresh-tool-pins.sh scripts/install/tool-pins.env scripts/install/pins_test.go
git commit -m "$(cat <<'EOF'
feat(install): pin installer tool downloads to verified digests

refresh-tool-pins.sh generates tool-pins.env by fetching each release and
recording its sha256. install-binary.sh will verify against the committed
digest and never fetch a checksum at install time -- a checksum fetched
alongside its artifact proves nothing about the artifact.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `install.detect` — the dependency inventory

**Files:**
- Create: `scripts/install/detect.sh`
- Create: `scripts/install/detect_test.go`

**Interfaces:**
- Consumes: `scripts/lib/capability.sh`
- Produces: capability id `install.detect`. Result object:
  `{"os":"linux"|"darwin"|"unsupported", "arch":"amd64"|..., "supported":bool, "tools":{"docker":{"present":bool,"version":string,"daemon":bool}, "k3d":{...}, "kubectl":{...}, "git":{...}, "mkcert":{...}}, "ports":{"80":bool,"443":bool}, "diskFreeMb":number}`.
  `present` means the binary resolves; `daemon` is docker-only and means `docker info` succeeded. Ports map to **true when free**.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/detect_test.go`:

```go
package install

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// envelope mirrors the capability result envelope (the I14 contract schema).
type envelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// runScript executes a capability script with stdin closed and returns its
// parsed envelope plus the raw combined output for diagnostics.
func runScript(t *testing.T, script string, args ...string) (envelope, string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Stdin = nil
	out, _ := cmd.CombinedOutput()
	var env envelope
	line := ""
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no JSON envelope in output:\n%s", out)
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\nline: %s", err, line)
	}
	return env, string(out)
}

type detectResult struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Supported bool   `json:"supported"`
	Tools     map[string]struct {
		Present bool   `json:"present"`
		Version string `json:"version"`
		Daemon  bool   `json:"daemon"`
	} `json:"tools"`
	Ports      map[string]bool `json:"ports"`
	DiskFreeMb int             `json:"diskFreeMb"`
}

// TestDetectIsReadOnlyAndReportsEveryTool asserts detect never mutates
// (changed=false) and reports a present flag for every dependency the graph
// depends on. A missing key would read as "absent" downstream and silently
// schedule an install of something already there.
func TestDetectIsReadOnlyAndReportsEveryTool(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/detect.sh"
	env, out := runScript(t, script)

	if !env.OK {
		t.Fatalf("detect failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	if env.Changed {
		t.Errorf("detect reported changed=true; detection must be read-only")
	}
	var r detectResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the detect schema: %v\nresult: %s", err, env.Result)
	}
	for _, tool := range []string{"docker", "k3d", "kubectl", "git", "mkcert"} {
		if _, ok := r.Tools[tool]; !ok {
			t.Errorf("tools[%q] missing -- an absent key reads as 'not installed' downstream", tool)
		}
	}
	if r.OS != runtime.GOOS {
		t.Errorf("os = %q, want %q", r.OS, runtime.GOOS)
	}
	if runtime.GOOS == "linux" && !r.Supported {
		t.Errorf("supported = false on linux; linux is the Epic 1 target platform")
	}
	for _, p := range []string{"80", "443"} {
		if _, ok := r.Ports[p]; !ok {
			t.Errorf("ports[%q] missing", p)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestDetectIsReadOnly -v`
Expected: FAIL — `no JSON envelope in output` (the script does not exist).

- [ ] **Step 3: Write the script**

Create `scripts/install/detect.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/detect.sh
# =========================
#
# Capability: install.detect -- inventory the machine for a local memQL install.
#
# READ-ONLY BY CONSTRUCTION. This script never installs, writes, or elevates;
# it answers "what is here?" so the graph can decide what to do. It always
# succeeds (exit 0) on a supported platform even when everything is missing --
# "docker is absent" is an ANSWER, not a failure. Only an unsupported platform
# is a refusal, and it is exit 3 (refused) rather than 4, because no
# prerequisite the user could install would change it.
#
# `present` means the binary resolves on PATH. `daemon` is docker-only and
# means `docker info` succeeded -- Docker Desktop installed but not running is
# the single most common false-positive in this whole flow, so it is a
# SEPARATE field rather than folded into `present`.
#
# `ports` reports TRUE WHEN FREE. Phrasing it as availability rather than
# occupancy means the graph reads `ports."443" == true` as "we can bind", with
# no double negative at the call site.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/detect.sh
#   scripts/install/detect.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 3 refused (unsupported OS/arch)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.detect" "Inventory OS, dependencies, ports and disk for a local memQL install."
cap_spec_param "workdir" "directory whose free space is reported (default \$HOME)"

#=============================================================================
# PLATFORM
#=============================================================================

function detect_os() {
    case "$(uname -s)" in
        Linux)  printf 'linux'  ;;
        Darwin) printf 'darwin' ;;
        *)      printf 'unsupported' ;;
    esac
}

function detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) printf 'amd64' ;;
        arm64|aarch64) printf 'arm64' ;;
        *) printf 'unsupported' ;;
    esac
}

#=============================================================================
# TOOL PROBES -- each emits a JSON object; none of them mutate anything
#=============================================================================

# version_of <binary> -- best-effort single-line version string.
function version_of() {
    local bin="$1" v=""
    v="$("$bin" --version 2>/dev/null | head -1 || true)"
    printf '%s' "$v"
}

# tool_json <name> -- {"present":bool,"version":"...","daemon":bool}
# daemon is meaningful for docker only; it is false everywhere else.
function tool_json() {
    local name="$1" present=false version="" daemon=false
    if command -v "$name" &>/dev/null; then
        present=true
        version="$(version_of "$name")"
        if [[ "$name" == "docker" ]] && docker info &>/dev/null; then
            daemon=true
        fi
    fi
    printf '{"present":%s,"version":"%s","daemon":%s}' \
        "$present" "$(cap_json_escape "$version")" "$daemon"
}

#=============================================================================
# PORTS + DISK
#=============================================================================

# port_free <port> -- true when nothing is listening. Tries ss, then lsof; when
# neither exists we report FREE rather than guessing occupied, because a false
# "occupied" halts the install on a machine that is actually fine, while a
# false "free" surfaces later as an honest bind error from k3d.
function port_free() {
    local port="$1"
    if command -v ss &>/dev/null; then
        if ss -ltn "sport = :${port}" 2>/dev/null | tail -n +2 | grep -q .; then
            printf 'false'; return
        fi
        printf 'true'; return
    fi
    if command -v lsof &>/dev/null; then
        if lsof -iTCP:"${port}" -sTCP:LISTEN &>/dev/null; then
            printf 'false'; return
        fi
        printf 'true'; return
    fi
    printf 'true'
}

# disk_free_mb <dir> -- free megabytes on the filesystem holding <dir>.
function disk_free_mb() {
    local dir="$1"
    df -Pm "$dir" 2>/dev/null | awk 'NR==2 {print $4}' || printf '0'
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local workdir os arch supported
    workdir="$(cap_param workdir "$HOME")"
    os="$(detect_os)"
    arch="$(detect_arch)"

    supported=false
    if [[ "$os" == "linux" && "$arch" == "amd64" ]]; then
        supported=true
    fi

    if [[ "$os" == "unsupported" ]]; then
        cap_fail 3 "unsupported operating system '$(uname -s)': memQL local install supports Linux (and macOS from Phase 3). Windows is not supported."
    fi

    local tools ports free
    tools="{\"docker\":$(tool_json docker),\"k3d\":$(tool_json k3d),\"kubectl\":$(tool_json kubectl),\"git\":$(tool_json git),\"mkcert\":$(tool_json mkcert)}"
    ports="{\"80\":$(port_free 80),\"443\":$(port_free 443)}"
    free="$(disk_free_mb "$workdir")"
    [[ -n "$free" ]] || free=0

    cap_info "os=${os} arch=${arch} supported=${supported} diskFreeMb=${free}"

    cap_result_set     os          "$os"
    cap_result_set     arch        "$arch"
    cap_result_set_raw supported   "$supported"
    cap_result_set_raw tools       "$tools"
    cap_result_set_raw ports       "$ports"
    cap_result_set_raw diskFreeMb  "$free"
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/detect.sh
go test ./scripts/install/ -run TestDetect -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/detect.sh scripts/install/detect_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.detect dependency inventory

Read-only probe of OS, arch, docker/k3d/kubectl/git/mkcert, ports 80/443
and free disk. Reports docker's daemon state as a SEPARATE field from
presence -- Docker installed but not running is the most common
false-positive in the install flow. Ports report true when FREE so the
graph reads them without a double negative.

An unsupported OS exits 3 (refused), not 4: no prerequisite the user
could install would change the answer.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `install.binary` — verified tool installation

**Files:**
- Create: `scripts/install/install-binary.sh`
- Create: `scripts/install/install_binary_test.go`

**Interfaces:**
- Consumes: `scripts/install/tool-pins.env` (Task 1)
- Produces: capability id `install.binary`. Params: `--tool=k3d|kubectl|mkcert`, `--dest` (default `$HOME/.memql/bin`), `--dry-run`. Result: `{"tool":string,"path":string,"version":string,"installed":bool,"preExisting":bool}`.
  `preExisting` is **true when the tool already resolved on PATH outside `dest`** — the receipt uses it to decide whether uninstall may remove it.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/install_binary_test.go`:

```go
package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type binaryResult struct {
	Tool        string `json:"tool"`
	Path        string `json:"path"`
	Version     string `json:"version"`
	Installed   bool   `json:"installed"`
	PreExisting bool   `json:"preExisting"`
}

// TestInstallBinaryDryRunTouchesNothing is the test that makes this script safe
// to exercise in CI: --dry-run must report exactly what it WOULD do and leave
// the destination empty. Without it there is no way to test the install path
// without network access and a mutated machine.
func TestInstallBinaryDryRunTouchesNothing(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/install-binary.sh"
	dest := t.TempDir()

	env, out := runScript(t, script, "--tool=k3d", "--dest="+dest, "--dry-run")
	if !env.OK {
		t.Fatalf("dry run failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	if env.Changed {
		t.Errorf("dry run reported changed=true; it must not mutate")
	}
	var r binaryResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the binary schema: %v", err)
	}
	if r.Installed {
		t.Errorf("dry run reported installed=true")
	}
	if r.Path != filepath.Join(dest, "k3d") {
		t.Errorf("path = %q, want %q", r.Path, filepath.Join(dest, "k3d"))
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d entries into dest; want 0", len(entries))
	}
}

// TestInstallBinaryRejectsUnknownTool locks the discriminator. An unknown tool
// must be exit 2 (bad param) rather than a confusing download failure, because
// the value is author-supplied and a typo is the likely cause.
func TestInstallBinaryRejectsUnknownTool(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/install-binary.sh"
	env, out := runScript(t, script, "--tool=kubectlx", "--dest="+t.TempDir())
	if env.OK {
		t.Fatalf("unknown tool accepted; output:\n%s", out)
	}
	if env.Error == nil || env.Error.Code != 2 {
		t.Errorf("want error.code 2 for an unknown tool; got %+v", env.Error)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestInstallBinary -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/install-binary.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/install-binary.sh
# =================================
#
# Capability: install.binary -- install a pinned tool binary into ~/.memql/bin.
#
# WHY NOT `curl | bash`. Every upstream ships a convenience installer that pipes
# a fetched script straight into a shell. Those installers are unpinned, run
# arbitrary code as the invoking user, and frequently write outside the
# directory we are prepared to reverse. This script instead downloads ONE
# artifact at a pinned version, verifies it against the digest committed in
# tool-pins.env, and moves it into a directory the receipt can remove wholesale.
#
# WHY THE DIGEST IS COMMITTED, NOT FETCHED. A checksum downloaded next to the
# artifact is signed by whoever served the artifact -- it detects corruption,
# not substitution. The pin is reviewed in a diff by a human once, which is the
# only point at which the value means anything.
#
# preExisting IS LOAD-BEARING. When the tool already resolves on PATH from
# somewhere other than <dest>, we record that and install nothing. The receipt
# carries the flag so uninstall leaves the user's own k3d alone -- removing a
# tool we did not install is the single worst thing an uninstaller can do.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/install-binary.sh --tool=k3d
#   scripts/install/install-binary.sh --tool=kubectl --dest=/tmp/bin --dry-run
#   scripts/install/install-binary.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param (unknown tool) | 4 prerequisite missing
#             (curl/sha256sum) | 5 download or digest mismatch

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.binary" "Install a pinned, digest-verified tool binary into the memQL bin directory."
cap_spec_param "tool"    "which tool to install: k3d | kubectl | mkcert"
cap_spec_param "dest"    "destination directory for the binary"
cap_spec_param "pins"    "path to the pinned versions/digests file"
cap_spec_param "dry-run" "report what would happen without downloading (flag)"

function check_prerequisites() {
    command -v curl      &>/dev/null || cap_fail 4 "curl is required to download tool binaries"
    command -v sha256sum &>/dev/null || cap_fail 4 "sha256sum is required to verify tool binaries"
}

# url_for <tool> <version> -- the single release artifact for linux/amd64.
function url_for() {
    local tool="$1" ver="$2"
    case "$tool" in
        k3d)     printf 'https://github.com/k3d-io/k3d/releases/download/%s/k3d-linux-amd64' "$ver" ;;
        kubectl) printf 'https://dl.k8s.io/release/%s/bin/linux/amd64/kubectl' "$ver" ;;
        mkcert)  printf 'https://github.com/FiloSottile/mkcert/releases/download/%s/mkcert-%s-linux-amd64' "$ver" "$ver" ;;
    esac
}

# pre_existing <tool> <dest> -- "true" when the tool resolves on PATH from
# somewhere OTHER than <dest>. Resolving from <dest> means a previous run of
# this script put it there, which is ours to manage.
function pre_existing() {
    local tool="$1" dest="$2" found
    found="$(command -v "$tool" 2>/dev/null || true)"
    if [[ -z "$found" ]]; then printf 'false'; return; fi
    if [[ "$found" == "${dest}/${tool}" ]]; then printf 'false'; return; fi
    printf 'true'
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local tool dest pins dry
    tool="$(cap_param tool "")"
    dest="$(cap_param dest "${HOME}/.memql/bin")"
    pins="$(cap_param pins "${SCRIPT_DIR}/tool-pins.env")"
    dry="$(cap_flag dry-run)"
    cap_require tool "$tool"

    case "$tool" in
        k3d|kubectl|mkcert) ;;
        *) cap_fail 2 "unknown tool '${tool}': expected one of k3d, kubectl, mkcert" ;;
    esac

    [[ -f "$pins" ]] || cap_fail 4 "pins file not found at ${pins}; run scripts/install/refresh-tool-pins.sh"
    # shellcheck disable=SC1090
    source "$pins"

    local upper ver sha
    upper="$(printf '%s' "$tool" | tr '[:lower:]' '[:upper:]')"
    ver="$(eval "printf '%s' \"\${${upper}_VERSION:-}\"")"
    sha="$(eval "printf '%s' \"\${${upper}_SHA256:-}\"")"
    [[ -n "$ver" && -n "$sha" ]] || cap_fail 4 "no pin for ${tool} in ${pins}; re-run refresh-tool-pins.sh"

    local target url existing
    target="${dest}/${tool}"
    url="$(url_for "$tool" "$ver")"
    existing="$(pre_existing "$tool" "$dest")"

    if [[ -n "$dry" ]]; then
        cap_info "DRY RUN: would download ${url} and install to ${target}"
        cap_result_set     tool        "$tool"
        cap_result_set     path        "$target"
        cap_result_set     version     "$ver"
        cap_result_set_raw installed   false
        cap_result_set_raw preExisting "$existing"
        cap_ok
    fi

    # Idempotent: an already-installed binary at the pinned digest is a no-op.
    if [[ -x "$target" ]] && [[ "$(sha256sum "$target" | cut -d' ' -f1)" == "$sha" ]]; then
        cap_info "${tool} ${ver} already installed at ${target} (digest matches)"
        cap_result_set     tool        "$tool"
        cap_result_set     path        "$target"
        cap_result_set     version     "$ver"
        cap_result_set_raw installed   false
        cap_result_set_raw preExisting "$existing"
        cap_ok
    fi

    check_prerequisites
    mkdir -p "$dest"

    local tmp got
    tmp="$(mktemp)"
    cap_info "downloading ${tool} ${ver}"
    if ! curl -fsSL --retry 3 -o "$tmp" "$url"; then
        rm -f "$tmp"
        cap_fail 5 "download failed: ${url}"
    fi
    got="$(sha256sum "$tmp" | cut -d' ' -f1)"
    if [[ "$got" != "$sha" ]]; then
        rm -f "$tmp"
        cap_fail 5 "digest mismatch for ${tool} ${ver}: got ${got}, pinned ${sha}. Refusing to install."
    fi
    chmod 0755 "$tmp"
    mv "$tmp" "$target"
    cap_changed
    cap_info "installed ${tool} ${ver} -> ${target}"

    cap_result_set     tool        "$tool"
    cap_result_set     path        "$target"
    cap_result_set     version     "$ver"
    cap_result_set_raw installed   true
    cap_result_set_raw preExisting "$existing"
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/install-binary.sh
go test ./scripts/install/ -run TestInstallBinary -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/install-binary.sh scripts/install/install_binary_test.go
git commit -m "$(cat <<'EOF'
feat(install): add digest-verified install.binary

Downloads one pinned artifact per tool and verifies it against the digest
committed in tool-pins.env. No curl|bash: upstream convenience installers
are unpinned, run arbitrary code, and write outside the directory the
receipt can reverse.

Records preExisting when the tool already resolves on PATH outside dest,
so uninstall never removes a k3d the user installed themselves.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `install.hostsEntries` — the `/etc/hosts` block

**Files:**
- Create: `scripts/install/hosts-entries.sh`
- Create: `scripts/install/hosts_entries_test.go`

**Interfaces:**
- Produces: capability id `install.hostsEntries`. Params: `--mode=add|remove`, `--domain` (default `memql.localhost`), `--hosts-file` (default `/etc/hosts`, injectable for tests), `--confirm`, `--dry-run`. Result: `{"mode":string,"domain":string,"hostnames":[string],"file":string,"applied":bool}`.
- The hostnames written are always `cockpit.<domain>`, `identity.<domain>`, `bff.<domain>` — the three the traefik front door serves.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/hosts_entries_test.go`:

```go
package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type hostsResult struct {
	Mode      string   `json:"mode"`
	Domain    string   `json:"domain"`
	Hostnames []string `json:"hostnames"`
	File      string   `json:"file"`
	Applied   bool     `json:"applied"`
}

const confirmPhrase = "edit my hosts file"

// writeHostsFixture creates a hosts file with pre-existing operator content
// that must survive untouched.
func writeHostsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "hosts")
	body := "127.0.0.1\tlocalhost\n::1\tlocalhost\n10.0.0.5\tmy-other-project.test\n"
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return f
}

// TestHostsAddThenRemoveRestoresByteForByte is THE test for this script. An
// uninstaller that leaves debris in /etc/hosts, or that eats a line the
// operator wrote, is worse than one that refuses to run.
func TestHostsAddThenRemoveRestoresByteForByte(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/hosts-entries.sh"
	f := writeHostsFixture(t)
	before, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	env, out := runScript(t, script, "--mode=add", "--hosts-file="+f, "--confirm="+confirmPhrase)
	if !env.OK {
		t.Fatalf("add failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	if !env.Changed {
		t.Errorf("add reported changed=false on a file with no memql block")
	}
	added, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("read after add: %v", err)
	}
	for _, h := range []string{"cockpit.memql.localhost", "identity.memql.localhost", "bff.memql.localhost"} {
		if !strings.Contains(string(added), h) {
			t.Errorf("hosts file missing %q after add", h)
		}
	}
	if !strings.Contains(string(added), "my-other-project.test") {
		t.Fatalf("add destroyed pre-existing operator content")
	}

	env, out = runScript(t, script, "--mode=remove", "--hosts-file="+f, "--confirm="+confirmPhrase)
	if !env.OK {
		t.Fatalf("remove failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	after, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("read after remove: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("remove did not restore the file byte-for-byte.\nbefore:\n%q\nafter:\n%q", before, after)
	}
}

// TestHostsAddIsIdempotent asserts a second add is a no-op (changed=false) and
// does not duplicate the block.
func TestHostsAddIsIdempotent(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/hosts-entries.sh"
	f := writeHostsFixture(t)

	if env, out := runScript(t, script, "--mode=add", "--hosts-file="+f, "--confirm="+confirmPhrase); !env.OK {
		t.Fatalf("first add failed: %s\n%s", env.Error.Message, out)
	}
	env, out := runScript(t, script, "--mode=add", "--hosts-file="+f, "--confirm="+confirmPhrase)
	if !env.OK {
		t.Fatalf("second add failed: %s\n%s", env.Error.Message, out)
	}
	if env.Changed {
		t.Errorf("second add reported changed=true; the block already existed")
	}
	b, _ := os.ReadFile(f)
	if n := strings.Count(string(b), "# BEGIN memql"); n != 1 {
		t.Errorf("found %d memql blocks, want exactly 1", n)
	}
}

// TestHostsRequiresConfirmation locks the non-interactive confirmation. The
// contract forbids a blocking prompt, so the guard is an explicit phrase --
// and a missing phrase must be exit 3 (refused), never a silent edit.
func TestHostsRequiresConfirmation(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/hosts-entries.sh"
	f := writeHostsFixture(t)
	env, out := runScript(t, script, "--mode=add", "--hosts-file="+f)
	if env.OK {
		t.Fatalf("edited the hosts file with no confirmation; output:\n%s", out)
	}
	if env.Error == nil || env.Error.Code != 3 {
		t.Errorf("want error.code 3 (refused); got %+v", env.Error)
	}
}

// TestHostsResultShape guards the fields the receipt depends on.
func TestHostsResultShape(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/hosts-entries.sh"
	f := writeHostsFixture(t)
	env, _ := runScript(t, script, "--mode=add", "--hosts-file="+f, "--confirm="+confirmPhrase, "--dry-run")
	var r hostsResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the hosts schema: %v", err)
	}
	if len(r.Hostnames) != 3 {
		t.Errorf("hostnames = %v, want 3 entries", r.Hostnames)
	}
	if r.Applied {
		t.Errorf("dry run reported applied=true")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestHosts -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/hosts-entries.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/hosts-entries.sh
# ================================
#
# Capability: install.hostsEntries -- add or remove the memQL front-door
# hostnames in /etc/hosts.
#
# A DELIMITED BLOCK, NOT SCATTERED LINES. Everything this script writes lives
# between "# BEGIN memql" and "# END memql". That is what makes removal exact:
# uninstall deletes the block and the file returns to what it was, byte for
# byte, including the operator's own entries and blank lines. Matching on
# hostnames instead would eat a line an operator had written themselves for the
# same host, which is silent data loss in a file people hand-edit.
#
# WHY A CONFIRMATION PHRASE, NOT A PROMPT. The contract forbids a blocking
# `read -p` (rule 3), and this is the most invasive thing the installer does to
# a shared system file. The guard is an explicit --confirm=<phrase> the caller
# must pass, so an automation and a human clear the same bar and neither can be
# surprised by an edit.
#
# WHY THE HOSTS FILE IS A PARAM. /etc/hosts cannot be written in a test, and a
# script whose destructive path is untestable is a script whose destructive
# path is untested. --hosts-file defaults to /etc/hosts and is redirected by
# the test suite.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/hosts-entries.sh --mode=add --confirm='edit my hosts file'
#   scripts/install/hosts-entries.sh --mode=remove --confirm='edit my hosts file'
#   scripts/install/hosts-entries.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 3 refused (no/incorrect confirmation) |
#             4 prerequisite missing (hosts file absent) | 5 write failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.hostsEntries" "Add or remove the memQL front-door hostnames in the hosts file."
cap_spec_param "mode"       "add | remove"
cap_spec_param "domain"     "front-door domain suffix"
cap_spec_param "hosts-file" "path of the hosts file to edit"
cap_spec_param "confirm"    "confirmation phrase: edit my hosts file"
cap_spec_param "dry-run"    "report what would happen without writing (flag)"

readonly BEGIN_MARK="# BEGIN memql"
readonly END_MARK="# END memql"
readonly CONFIRM_PHRASE="edit my hosts file"

# hostnames_for <domain> -- the three names the traefik front door serves.
function hostnames_for() {
    local d="$1"
    printf 'cockpit.%s identity.%s bff.%s' "$d" "$d" "$d"
}

# has_block <file> -- 0 when a memql block is present.
function has_block() {
    grep -qF "$BEGIN_MARK" "$1"
}

# strip_block <file> -- prints <file> with the memql block removed. Deleting
# the trailing newline the block introduced is what makes add->remove a
# byte-for-byte round trip.
function strip_block() {
    awk -v b="$BEGIN_MARK" -v e="$END_MARK" '
        $0 == b { skip = 1; next }
        $0 == e { skip = 0; next }
        !skip   { print }
    ' "$1"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local mode domain file confirm dry
    mode="$(cap_param mode "")"
    domain="$(cap_param domain "memql.localhost")"
    file="$(cap_param hosts-file "/etc/hosts")"
    confirm="$(cap_param confirm "")"
    dry="$(cap_flag dry-run)"
    cap_require mode "$mode"

    case "$mode" in
        add|remove) ;;
        *) cap_fail 2 "unknown mode '${mode}': expected add or remove" ;;
    esac
    [[ -f "$file" ]] || cap_fail 4 "hosts file not found at ${file}"

    local names names_json
    names="$(hostnames_for "$domain")"
    names_json="$(printf '%s' "$names" | awk '{for(i=1;i<=NF;i++){printf "%s\"%s\"", (i>1?",":""), $i}}')"

    if [[ -n "$dry" ]]; then
        cap_info "DRY RUN: would ${mode} the memql block in ${file} for ${names}"
        cap_result_set     mode      "$mode"
        cap_result_set     domain    "$domain"
        cap_result_set_raw hostnames "[${names_json}]"
        cap_result_set     file      "$file"
        cap_result_set_raw applied   false
        cap_ok
    fi

    cap_confirm_or_die "$confirm" "$CONFIRM_PHRASE"

    local tmp
    tmp="$(mktemp)"

    if [[ "$mode" == "add" ]]; then
        if has_block "$file"; then
            rm -f "$tmp"
            cap_info "memql block already present in ${file}; nothing to do"
            cap_result_set     mode      "$mode"
            cap_result_set     domain    "$domain"
            cap_result_set_raw hostnames "[${names_json}]"
            cap_result_set     file      "$file"
            cap_result_set_raw applied   false
            cap_ok
        fi
        cat "$file" > "$tmp"
        {
            printf '%s\n' "$BEGIN_MARK"
            local h
            for h in $names; do
                printf '127.0.0.1\t%s\n' "$h"
            done
            printf '%s\n' "$END_MARK"
        } >> "$tmp"
    else
        if ! has_block "$file"; then
            rm -f "$tmp"
            cap_info "no memql block in ${file}; nothing to remove"
            cap_result_set     mode      "$mode"
            cap_result_set     domain    "$domain"
            cap_result_set_raw hostnames "[${names_json}]"
            cap_result_set     file      "$file"
            cap_result_set_raw applied   false
            cap_ok
        fi
        strip_block "$file" > "$tmp"
    fi

    if ! cat "$tmp" > "$file" 2>/dev/null; then
        rm -f "$tmp"
        cap_fail 5 "cannot write ${file}: re-run with the privileges needed to edit it"
    fi
    rm -f "$tmp"
    cap_changed
    cap_info "${mode} complete for ${names} in ${file}"

    cap_result_set     mode      "$mode"
    cap_result_set     domain    "$domain"
    cap_result_set_raw hostnames "[${names_json}]"
    cap_result_set     file      "$file"
    cap_result_set_raw applied   true
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/hosts-entries.sh
go test ./scripts/install/ -run TestHosts -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both. If the byte-for-byte test fails, the block's trailing newline handling in `strip_block` is the cause — the fixture must come back exactly as written.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/hosts-entries.sh scripts/install/hosts_entries_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.hostsEntries with byte-exact removal

Everything written lives between "# BEGIN memql" and "# END memql", so
removal restores the file byte-for-byte. Matching on hostnames instead
would eat a line the operator wrote themselves for the same host.

Confirmation is an explicit --confirm phrase (the contract forbids a
blocking prompt), and --hosts-file is a param so the destructive path is
actually testable.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `install.mkcert` — CA trust and the wildcard certificate

**Files:**
- Create: `scripts/install/mkcert-setup.sh`
- Create: `scripts/install/mkcert_setup_test.go`

**Interfaces:**
- Consumes: `mkcert` on PATH or at `--mkcert-bin` (Task 3 installs it).
- Produces: capability id `install.mkcert`. Params: `--domain`, `--out-dir` (default `$HOME/.memql/tls`), `--mkcert-bin`, `--install-ca` (flag), `--dry-run`. Result: `{"domain":string,"certPath":string,"keyPath":string,"caRoot":string,"caPreExisting":bool,"caInstalled":bool,"certIssued":bool}`.
- `caPreExisting` is **true when a mkcert CA already existed in the trust store before this run**. The receipt carries it so uninstall never uninstalls a CA that predates memQL and may be signing other projects' certificates.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/mkcert_setup_test.go`:

```go
package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type mkcertResult struct {
	Domain        string `json:"domain"`
	CertPath      string `json:"certPath"`
	KeyPath       string `json:"keyPath"`
	CARoot        string `json:"caRoot"`
	CAPreExisting bool   `json:"caPreExisting"`
	CAInstalled   bool   `json:"caInstalled"`
	CertIssued    bool   `json:"certIssued"`
}

// fakeMkcert writes a stub mkcert that records its argv and fakes the two
// subcommands the script uses. Testing against the real binary would mutate
// the runner's trust store, which is exactly the side effect under test.
func fakeMkcert(t *testing.T, caRootDir string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mkcert")
	body := `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  -CAROOT) printf '%s\n' "` + caRootDir + `" ;;
  -install) printf 'installed\n' >&2 ;;
  *)
    # Emulate issuance: -cert-file <p> -key-file <p> <names...>
    cert=""; key=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        -cert-file) cert="$2"; shift 2 ;;
        -key-file)  key="$2";  shift 2 ;;
        *) shift ;;
      esac
    done
    [[ -n "$cert" ]] && printf 'CERT\n' > "$cert"
    [[ -n "$key"  ]] && printf 'KEY\n'  > "$key"
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake mkcert: %v", err)
	}
	return bin
}

// TestMkcertReportsPreExistingCA is the assertion uninstall depends on: when a
// rootCA.pem already exists, the script must report caPreExisting=true and
// leave the trust store alone.
func TestMkcertReportsPreExistingCA(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/mkcert-setup.sh"
	caRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(caRoot, "rootCA.pem"), []byte("PRE-EXISTING"), 0o644); err != nil {
		t.Fatalf("seed CA: %v", err)
	}
	out := t.TempDir()
	bin := fakeMkcert(t, caRoot)

	env, raw := runScript(t, script,
		"--domain=memql.localhost", "--out-dir="+out, "--mkcert-bin="+bin, "--install-ca")
	if !env.OK {
		t.Fatalf("mkcert setup failed: %s\noutput:\n%s", env.Error.Message, raw)
	}
	var r mkcertResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the mkcert schema: %v", err)
	}
	if !r.CAPreExisting {
		t.Errorf("caPreExisting = false, but rootCA.pem existed before the run")
	}
	if r.CAInstalled {
		t.Errorf("caInstalled = true; a pre-existing CA must not be re-installed")
	}
	if !r.CertIssued {
		t.Errorf("certIssued = false; the wildcard pair should still be issued")
	}
	for _, p := range []string{r.CertPath, r.KeyPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected issued file at %s: %v", p, err)
		}
	}
}

// TestMkcertInstallsCAWhenAbsent covers the other half: no rootCA.pem means we
// created the CA, and the receipt may later remove it.
func TestMkcertInstallsCAWhenAbsent(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/mkcert-setup.sh"
	caRoot := t.TempDir()
	bin := fakeMkcert(t, caRoot)

	env, raw := runScript(t, script,
		"--domain=memql.localhost", "--out-dir="+t.TempDir(), "--mkcert-bin="+bin, "--install-ca")
	if !env.OK {
		t.Fatalf("mkcert setup failed: %s\noutput:\n%s", env.Error.Message, raw)
	}
	var r mkcertResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the mkcert schema: %v", err)
	}
	if r.CAPreExisting {
		t.Errorf("caPreExisting = true on an empty CAROOT")
	}
	if !r.CAInstalled {
		t.Errorf("caInstalled = false; the CA should have been installed")
	}
	if !env.Changed {
		t.Errorf("changed = false after installing a CA and issuing a cert")
	}
}

// TestMkcertMissingBinaryIsPrerequisiteFailure locks the exit code: a missing
// mkcert is 4 (prerequisite missing), which the wizard renders as a guided
// instruction rather than an error.
func TestMkcertMissingBinaryIsPrerequisiteFailure(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/mkcert-setup.sh"
	env, _ := runScript(t, script,
		"--domain=memql.localhost", "--out-dir="+t.TempDir(),
		"--mkcert-bin="+filepath.Join(t.TempDir(), "absent"))
	if env.OK {
		t.Fatal("succeeded with no mkcert binary")
	}
	if env.Error == nil || env.Error.Code != 4 {
		t.Errorf("want error.code 4 (prerequisite missing); got %+v", env.Error)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestMkcert -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/mkcert-setup.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/mkcert-setup.sh
# ===============================
#
# Capability: install.mkcert -- ensure a trusted local CA and issue the memQL
# front-door wildcard certificate.
#
# caPreExisting IS THE WHOLE POINT OF THE PROBE. mkcert's CA lives in the
# operator's system trust store and may already be signing certificates for
# other projects. So the script asks, BEFORE doing anything, whether a
# rootCA.pem is already there; if it is, the CA is left exactly as found and
# only the certificate is issued. The receipt carries the flag so uninstall
# can offer "remove the CA" only when memQL is the thing that created it.
#
# WHY --mkcert-bin IS A PARAM. The tool is installed into ~/.memql/bin by
# install.binary, which is not on PATH for the invoking process. Taking the
# path explicitly avoids depending on a PATH mutation that may not have
# happened yet -- and lets the tests substitute a stub instead of mutating the
# runner's real trust store.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/mkcert-setup.sh --domain=memql.localhost --install-ca
#   scripts/install/mkcert-setup.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (mkcert absent) |
#             5 CA install or certificate issuance failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.mkcert" "Ensure a trusted local CA and issue the memQL front-door wildcard certificate."
cap_spec_param "domain"     "front-door domain suffix"
cap_spec_param "out-dir"    "directory to write the certificate and key into"
cap_spec_param "mkcert-bin" "path to the mkcert binary"
cap_spec_param "install-ca" "install the CA into the system trust store when absent (flag)"
cap_spec_param "dry-run"    "report what would happen without writing (flag)"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local domain out_dir bin install_ca dry
    domain="$(cap_param domain "memql.localhost")"
    out_dir="$(cap_param out-dir "${HOME}/.memql/tls")"
    bin="$(cap_param mkcert-bin "${HOME}/.memql/bin/mkcert")"
    install_ca="$(cap_flag install-ca)"
    dry="$(cap_flag dry-run)"

    # Fall back to PATH when the explicit path is not executable, so an
    # operator who installed mkcert themselves is not forced to pass a flag.
    if [[ ! -x "$bin" ]]; then
        if command -v mkcert &>/dev/null; then
            bin="$(command -v mkcert)"
        else
            cap_fail 4 "mkcert not found at ${bin} and not on PATH; install it first (install.binary --tool=mkcert)"
        fi
    fi

    local cert_path key_path
    cert_path="${out_dir}/${domain}.crt"
    key_path="${out_dir}/${domain}.key"

    if [[ -n "$dry" ]]; then
        cap_info "DRY RUN: would ensure a CA and issue *.${domain} into ${out_dir}"
        cap_result_set     domain        "$domain"
        cap_result_set     certPath      "$cert_path"
        cap_result_set     keyPath       "$key_path"
        cap_result_set     caRoot        ""
        cap_result_set_raw caPreExisting false
        cap_result_set_raw caInstalled   false
        cap_result_set_raw certIssued    false
        cap_ok
    fi

    local ca_root ca_pre=false ca_installed=false
    ca_root="$("$bin" -CAROOT 2>/dev/null | head -1 || true)"
    [[ -n "$ca_root" ]] || cap_fail 5 "could not determine mkcert CAROOT"

    if [[ -f "${ca_root}/rootCA.pem" ]]; then
        ca_pre=true
        cap_info "existing mkcert CA found at ${ca_root}; leaving the trust store untouched"
    elif [[ -n "$install_ca" ]]; then
        cap_info "installing a new mkcert CA into the system trust store"
        if ! "$bin" -install >&2; then
            cap_fail 5 "mkcert -install failed; the CA could not be added to the trust store"
        fi
        ca_installed=true
        cap_changed
    else
        cap_fail 4 "no mkcert CA present and --install-ca was not passed; the front door cannot serve trusted TLS"
    fi

    mkdir -p "$out_dir"
    cap_info "issuing certificate for *.${domain} and ${domain}"
    if ! "$bin" -cert-file "$cert_path" -key-file "$key_path" "*.${domain}" "${domain}" >&2; then
        cap_fail 5 "mkcert certificate issuance failed for *.${domain}"
    fi
    chmod 0600 "$key_path" 2>/dev/null || true
    cap_changed

    cap_result_set     domain        "$domain"
    cap_result_set     certPath      "$cert_path"
    cap_result_set     keyPath       "$key_path"
    cap_result_set     caRoot        "$ca_root"
    cap_result_set_raw caPreExisting "$ca_pre"
    cap_result_set_raw caInstalled   "$ca_installed"
    cap_result_set_raw certIssued    true
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/mkcert-setup.sh
go test ./scripts/install/ -run TestMkcert -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/mkcert-setup.sh scripts/install/mkcert_setup_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.mkcert with pre-existing CA detection

Probes for an existing rootCA.pem before touching the trust store. When
one is found the CA is left exactly as it was and only the certificate is
issued; the receipt carries caPreExisting so uninstall offers CA removal
ONLY when memQL created it. That CA may be signing other projects' certs.

Tests drive a stub mkcert -- exercising the real binary would mutate the
runner's trust store, which is the side effect under test.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: `install.cloneStack` — the pinned checkout

**Files:**
- Create: `scripts/install/clone-stack.sh`
- Create: `scripts/install/clone_stack_test.go`

**Interfaces:**
- Produces: capability id `install.cloneStack`. Params: `--repo-url`, `--ref` (a tag), `--dest` (default `$HOME/.memql/src`), `--allow-branch` (flag, dev escape hatch), `--dry-run`. Result: `{"repoUrl":string,"ref":string,"dest":string,"cloned":bool,"updated":bool,"commit":string}`.
- Refusing a non-tag ref is the point: `scripts/k3d/up.sh` defaults `targetRevision` to the operator's current branch, which is right for repo development and wrong for an install (spec D3).

- [ ] **Step 1: Write the failing test**

Create `scripts/install/clone_stack_test.go`:

```go
package install

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

type cloneResult struct {
	RepoURL string `json:"repoUrl"`
	Ref     string `json:"ref"`
	Dest    string `json:"dest"`
	Cloned  bool   `json:"cloned"`
	Updated bool   `json:"updated"`
	Commit  string `json:"commit"`
}

// originRepo builds a throwaway git repo with one commit and one tag, so the
// clone path is exercised without network access.
func originRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := exec.Command("bash", "-c", "printf 'stack\n' > "+filepath.Join(dir, "README")).Run(); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	run("add", "README")
	run("commit", "-q", "-m", "seed")
	run("tag", "v0.0.1")
	return dir
}

// TestCloneStackRefusesABranchRef is the guard that keeps an install off a
// moving target. up.sh defaults to the operator's current branch, which is
// correct for repo development and wrong for an install.
func TestCloneStackRefusesABranchRef(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/clone-stack.sh"
	origin := originRepo(t)
	env, out := runScript(t, script,
		"--repo-url="+origin, "--ref=main", "--dest="+filepath.Join(t.TempDir(), "src"))
	if env.OK {
		t.Fatalf("accepted a branch ref; output:\n%s", out)
	}
	if env.Error == nil || env.Error.Code != 2 {
		t.Errorf("want error.code 2 (bad param) for a branch ref; got %+v", env.Error)
	}
}

// TestCloneStackClonesAtTagThenIsIdempotent covers the happy path and the
// resume path: re-running against an existing checkout at the same tag must be
// a no-op, because the wizard re-runs every step's verify on resume.
func TestCloneStackClonesAtTagThenIsIdempotent(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/clone-stack.sh"
	origin := originRepo(t)
	dest := filepath.Join(t.TempDir(), "src")

	env, out := runScript(t, script, "--repo-url="+origin, "--ref=v0.0.1", "--dest="+dest)
	if !env.OK {
		t.Fatalf("clone failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	var r cloneResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the clone schema: %v", err)
	}
	if !r.Cloned {
		t.Errorf("cloned = false on a fresh destination")
	}
	if r.Commit == "" {
		t.Errorf("commit is empty; the receipt records it to prove what was installed")
	}
	if !env.Changed {
		t.Errorf("changed = false after a fresh clone")
	}

	env, out = runScript(t, script, "--repo-url="+origin, "--ref=v0.0.1", "--dest="+dest)
	if !env.OK {
		t.Fatalf("second run failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	if env.Changed {
		t.Errorf("second run reported changed=true; the checkout was already at the tag")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestCloneStack -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/clone-stack.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/clone-stack.sh
# ==============================
#
# Capability: install.cloneStack -- fetch the memQL stack at a pinned tag.
#
# TAGS ONLY, BY DEFAULT. scripts/k3d/up.sh points its ArgoCD Application at the
# operator's CURRENT BRANCH, which is exactly right when you are developing the
# repo and exactly wrong when you are installing a cluster: a branch moves, so
# two installs a week apart are not the same install and neither is
# reproducible. Anything that does not resolve to an annotated or lightweight
# TAG is refused with exit 2. --allow-branch exists for developing the
# installer itself and is never passed by the graph.
#
# IDEMPOTENT ON RESUME. The wizard re-runs every step's verify when it resumes,
# so an existing checkout already at the requested tag must report
# changed=false rather than re-cloning -- otherwise resuming an install would
# discard local state and take minutes for nothing.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/clone-stack.sh --ref=v1.4.0
#   scripts/install/clone-stack.sh --repo-url=/path/to/repo --ref=v0.0.1 --dest=/tmp/src
#   scripts/install/clone-stack.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param (non-tag ref) | 4 prerequisite missing (git) |
#             5 clone or checkout failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.cloneStack" "Fetch the memQL stack at a pinned release tag."
cap_spec_param "repo-url"     "git repository URL or path"
cap_spec_param "ref"          "release TAG to check out"
cap_spec_param "dest"         "destination directory for the checkout"
cap_spec_param "allow-branch" "permit a non-tag ref (installer development only) (flag)"
cap_spec_param "dry-run"      "report what would happen without cloning (flag)"

# ref_is_tag <repo-url> <ref> -- 0 when <ref> exists as a tag on the remote.
function ref_is_tag() {
    git ls-remote --tags "$1" "refs/tags/$2" 2>/dev/null | grep -q .
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local repo_url ref dest allow_branch dry
    repo_url="$(cap_param repo-url "https://github.com/znasllc-io/memql.git")"
    ref="$(cap_param ref "")"
    dest="$(cap_param dest "${HOME}/.memql/src")"
    allow_branch="$(cap_flag allow-branch)"
    dry="$(cap_flag dry-run)"
    cap_require ref "$ref"

    command -v git &>/dev/null || cap_fail 4 "git is required to fetch the memQL stack"

    if [[ -z "$allow_branch" ]] && ! ref_is_tag "$repo_url" "$ref"; then
        cap_fail 2 "ref '${ref}' is not a tag on ${repo_url}. An install must pin a release tag -- a branch moves, so two installs of the same 'version' would not be the same install. Pass --allow-branch only when developing the installer."
    fi

    if [[ -n "$dry" ]]; then
        cap_info "DRY RUN: would clone ${repo_url} at ${ref} into ${dest}"
        cap_result_set     repoUrl "$repo_url"
        cap_result_set     ref     "$ref"
        cap_result_set     dest    "$dest"
        cap_result_set_raw cloned  false
        cap_result_set_raw updated false
        cap_result_set     commit  ""
        cap_ok
    fi

    local cloned=false updated=false commit=""

    if [[ -d "${dest}/.git" ]]; then
        local current
        current="$(git -C "$dest" describe --tags --exact-match 2>/dev/null || true)"
        if [[ "$current" == "$ref" ]]; then
            commit="$(git -C "$dest" rev-parse HEAD)"
            cap_info "checkout at ${dest} is already at ${ref}; nothing to do"
            cap_result_set     repoUrl "$repo_url"
            cap_result_set     ref     "$ref"
            cap_result_set     dest    "$dest"
            cap_result_set_raw cloned  false
            cap_result_set_raw updated false
            cap_result_set     commit  "$commit"
            cap_ok
        fi
        cap_info "updating ${dest} to ${ref}"
        git -C "$dest" fetch --tags --depth 1 origin "refs/tags/${ref}:refs/tags/${ref}" >&2 \
            || cap_fail 5 "fetch of tag ${ref} failed in ${dest}"
        git -C "$dest" checkout -q "tags/${ref}" >&2 \
            || cap_fail 5 "checkout of tag ${ref} failed in ${dest}"
        updated=true
    else
        mkdir -p "$(dirname "$dest")"
        cap_info "cloning ${repo_url} at ${ref} into ${dest}"
        git clone --quiet --depth 1 --branch "$ref" "$repo_url" "$dest" >&2 \
            || cap_fail 5 "clone of ${repo_url} at ${ref} failed"
        cloned=true
    fi

    commit="$(git -C "$dest" rev-parse HEAD)"
    cap_changed
    cap_info "stack at ${ref} (${commit}) in ${dest}"

    cap_result_set     repoUrl "$repo_url"
    cap_result_set     ref     "$ref"
    cap_result_set     dest    "$dest"
    cap_result_set_raw cloned  "$cloned"
    cap_result_set_raw updated "$updated"
    cap_result_set     commit  "$commit"
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/clone-stack.sh
go test ./scripts/install/ -run TestCloneStack -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/clone-stack.sh scripts/install/clone_stack_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.cloneStack pinned to a release tag

Refuses any ref that is not a tag. up.sh points ArgoCD at the operator's
current BRANCH, which is right for repo development and wrong for an
install: a branch moves, so two installs of the same "version" are not
the same install. --allow-branch exists only for developing the installer.

Idempotent on resume -- an existing checkout already at the tag reports
changed=false instead of re-cloning.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: `install.verifyProviderKey` — one live call, no key in the logs

**Files:**
- Create: `scripts/install/verify-provider-key.sh`
- Create: `scripts/install/verify_provider_key_test.go`

**Interfaces:**
- Produces: capability id `install.verifyProviderKey`. Params: `--vendor=anthropic|openai`, `--api-key`, `--base-url`, `--curl-bin`. Result: `{"vendor":string,"reachable":bool,"status":number}`.
- Uses each vendor's **model-list** endpoint: a real authenticated call that spends no tokens. `GET /v1/models` with `x-api-key` + `anthropic-version` (Anthropic) or `Authorization: Bearer` (OpenAI).
- A rejected key is **exit 3 (refused)**, not 5. The key is a user-supplied credential, and "your key was rejected" is a different remedy from "the network failed".

- [ ] **Step 1: Write the failing test**

Create `scripts/install/verify_provider_key_test.go`:

```go
package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type providerResult struct {
	Vendor    string `json:"vendor"`
	Reachable bool   `json:"reachable"`
	Status    int    `json:"status"`
}

// fakeCurl writes a stub curl that echoes a fixed HTTP status and records the
// argv it was given, so the test can assert the key never reaches the command
// line (where `ps` would expose it to every user on the machine).
func fakeCurl(t *testing.T, status, argvLog string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "curl")
	body := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "` + argvLog + `"
printf '` + status + `'
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}
	return bin
}

// TestVerifyProviderKeyNeverPutsTheKeyOnTheCommandLine is the security
// assertion. A key passed as a curl argument is visible in `ps` output to every
// user on the box for the lifetime of the call.
func TestVerifyProviderKeyNeverPutsTheKeyOnTheCommandLine(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/verify-provider-key.sh"
	log := filepath.Join(t.TempDir(), "argv.log")
	secret := "sk-ant-SECRETVALUE12345"

	env, out := runScript(t, script,
		"--vendor=anthropic", "--api-key="+secret, "--curl-bin="+fakeCurl(t, "200", log))
	if !env.OK {
		t.Fatalf("verify failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("the API key appeared in curl's argv:\n%s", b)
	}
	if strings.Contains(out, secret) {
		t.Errorf("the API key was written to the script's own output")
	}
}

// TestVerifyProviderKeyRejectedKeyIsRefusal locks the exit code: 401/403 is
// exit 3 (refused), which the wizard renders as "check your key", not exit 5
// ("something broke"), which sends the user hunting the wrong problem.
func TestVerifyProviderKeyRejectedKeyIsRefusal(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/verify-provider-key.sh"
	log := filepath.Join(t.TempDir(), "argv.log")

	env, _ := runScript(t, script,
		"--vendor=openai", "--api-key=bad", "--curl-bin="+fakeCurl(t, "401", log))
	if env.OK {
		t.Fatal("a 401 was reported as success")
	}
	if env.Error == nil || env.Error.Code != 3 {
		t.Errorf("want error.code 3 (refused) for a rejected key; got %+v", env.Error)
	}
}

// TestVerifyProviderKeySuccessShape guards the fields the graph reads.
func TestVerifyProviderKeySuccessShape(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/verify-provider-key.sh"
	log := filepath.Join(t.TempDir(), "argv.log")

	env, _ := runScript(t, script,
		"--vendor=openai", "--api-key=good", "--curl-bin="+fakeCurl(t, "200", log))
	var r providerResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the provider schema: %v", err)
	}
	if !r.Reachable || r.Status != 200 || r.Vendor != "openai" {
		t.Errorf("unexpected result: %+v", r)
	}
	if env.Changed {
		t.Errorf("verification reported changed=true; it must not mutate")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestVerifyProviderKey -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/verify-provider-key.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/verify-provider-key.sh
# ======================================
#
# Capability: install.verifyProviderKey -- prove an AI provider key works
# before the cluster is built around it.
#
# THE KEY NEVER TOUCHES A COMMAND LINE. curl arguments are visible in `ps` to
# every user on the machine for the duration of the call, so the auth header is
# written to a 0600 curl config file and passed with `-K`. The file is removed
# on every exit path. This is the whole reason the script does not simply call
# `curl -H "Authorization: Bearer $key"`.
#
# THE MODEL-LIST ENDPOINT, NOT A COMPLETION. GET /v1/models is a real
# authenticated request that spends no tokens, so verification is free and
# cannot be mistaken for usage on the user's bill.
#
# A REJECTED KEY IS EXIT 3, NOT 5. "Your key was rejected" and "the call failed"
# have different remedies, and the wizard renders 3 as a refusal the user can
# fix by pasting a different key.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/verify-provider-key.sh --vendor=anthropic --api-key=sk-ant-...
#   scripts/install/verify-provider-key.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 3 refused (key rejected) |
#             4 prerequisite missing (curl) | 5 unreachable / unexpected status

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.verifyProviderKey" "Verify an AI provider API key with one free authenticated call."
cap_spec_param "vendor"   "anthropic | openai"
cap_spec_param "api-key"  "the API key to verify"
cap_spec_param "base-url" "override the vendor API base URL"
cap_spec_param "curl-bin" "path to the curl binary"

CFG=""
function cleanup() { [[ -n "$CFG" && -f "$CFG" ]] && rm -f "$CFG"; }
trap cleanup EXIT

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local vendor key base curl_bin
    vendor="$(cap_param vendor "")"
    key="$(cap_param api-key "")"
    curl_bin="$(cap_param curl-bin "curl")"
    cap_require vendor "$vendor"
    cap_require api-key "$key"

    case "$vendor" in
        anthropic) base="$(cap_param base-url "https://api.anthropic.com")" ;;
        openai)    base="$(cap_param base-url "https://api.openai.com")" ;;
        *) cap_fail 2 "unknown vendor '${vendor}': expected anthropic or openai" ;;
    esac

    command -v "$curl_bin" &>/dev/null || [[ -x "$curl_bin" ]] \
        || cap_fail 4 "curl not found at '${curl_bin}'"

    # Auth via a 0600 config file so the key is never an argv element.
    CFG="$(mktemp)"
    chmod 0600 "$CFG"
    if [[ "$vendor" == "anthropic" ]]; then
        {
            printf 'header = "x-api-key: %s"\n' "$key"
            printf 'header = "anthropic-version: 2023-06-01"\n'
        } > "$CFG"
    else
        printf 'header = "Authorization: Bearer %s"\n' "$key" > "$CFG"
    fi

    local status
    cap_info "verifying ${vendor} credentials against ${base}/v1/models"
    status="$("$curl_bin" -sS -o /dev/null -w '%{http_code}' \
        -K "$CFG" --max-time 20 "${base}/v1/models" 2>/dev/null || true)"
    [[ -n "$status" ]] || status=0

    cap_result_set     vendor    "$vendor"
    cap_result_set_raw status    "$status"

    case "$status" in
        200)
            cap_info "${vendor} key verified"
            cap_result_set_raw reachable true
            cap_ok
            ;;
        401|403)
            cap_result_set_raw reachable true
            cap_fail 3 "${vendor} rejected the API key (HTTP ${status}). Check the key and paste it again."
            ;;
        0)
            cap_result_set_raw reachable false
            cap_fail 5 "could not reach ${base}; check network connectivity"
            ;;
        *)
            cap_result_set_raw reachable true
            cap_fail 5 "${vendor} returned an unexpected HTTP ${status} from /v1/models"
            ;;
    esac
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/verify-provider-key.sh
go test ./scripts/install/ -run TestVerifyProviderKey -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/verify-provider-key.sh scripts/install/verify_provider_key_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.verifyProviderKey

Verifies the key with GET /v1/models -- a real authenticated call that
spends no tokens. The key goes into a 0600 curl config file, never an
argv element: curl arguments are visible in `ps` to every user on the box.

A rejected key exits 3 (refused), not 5, because "check your key" and
"the call failed" send the user to different places.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: `install.verifyFrontDoor` — DNS, TLS, and gRPC

**Files:**
- Create: `scripts/install/verify-frontdoor.sh`
- Create: `scripts/install/verify_frontdoor_test.go`

**Interfaces:**
- Produces: capability id `install.verifyFrontDoor`. Params: `--domain`, `--report-only` (flag), `--curl-bin`, `--timeout`. Result: `{"domain":string,"checks":[{"name":string,"host":string,"passed":bool,"detail":string}],"passedCount":number,"failedCount":number}`.
- Default is **strict**: any failed check is exit 5. `--report-only` returns the same result with exit 0 and is what the tests and the wizard's diagnostics view use.
- Per-check results rather than one boolean: "the front door is broken" is not actionable, "identity.memql.localhost does not resolve" is.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/verify_frontdoor_test.go`:

```go
package install

import (
	"encoding/json"
	"testing"
)

type frontDoorCheck struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type frontDoorResult struct {
	Domain      string           `json:"domain"`
	Checks      []frontDoorCheck `json:"checks"`
	PassedCount int              `json:"passedCount"`
	FailedCount int              `json:"failedCount"`
	AllPassed   bool             `json:"allPassed"`
}

// TestVerifyFrontDoorReportsPerCheck asserts the script names WHICH check
// failed. Against a domain that cannot resolve, every DNS check must fail and
// each must be individually attributable -- "the front door is broken" is not
// something a user can act on.
func TestVerifyFrontDoorReportsPerCheck(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/verify-frontdoor.sh"
	env, out := runScript(t, script,
		"--domain=nx-does-not-exist.invalid", "--report-only", "--timeout=2")
	if !env.OK {
		t.Fatalf("--report-only must exit 0 even when checks fail: %s\noutput:\n%s", env.Error.Message, out)
	}
	var r frontDoorResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the front-door schema: %v", err)
	}
	// Three hostnames x (dns, tls) plus one grpc check.
	if len(r.Checks) < 7 {
		t.Fatalf("got %d checks, want at least 7 (dns+tls per host, plus grpc)", len(r.Checks))
	}
	if r.FailedCount == 0 {
		t.Errorf("failedCount = 0 against an unresolvable domain")
	}
	for _, c := range r.Checks {
		if c.Name == "" || c.Host == "" {
			t.Errorf("check missing name/host: %+v", c)
		}
		if !c.Passed && c.Detail == "" {
			t.Errorf("failed check %q has no detail; the user cannot act on it", c.Name)
		}
	}
}

// TestVerifyFrontDoorStrictFailsTheStep locks the default: without
// --report-only a failing check is exit 5, so the graph halts instead of
// declaring an unreachable cluster installed.
func TestVerifyFrontDoorStrictFailsTheStep(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/verify-frontdoor.sh"
	env, _ := runScript(t, script, "--domain=nx-does-not-exist.invalid", "--timeout=2")
	if env.OK {
		t.Fatal("strict mode reported success against an unresolvable domain")
	}
	if env.Error == nil || env.Error.Code != 5 {
		t.Errorf("want error.code 5 (operation failed); got %+v", env.Error)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestVerifyFrontDoor -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/verify-frontdoor.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/verify-frontdoor.sh
# ===================================
#
# Capability: install.verifyFrontDoor -- prove the cluster is actually reachable
# the way a client reaches it.
#
# PER-CHECK RESULTS, NOT ONE BOOLEAN. Three things can be wrong independently:
# the name does not resolve, it resolves but TLS is untrusted, or both are fine
# and the gRPC head is not answering. Collapsing them into "healthy: false"
# throws away the only information that tells the user what to fix, so every
# check reports its own name, host, pass flag, and detail.
#
# STRICT BY DEFAULT. This is a verify step: the graph advances on it, so a
# failed check must halt the run (exit 5). --report-only returns the identical
# result with exit 0 and exists for diagnostics and for tests.
#
# DNS MUST RESOLVE TO 127.0.0.1 SPECIFICALLY. A name that resolves somewhere
# else is a worse failure than one that does not resolve at all -- it means the
# client would silently talk to a different machine.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/verify-frontdoor.sh --domain=memql.localhost
#   scripts/install/verify-frontdoor.sh --domain=local.znas.io --report-only
#   scripts/install/verify-frontdoor.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (curl) |
#             5 one or more checks failed (strict mode)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.verifyFrontDoor" "Verify DNS, TLS trust and gRPC reachability for the memQL front door."
cap_spec_param "domain"      "front-door domain suffix"
cap_spec_param "report-only" "report results without failing the step (flag)"
cap_spec_param "curl-bin"    "path to the curl binary"
cap_spec_param "timeout"     "per-request timeout in seconds"

CHECKS=()
PASSED=0
FAILED=0

# record <name> <host> <passed-bool> <detail>
function record() {
    local name="$1" host="$2" passed="$3" detail="$4"
    CHECKS+=("{\"name\":\"$(cap_json_escape "$name")\",\"host\":\"$(cap_json_escape "$host")\",\"passed\":${passed},\"detail\":\"$(cap_json_escape "$detail")\"}")
    if [[ "$passed" == "true" ]]; then PASSED=$((PASSED + 1)); else FAILED=$((FAILED + 1)); fi
}

# resolves_to_loopback <host> -- prints the resolved addresses, empty when the
# name does not resolve at all.
function resolved_addrs() {
    getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' '
}

function check_dns() {
    local host="$1" addrs
    addrs="$(resolved_addrs "$host")"
    if [[ -z "$addrs" ]]; then
        record "dns" "$host" false "does not resolve; add the hosts entry or the wildcard DNS record"
        return
    fi
    if [[ "$addrs" != *"127.0.0.1"* ]]; then
        record "dns" "$host" false "resolves to ${addrs% } instead of 127.0.0.1 -- a client would reach a different machine"
        return
    fi
    record "dns" "$host" true "resolves to 127.0.0.1"
}

function check_tls() {
    local host="$1" curl_bin="$2" timeout="$3" status
    status="$("$curl_bin" -sS -o /dev/null -w '%{http_code}' --max-time "$timeout" \
        "https://${host}/healthz" 2>/dev/null || true)"
    if [[ -z "$status" || "$status" == "000" ]]; then
        record "tls" "$host" false "TLS handshake or connection failed; the certificate may not be trusted or the front door is down"
        return
    fi
    record "tls" "$host" true "HTTPS answered ${status} with a trusted certificate"
}

function check_grpc() {
    local host="$1" curl_bin="$2" timeout="$3" status
    # The gRPC head answers HTTP/2 on 443 behind the same front door. A plain
    # GET is refused by gRPC with a non-empty status, which is enough to prove
    # the route exists without speaking the protocol.
    status="$("$curl_bin" -sS -o /dev/null -w '%{http_code}' --http2 --max-time "$timeout" \
        "https://${host}/" 2>/dev/null || true)"
    if [[ -z "$status" || "$status" == "000" ]]; then
        record "grpc" "$host" false "no HTTP/2 response on 443; the bff route is not reachable"
        return
    fi
    record "grpc" "$host" true "HTTP/2 route answered ${status}"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local domain report_only curl_bin timeout
    domain="$(cap_param domain "memql.localhost")"
    report_only="$(cap_flag report-only)"
    curl_bin="$(cap_param curl-bin "curl")"
    timeout="$(cap_param timeout "10")"

    command -v "$curl_bin" &>/dev/null || [[ -x "$curl_bin" ]] \
        || cap_fail 4 "curl not found at '${curl_bin}'"

    local host
    for host in "cockpit.${domain}" "identity.${domain}" "bff.${domain}"; do
        check_dns "$host"
        check_tls "$host" "$curl_bin" "$timeout"
    done
    check_grpc "bff.${domain}" "$curl_bin" "$timeout"

    local joined
    joined="$(IFS=,; printf '%s' "${CHECKS[*]}")"
    cap_info "front door: ${PASSED} passed, ${FAILED} failed"

    cap_result_set     domain      "$domain"
    cap_result_set_raw checks      "[${joined}]"
    cap_result_set_raw passedCount "$PASSED"
    cap_result_set_raw failedCount "$FAILED"
    # allPassed is what the graph verifies on. A count is a number and the
    # verify kinds compare booleans, but more to the point "did every check
    # pass" is the question being asked -- deriving it here keeps that answer in
    # one place instead of in every consumer.
    cap_result_set_raw allPassed   "$( [[ "$FAILED" -eq 0 ]] && echo true || echo false )"

    if [[ "$FAILED" -gt 0 && -z "$report_only" ]]; then
        cap_fail 5 "${FAILED} front-door check(s) failed for ${domain}; see result.checks for which"
    fi
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/verify-frontdoor.sh
go test ./scripts/install/ -run TestVerifyFrontDoor -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/verify-frontdoor.sh scripts/install/verify_frontdoor_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.verifyFrontDoor with per-check results

Reports dns/tls/grpc per hostname rather than one boolean: those three
fail independently and collapsing them discards the only information the
user can act on. DNS must resolve to 127.0.0.1 specifically -- resolving
elsewhere is worse than not resolving, because the client would silently
reach a different machine.

Strict by default (exit 5) since the graph advances on this step;
--report-only returns the same result with exit 0 for diagnostics.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: `install.magicLink` — the owner's first sign-in

**Files:**
- Create: `scripts/install/magic-link.sh`
- Create: `scripts/install/magic_link_test.go`

**Interfaces:**
- Produces: capability id `install.magicLink`. Params: `--namespace`, `--deployment`, `--owner-email`, `--kubectl-bin`, `--local` (flag, required), `--since`. Result: `{"ownerEmail":string,"link":string,"found":bool}`.
- **`--local` is mandatory.** Reading a sign-in credential out of pod logs is a local-development affordance; without the flag the script refuses with exit 3 rather than doing it against whatever cluster `kubectl` happens to point at.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/magic_link_test.go`:

```go
package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type magicLinkResult struct {
	OwnerEmail string `json:"ownerEmail"`
	Link       string `json:"link"`
	Found      bool   `json:"found"`
}

// fakeKubectl writes a stub kubectl whose `logs` subcommand prints the supplied
// body. Testing against a real cluster is impossible in unit CI, and the
// parsing is the part that can actually be wrong.
func fakeKubectl(t *testing.T, logBody string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "kubectl")
	logFile := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(logFile, []byte(logBody), 0o644); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	body := `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  logs) cat "` + logFile + `" ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	return bin
}

// TestMagicLinkRequiresLocalFlag is the guard. Extracting a sign-in credential
// from pod logs is a local affordance; without --local the script must refuse
// rather than run against whatever cluster kubectl currently points at.
func TestMagicLinkRequiresLocalFlag(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/magic-link.sh"
	k := fakeKubectl(t, "nothing here\n")
	env, out := runScript(t, script,
		"--owner-email=owner@example.com", "--kubectl-bin="+k)
	if env.OK {
		t.Fatalf("ran without --local; output:\n%s", out)
	}
	if env.Error == nil || env.Error.Code != 3 {
		t.Errorf("want error.code 3 (refused); got %+v", env.Error)
	}
}

// TestMagicLinkExtractsTheMostRecentLink asserts the parse picks the LAST link
// in the log. A restarted identity pod emits more than one, and the stale one
// is single-use and already spent.
func TestMagicLinkExtractsTheMostRecentLink(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/magic-link.sh"
	logBody := "" +
		"level=info msg=\"magic link\" url=https://identity.memql.localhost/auth/complete?token=OLDTOKEN\n" +
		"level=info msg=\"unrelated line\"\n" +
		"level=info msg=\"magic link\" url=https://identity.memql.localhost/auth/complete?token=NEWTOKEN\n"
	env, out := runScript(t, script,
		"--owner-email=owner@example.com", "--local", "--kubectl-bin="+fakeKubectl(t, logBody))
	if !env.OK {
		t.Fatalf("extraction failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	var r magicLinkResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the magic-link schema: %v", err)
	}
	if !r.Found {
		t.Fatalf("found = false with two links in the log")
	}
	if r.Link != "https://identity.memql.localhost/auth/complete?token=NEWTOKEN" {
		t.Errorf("link = %q, want the MOST RECENT link (the older one is spent)", r.Link)
	}
}

// TestMagicLinkNoLinkIsPrerequisiteFailure: no link yet is exit 4, which the
// wizard renders as "waiting on identity" rather than an error.
func TestMagicLinkNoLinkIsPrerequisiteFailure(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/magic-link.sh"
	env, _ := runScript(t, script,
		"--owner-email=owner@example.com", "--local", "--kubectl-bin="+fakeKubectl(t, "boring logs\n"))
	if env.OK {
		t.Fatal("reported success with no link in the logs")
	}
	if env.Error == nil || env.Error.Code != 4 {
		t.Errorf("want error.code 4 (prerequisite missing); got %+v", env.Error)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestMagicLink -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/magic-link.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/magic-link.sh
# =============================
#
# Capability: install.magicLink -- recover the cluster owner's first sign-in
# link from the identity pod's logs.
#
# WHY THIS EXISTS. Identity's primary login is magic-link, and a local cluster
# has no mail configured -- integrations/email falls back to LogSender when
# neither Graph nor SMTP credentials are present. So "check your email" is a
# dead end on exactly the install this wizard performs. The link is already
# being printed; this reads it back.
#
# --local IS MANDATORY, DELIBERATELY. Pulling an authentication credential out
# of pod logs is a local-development affordance and nothing else. kubectl points
# at whatever context the operator last used, which could be staging, so the
# script refuses (exit 3) unless the caller states explicitly that this is a
# local cluster. Making it a required flag rather than a default keeps the
# decision at the call site, where the graph knows the answer.
#
# THE LAST LINK WINS. A restarted identity pod emits more than one; magic links
# are single-use, so the older ones are already spent and handing one back would
# fail at exactly the moment the user clicks it.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/magic-link.sh --owner-email=me@example.com --local
#   scripts/install/magic-link.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 3 refused (--local not stated) |
#             4 prerequisite missing (kubectl absent, or no link in the logs)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.magicLink" "Recover the cluster owner's first sign-in link from the identity logs."
cap_spec_param "namespace"   "kubernetes namespace holding the identity deployment"
cap_spec_param "deployment"  "identity deployment name"
cap_spec_param "owner-email" "the owner's email, recorded on the result"
cap_spec_param "kubectl-bin" "path to the kubectl binary"
cap_spec_param "local"       "affirm this is a LOCAL cluster (required) (flag)"
cap_spec_param "since"       "how far back to read logs (kubectl --since value)"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ns deploy email kubectl_bin is_local since
    ns="$(cap_param namespace "memql")"
    deploy="$(cap_param deployment "identity")"
    email="$(cap_param owner-email "")"
    kubectl_bin="$(cap_param kubectl-bin "kubectl")"
    is_local="$(cap_flag local)"
    since="$(cap_param since "1h")"
    cap_require owner-email "$email"

    if [[ -z "$is_local" ]]; then
        cap_fail 3 "refusing to read a sign-in credential from pod logs without --local. kubectl points at whatever context was last used; state explicitly that this is a local cluster."
    fi

    command -v "$kubectl_bin" &>/dev/null || [[ -x "$kubectl_bin" ]] \
        || cap_fail 4 "kubectl not found at '${kubectl_bin}'"

    local logs link
    logs="$("$kubectl_bin" logs -n "$ns" "deploy/${deploy}" --since="$since" 2>/dev/null || true)"

    # The LAST match wins -- see the header. grep -o emits one URL per line in
    # log order, so tail -1 is the most recent.
    link="$(printf '%s\n' "$logs" \
        | grep -oE 'https?://[^[:space:]"]+/auth/complete\?token=[A-Za-z0-9._-]+' \
        | tail -1 || true)"

    if [[ -z "$link" ]]; then
        cap_result_set     ownerEmail "$email"
        cap_result_set     link       ""
        cap_result_set_raw found      false
        cap_fail 4 "no magic link found in the last ${since} of ${deploy} logs; the identity service may still be starting"
    fi

    cap_info "recovered a sign-in link for ${email}"
    cap_result_set     ownerEmail "$email"
    cap_result_set     link       "$link"
    cap_result_set_raw found      true
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/magic-link.sh
go test ./scripts/install/ -run TestMagicLink -v
go test ./scripts/lib/ -run TestCapabilityScripts -v
```
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/magic-link.sh scripts/install/magic_link_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.magicLink for the owner's first sign-in

A local cluster has no mail (integrations/email falls back to LogSender),
so "check your email" is a dead end on exactly the install the wizard
performs. The link is already printed; this reads it back.

--local is mandatory: kubectl points at whatever context was last used,
so pulling an auth credential from logs must be an explicit local
decision made at the call site. The LAST link wins -- magic links are
single-use and a restarted pod leaves spent ones in the log.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: `install.removeArtifact` — every uninstall reversal

**Files:**
- Create: `scripts/install/remove-artifact.sh`
- Create: `scripts/install/remove_artifact_test.go`

**Interfaces:**
- Produces: capability id `install.removeArtifact`. Params: `--kind=binary|hostsEntries|mkcertCA|stack|images`, `--target`, `--pre-existing=true|false`, `--confirm`, `--dry-run`, plus `--mkcert-bin` / `--docker-bin` / `--hosts-file` for the kind-specific backends. Result: `{"kind":string,"target":string,"removed":bool,"skippedPreExisting":bool}`.
- **`--pre-existing=true` is an unconditional refusal (exit 3).** This is the single rule that makes uninstall safe: the receipt records what pre-existed, and this script will not remove it no matter what the caller asks.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/remove_artifact_test.go`:

```go
package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type removeResult struct {
	Kind               string `json:"kind"`
	Target             string `json:"target"`
	Removed            bool   `json:"removed"`
	SkippedPreExisting bool   `json:"skippedPreExisting"`
}

const removeConfirm = "remove memql artifacts"

// TestRemoveArtifactRefusesPreExisting is the most important test in the
// uninstall path. The receipt records that a tool was already on the machine;
// this script must refuse to remove it even when explicitly asked.
func TestRemoveArtifactRefusesPreExisting(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/remove-artifact.sh"
	f := filepath.Join(t.TempDir(), "k3d")
	if err := os.WriteFile(f, []byte("binary"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	env, out := runScript(t, script,
		"--kind=binary", "--target="+f, "--pre-existing=true", "--confirm="+removeConfirm)
	if env.OK {
		t.Fatalf("removed a pre-existing artifact; output:\n%s", out)
	}
	if env.Error == nil || env.Error.Code != 3 {
		t.Errorf("want error.code 3 (refused); got %+v", env.Error)
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("the pre-existing file was deleted anyway: %v", err)
	}
}

// TestRemoveArtifactRemovesOurOwnBinary covers the normal path.
func TestRemoveArtifactRemovesOurOwnBinary(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/remove-artifact.sh"
	f := filepath.Join(t.TempDir(), "k3d")
	if err := os.WriteFile(f, []byte("binary"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	env, out := runScript(t, script,
		"--kind=binary", "--target="+f, "--pre-existing=false", "--confirm="+removeConfirm)
	if !env.OK {
		t.Fatalf("remove failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	var r removeResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the remove schema: %v", err)
	}
	if !r.Removed || r.SkippedPreExisting {
		t.Errorf("unexpected result: %+v", r)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Errorf("file still present after removal")
	}
}

// TestRemoveArtifactIsIdempotent: removing something already gone is a
// successful no-op, so a re-run of uninstall does not fail halfway through.
func TestRemoveArtifactIsIdempotent(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/remove-artifact.sh"
	f := filepath.Join(t.TempDir(), "absent")
	env, out := runScript(t, script,
		"--kind=binary", "--target="+f, "--pre-existing=false", "--confirm="+removeConfirm)
	if !env.OK {
		t.Fatalf("removing an absent target failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	if env.Changed {
		t.Errorf("changed = true when there was nothing to remove")
	}
}

// TestRemoveArtifactRequiresConfirmation locks the phrase guard.
func TestRemoveArtifactRequiresConfirmation(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/remove-artifact.sh"
	f := filepath.Join(t.TempDir(), "k3d")
	if err := os.WriteFile(f, []byte("binary"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	env, _ := runScript(t, script, "--kind=binary", "--target="+f, "--pre-existing=false")
	if env.OK {
		t.Fatal("removed without confirmation")
	}
	if env.Error == nil || env.Error.Code != 3 {
		t.Errorf("want error.code 3 (refused); got %+v", env.Error)
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("file removed despite the refusal: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestRemoveArtifact -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/remove-artifact.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/remove-artifact.sh
# ==================================
#
# Capability: install.removeArtifact -- reverse one thing the installer created.
#
# ONE SCRIPT, ONE RULE: --pre-existing=true IS AN UNCONDITIONAL REFUSAL. The
# receipt records, per artifact, whether it was already on the machine before
# the install. This script refuses to remove anything so marked, even when the
# caller explicitly asks -- so a bug in the executor, a hand-edited receipt, or
# an operator running it directly all hit the same wall. Removing a tool we did
# not install is the single worst thing an uninstaller can do, and it is worth
# enforcing at the point of action rather than trusting every caller.
#
# IDEMPOTENT: removing something already gone is a successful no-op with
# changed=false, so re-running uninstall after a partial run works.
#
# The kinds map to the artifacts the install graph creates:
#   binary        a tool binary in ~/.memql/bin
#   hostsEntries  the delimited /etc/hosts block (delegates to hosts-entries.sh)
#   mkcertCA      the mkcert root CA in the system trust store
#   stack         the cloned repository checkout
#   images        the cluster's imported container images
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/remove-artifact.sh --kind=binary --target=~/.memql/bin/k3d \
#       --pre-existing=false --confirm='remove memql artifacts'
#   scripts/install/remove-artifact.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param (unknown kind) | 3 refused (pre-existing, or
#             no confirmation) | 5 removal failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.removeArtifact" "Reverse one artifact the memQL installer created."
cap_spec_param "kind"         "binary | hostsEntries | mkcertCA | stack | images"
cap_spec_param "target"       "path or identifier of the artifact"
cap_spec_param "pre-existing" "true when the artifact predates the install (refuses removal)"
cap_spec_param "confirm"      "confirmation phrase: remove memql artifacts"
cap_spec_param "hosts-file"   "hosts file path (kind=hostsEntries)"
cap_spec_param "domain"       "front-door domain (kind=hostsEntries)"
cap_spec_param "mkcert-bin"   "path to the mkcert binary (kind=mkcertCA)"
cap_spec_param "docker-bin"   "path to the docker binary (kind=images)"
cap_spec_param "dry-run"      "report what would happen without removing (flag)"

readonly CONFIRM_PHRASE="remove memql artifacts"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local kind target pre confirm dry
    kind="$(cap_param kind "")"
    target="$(cap_param target "")"
    pre="$(cap_param pre-existing "false")"
    confirm="$(cap_param confirm "")"
    dry="$(cap_flag dry-run)"
    cap_require kind "$kind"

    case "$kind" in
        binary|hostsEntries|mkcertCA|stack|images) ;;
        *) cap_fail 2 "unknown kind '${kind}': expected binary, hostsEntries, mkcertCA, stack or images" ;;
    esac

    cap_result_set kind   "$kind"
    cap_result_set target "$target"

    # The rule. Checked before the confirmation so the refusal names the real
    # reason rather than sending the caller off to fix a phrase.
    if [[ "$pre" == "true" ]]; then
        cap_result_set_raw removed            false
        cap_result_set_raw skippedPreExisting true
        cap_fail 3 "refusing to remove '${target}': the receipt records it as pre-existing, so memQL did not install it"
    fi

    if [[ -n "$dry" ]]; then
        cap_info "DRY RUN: would remove ${kind} at ${target}"
        cap_result_set_raw removed            false
        cap_result_set_raw skippedPreExisting false
        cap_ok
    fi

    cap_confirm_or_die "$confirm" "$CONFIRM_PHRASE"

    local removed=false
    case "$kind" in
        binary|stack)
            if [[ -e "$target" ]]; then
                rm -rf "$target" || cap_fail 5 "could not remove ${target}"
                removed=true
                cap_changed
            else
                cap_info "${target} is already gone"
            fi
            ;;
        hostsEntries)
            local hosts_file domain
            hosts_file="$(cap_param hosts-file "/etc/hosts")"
            domain="$(cap_param domain "memql.localhost")"
            local out
            if ! out="$("${SCRIPT_DIR}/hosts-entries.sh" --mode=remove \
                    --hosts-file="$hosts_file" --domain="$domain" \
                    --confirm='edit my hosts file' 2>/dev/null)"; then
                cap_fail 5 "hosts block removal failed for ${hosts_file}"
            fi
            if printf '%s' "$out" | grep -q '"applied":true'; then
                removed=true
                cap_changed
            fi
            ;;
        mkcertCA)
            local mkcert_bin
            mkcert_bin="$(cap_param mkcert-bin "${HOME}/.memql/bin/mkcert")"
            [[ -x "$mkcert_bin" ]] || mkcert_bin="$(command -v mkcert || true)"
            if [[ -n "$mkcert_bin" && -x "$mkcert_bin" ]]; then
                "$mkcert_bin" -uninstall >&2 || cap_fail 5 "mkcert -uninstall failed"
                removed=true
                cap_changed
            else
                cap_info "mkcert not available; the CA cannot be uninstalled here"
            fi
            ;;
        images)
            local docker_bin
            docker_bin="$(cap_param docker-bin "docker")"
            if command -v "$docker_bin" &>/dev/null; then
                # target is an image reference filter, e.g. "memql/".
                local ids
                ids="$("$docker_bin" images --format '{{.Repository}}:{{.Tag}} {{.ID}}' 2>/dev/null \
                    | awk -v f="$target" 'index($1, f) == 1 {print $2}' | sort -u || true)"
                if [[ -n "$ids" ]]; then
                    # shellcheck disable=SC2086
                    "$docker_bin" rmi -f $ids >&2 || cap_fail 5 "docker rmi failed for ${target}"
                    removed=true
                    cap_changed
                else
                    cap_info "no images matching ${target}"
                fi
            else
                cap_info "docker not available; no images to remove"
            fi
            ;;
    esac

    cap_result_set_raw removed            "$removed"
    cap_result_set_raw skippedPreExisting false
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/remove-artifact.sh
go test ./scripts/install/ -v
go test ./scripts/lib/ -v
```
Expected: PASS. The full `scripts/lib` run now covers all eight new capability scripts against the contract gate.

- [ ] **Step 5: Commit**

```bash
git add scripts/install/remove-artifact.sh scripts/install/remove_artifact_test.go
git commit -m "$(cat <<'EOF'
feat(install): add install.removeArtifact with a pre-existing refusal

One script for every uninstall reversal, with one rule enforced at the
point of action: --pre-existing=true is an unconditional exit 3. A bug in
the executor, a hand-edited receipt, or a direct invocation all hit the
same wall, because removing a tool we did not install is the worst thing
an uninstaller can do.

Idempotent: removing something already gone is a no-op with
changed=false, so re-running uninstall after a partial run works.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Register the install scripts in the capability allowlist

**Files:**
- Modify: `component/automations/steps/capability_script.go:61-80` (the `capabilityScriptAllowlist` map)
- Create: `component/automations/steps/install_allowlist_test.go`

**Interfaces:**
- Consumes: the eight capability ids from Tasks 1–10.
- Produces: those ids resolvable by the in-engine action executor, which is what makes Phase 5 possible without touching any script.

The allowlist is the security boundary (see the file's header comment): a shell action can only run a script listed here. The test walks `scripts/install/` and fails on any script whose `cap_init` id is unregistered, so adding a ninth script later cannot silently be unreachable.

- [ ] **Step 1: Write the failing test**

Create `component/automations/steps/install_allowlist_test.go`:

```go
package steps

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestInstallScriptsAreAllowlisted walks scripts/install/ and asserts every
// capability script's cap_init id is registered. The allowlist is the security
// boundary AND the reachability boundary: an unregistered script cannot be
// invoked by an action, so a new installer step would silently do nothing in
// the in-engine path while working fine from the host executor.
func TestInstallScriptsAreAllowlisted(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	dir := filepath.Join(root, "scripts", "install")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read scripts/install: %v", err)
	}
	reInit := regexp.MustCompile(`(?m)^\s*cap_init\s+"([^"]+)"`)

	found := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sh" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		m := reInit.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s has no cap_init id", e.Name())
			continue
		}
		id := m[1]
		found++

		rel, ok := capabilityScriptAllowlist[id]
		if !ok {
			t.Errorf("capability %q (%s) is not in capabilityScriptAllowlist -- "+
				"an action naming it would be refused", id, e.Name())
			continue
		}
		want := filepath.Join("scripts", "install", e.Name())
		if rel != want {
			t.Errorf("capability %q maps to %q, want %q", id, rel, want)
		}
	}
	if found == 0 {
		t.Fatal("no capability scripts found under scripts/install -- the walk is broken")
	}
}

// TestAllowlistPathsExist guards the other direction: every registered path
// must be a real file, so a rename cannot leave a dangling entry that fails
// only at run time.
func TestAllowlistPathsExist(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	for id, rel := range capabilityScriptAllowlist {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("allowlist entry %q -> %q does not exist: %v", id, rel, err)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./component/automations/steps/ -run 'TestInstallScriptsAreAllowlisted|TestAllowlistPathsExist' -v`
Expected: FAIL — eight `capability "install.X" is not in capabilityScriptAllowlist` errors.

- [ ] **Step 3: Register the scripts**

In `component/automations/steps/capability_script.go`, add a new block to `capabilityScriptAllowlist` immediately after the deploy-pack entries (keep the existing entries untouched):

```go
	// Local install/uninstall substrate (Epic 1). Each is an I14 capability
	// script under scripts/install/; the host executor and the in-engine action
	// executor invoke the SAME ids, which is what lets the install replay
	// in-engine in Phase 5 without touching a script.
	"install.refreshToolPins":  "scripts/install/refresh-tool-pins.sh",
	"install.detect":           "scripts/install/detect.sh",
	"install.binary":           "scripts/install/install-binary.sh",
	"install.hostsEntries":     "scripts/install/hosts-entries.sh",
	"install.mkcert":           "scripts/install/mkcert-setup.sh",
	"install.cloneStack":       "scripts/install/clone-stack.sh",
	"install.verifyProviderKey": "scripts/install/verify-provider-key.sh",
	"install.verifyFrontDoor":  "scripts/install/verify-frontdoor.sh",
	"install.magicLink":        "scripts/install/magic-link.sh",
	"install.removeArtifact":   "scripts/install/remove-artifact.sh",
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w component/automations/steps/capability_script.go && go test ./component/automations/steps/ -run 'TestInstallScripts|TestAllowlistPaths' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add component/automations/steps/capability_script.go component/automations/steps/install_allowlist_test.go
git commit -m "$(cat <<'EOF'
feat(install): register the install capability scripts in the allowlist

The allowlist is both the security boundary and the reachability
boundary: an unregistered script cannot be invoked by an action, so a new
installer step would silently do nothing in the in-engine path while
working fine from the host executor. A test walks scripts/install/ so a
future script cannot be forgotten.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 12: The graph documents and their loader

**Files:**
- Create: `scripts/install/graph/graph.go`
- Create: `scripts/install/graph/install.json`
- Create: `scripts/install/graph/uninstall.json`
- Create: `scripts/install/graph/loader_test.go`

**Interfaces:**
- Produces (Go, package `graph`, import path `github.com/znasllc-io/memql/scripts/install/graph`):
  - `type Verify struct { Kind string; Path string; Value string }`
  - `type Receipt struct { Kind string; TargetPath string; PreExistingPath string }`
  - `type Step struct { ID, Title, Script string; Params map[string]string; DependsOn []string; Verify Verify; Elevation string; ReadOnly bool; Receipt *Receipt; Reverses string }`
  - `type Graph struct { Name string; Steps []Step }`
  - `func Load(path string) (Graph, error)` — parses and structurally validates.
  - `func (g Graph) ByID(id string) (Step, bool)`
  - `func (g Graph) TopoOrder() ([][]string, error)` — returns **waves**: each wave is a set of step ids with no unmet dependency, so the executor runs a wave concurrently.
- Consumed by: Task 13 (validation), Task 15 (the executor loads the same JSON through a TypeScript port of this schema).

Verify kinds: `resultTrue` / `resultFalse` (a boolean at `Path`), `resultNonEmpty` (a non-empty string at `Path`), `scriptOk` (the envelope's `ok`). **`scriptOk` is legal only when `ReadOnly` is true** — for a mutating step it would mean "the command exited 0", which is exactly the weak signal the spec rejects as an advance condition.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/graph/loader_test.go`:

```go
package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGraph(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestLoadRejectsAStepWithoutVerify is the structural rule the whole design
// rests on: verify -- not a zero exit code -- is what advances the wizard, so a
// step that declares none cannot be part of a graph.
func TestLoadRejectsAStepWithoutVerify(t *testing.T) {
	p := writeGraph(t, `{"name":"x","steps":[
      {"id":"a","title":"A","script":"install.detect","elevation":"none"}
    ]}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("loaded a step with no verify")
	}
	if !strings.Contains(err.Error(), "verify") {
		t.Errorf("error should name the missing verify; got %v", err)
	}
}

// TestLoadRejectsScriptOkOnAMutatingStep locks the weak-signal rule: exit 0 is
// not evidence a mutation took effect.
func TestLoadRejectsScriptOkOnAMutatingStep(t *testing.T) {
	p := writeGraph(t, `{"name":"x","steps":[
      {"id":"a","title":"A","script":"install.binary","elevation":"none",
       "verify":{"kind":"scriptOk"}}
    ]}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("accepted scriptOk on a mutating step")
	}
	if !strings.Contains(err.Error(), "readOnly") {
		t.Errorf("error should explain the readOnly requirement; got %v", err)
	}
}

// TestLoadRejectsADanglingDependency: a typo in dependsOn must fail at load,
// not strand a step forever at run time.
func TestLoadRejectsADanglingDependency(t *testing.T) {
	p := writeGraph(t, `{"name":"x","steps":[
      {"id":"a","title":"A","script":"install.detect","elevation":"none","readOnly":true,
       "verify":{"kind":"scriptOk"},"dependsOn":["nope"]}
    ]}`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want a dangling-dependency error naming \"nope\"; got %v", err)
	}
}

// TestLoadRejectsDuplicateIDs -- ids are how the receipt and the executor
// address a step; two steps sharing one makes both unaddressable.
func TestLoadRejectsDuplicateIDs(t *testing.T) {
	p := writeGraph(t, `{"name":"x","steps":[
      {"id":"a","title":"A","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"}},
      {"id":"a","title":"B","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"}}
    ]}`)
	if _, err := Load(p); err == nil {
		t.Fatal("accepted duplicate step ids")
	}
}

// TestTopoOrderGroupsIndependentStepsIntoOneWave is what makes parallelism a
// property of the graph rather than a decision in the UI.
func TestTopoOrderGroupsIndependentStepsIntoOneWave(t *testing.T) {
	p := writeGraph(t, `{"name":"x","steps":[
      {"id":"root","title":"R","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"}},
      {"id":"a","title":"A","script":"install.binary","elevation":"none","dependsOn":["root"],
       "verify":{"kind":"resultNonEmpty","path":"path"}},
      {"id":"b","title":"B","script":"install.binary","elevation":"none","dependsOn":["root"],
       "verify":{"kind":"resultNonEmpty","path":"path"}}
    ]}`)
	g, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	waves, err := g.TopoOrder()
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	if len(waves) != 2 {
		t.Fatalf("got %d waves, want 2", len(waves))
	}
	if len(waves[0]) != 1 || waves[0][0] != "root" {
		t.Errorf("wave 0 = %v, want [root]", waves[0])
	}
	if len(waves[1]) != 2 {
		t.Errorf("wave 1 = %v, want a and b together (they are independent)", waves[1])
	}
}

// TestTopoOrderDetectsACycle.
func TestTopoOrderDetectsACycle(t *testing.T) {
	p := writeGraph(t, `{"name":"x","steps":[
      {"id":"a","title":"A","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"},"dependsOn":["b"]},
      {"id":"b","title":"B","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"},"dependsOn":["a"]}
    ]}`)
	g, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := g.TopoOrder(); err == nil {
		t.Fatal("no cycle reported for a <-> b")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/graph/ -v`
Expected: FAIL — `undefined: Load` (the package does not exist).

- [ ] **Step 3: Write the loader**

Create `scripts/install/graph/graph.go`:

```go
// Package graph loads and structurally validates the install/uninstall step
// graphs.
//
// THE GRAPH IS THE ONLY PLACE DECISIONS LIVE. Step order, what may run
// concurrently, which steps need elevation, how a step is known to have
// worked, and what uninstall must reverse are all data here -- never logic in
// a front end. Two executors read this same document: the host executor
// (pre-cluster, TypeScript) and the engine's action executor (post-cluster).
//
// VERIFY IS MANDATORY BECAUSE VERIFY IS WHAT ADVANCES THE WIZARD. A zero exit
// code means a command ran, not that the world changed; a step that cannot
// state how to check itself cannot be resumed, cannot be run in Guided mode
// (where a human runs the command and we poll), and cannot be trusted after a
// crash. So Load refuses one.
package graph

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Verify kinds. scriptOk is the weakest and is legal only on a read-only step.
const (
	VerifyResultTrue     = "resultTrue"
	VerifyResultFalse    = "resultFalse"
	VerifyResultNonEmpty = "resultNonEmpty"
	VerifyScriptOK       = "scriptOk"
)

// Elevation values.
const (
	ElevationNone  = "none"
	ElevationAdmin = "admin"
)

// Verify says how a step is known to be satisfied. Path addresses a field of
// the capability result object (a flat key; nested access is deliberately not
// supported so a verify stays trivially evaluable in both executors).
type Verify struct {
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Value string `json:"value,omitempty"`
}

// Receipt names the result fields recorded for a step that creates something
// reversible. TargetPath is the result key holding what was created;
// PreExistingPath is the result key holding whether it predated the install.
//
// A step with a Receipt MUST have a matching reversal in the uninstall graph;
// that is asserted in graph_test.go and is what stops uninstall drifting behind
// install.
type Receipt struct {
	Kind            string `json:"kind"`
	TargetPath      string `json:"targetPath"`
	PreExistingPath string `json:"preExistingPath,omitempty"`
}

// Step is one node. Params are rendered to --name=value flags by the executor.
type Step struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Script    string            `json:"script"`
	Params    map[string]string `json:"params,omitempty"`
	DependsOn []string          `json:"dependsOn,omitempty"`
	Verify    *Verify           `json:"verify"`
	Elevation string            `json:"elevation"`
	ReadOnly  bool              `json:"readOnly,omitempty"`
	Receipt   *Receipt          `json:"receipt,omitempty"`
	// Reverses names the install step this uninstall step undoes. Empty on
	// install graphs.
	Reverses string `json:"reverses,omitempty"`
}

// Graph is a whole document.
type Graph struct {
	Name  string `json:"name"`
	Steps []Step `json:"steps"`
}

// Load reads and validates a graph document.
func Load(path string) (Graph, error) {
	var g Graph
	b, err := os.ReadFile(path)
	if err != nil {
		return g, fmt.Errorf("read graph %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return g, fmt.Errorf("parse graph %s: %w", path, err)
	}
	if err := g.validate(); err != nil {
		return g, fmt.Errorf("graph %s: %w", path, err)
	}
	return g, nil
}

func (g Graph) validate() error {
	if len(g.Steps) == 0 {
		return fmt.Errorf("has no steps")
	}
	seen := map[string]bool{}
	for _, s := range g.Steps {
		if s.ID == "" {
			return fmt.Errorf("a step has an empty id")
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate step id %q -- ids address a step in the receipt and the executor", s.ID)
		}
		seen[s.ID] = true

		if s.Title == "" {
			return fmt.Errorf("step %q has no title (the wizard renders it)", s.ID)
		}
		if s.Script == "" {
			return fmt.Errorf("step %q names no capability script", s.ID)
		}
		switch s.Elevation {
		case ElevationNone, ElevationAdmin:
		default:
			return fmt.Errorf("step %q has elevation %q, want none or admin", s.ID, s.Elevation)
		}
		if s.Verify == nil {
			return fmt.Errorf("step %q declares no verify -- verify, not a zero exit code, is what advances the wizard", s.ID)
		}
		switch s.Verify.Kind {
		case VerifyResultTrue, VerifyResultFalse, VerifyResultNonEmpty:
			if s.Verify.Path == "" {
				return fmt.Errorf("step %q verify kind %q needs a path", s.ID, s.Verify.Kind)
			}
		case VerifyScriptOK:
			if !s.ReadOnly {
				return fmt.Errorf("step %q uses verify kind scriptOk but is not readOnly -- "+
					"for a mutating step that only proves the command exited 0, not that anything changed", s.ID)
			}
		default:
			return fmt.Errorf("step %q has unknown verify kind %q", s.ID, s.Verify.Kind)
		}
		if s.Receipt != nil {
			if s.Receipt.Kind == "" || s.Receipt.TargetPath == "" {
				return fmt.Errorf("step %q receipt needs both kind and targetPath", s.ID)
			}
		}
	}
	for _, s := range g.Steps {
		for _, d := range s.DependsOn {
			if !seen[d] {
				return fmt.Errorf("step %q depends on %q, which does not exist", s.ID, d)
			}
		}
	}
	return nil
}

// ByID returns the step with the given id.
func (g Graph) ByID(id string) (Step, bool) {
	for _, s := range g.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

// TopoOrder returns execution WAVES: every id in a wave has all its
// dependencies satisfied by earlier waves, so a wave runs concurrently. That
// is how "some of these can be installed in parallel" becomes a property of
// the data rather than a decision in the executor.
func (g Graph) TopoOrder() ([][]string, error) {
	pending := map[string][]string{}
	for _, s := range g.Steps {
		deps := append([]string(nil), s.DependsOn...)
		pending[s.ID] = deps
	}
	done := map[string]bool{}
	var waves [][]string

	for len(done) < len(pending) {
		var wave []string
		for id, deps := range pending {
			if done[id] {
				continue
			}
			ready := true
			for _, d := range deps {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, id)
			}
		}
		if len(wave) == 0 {
			var stuck []string
			for id := range pending {
				if !done[id] {
					stuck = append(stuck, id)
				}
			}
			sort.Strings(stuck)
			return nil, fmt.Errorf("dependency cycle among steps %v", stuck)
		}
		sort.Strings(wave)
		for _, id := range wave {
			done[id] = true
		}
		waves = append(waves, wave)
	}
	return waves, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./scripts/install/graph/ -v`
Expected: PASS (all six loader tests).

- [ ] **Step 5: Write the install graph**

Create `scripts/install/graph/install.json`:

```json
{
  "name": "install",
  "steps": [
    {
      "id": "detect",
      "title": "Detect dependencies",
      "script": "install.detect",
      "elevation": "none",
      "readOnly": true,
      "verify": { "kind": "scriptOk" }
    },
    {
      "id": "verifyProviderKey",
      "title": "Verify the AI provider key",
      "script": "install.verifyProviderKey",
      "dependsOn": ["detect"],
      "elevation": "none",
      "readOnly": true,
      "verify": { "kind": "resultTrue", "path": "reachable" }
    },
    {
      "id": "binaryK3d",
      "title": "Install k3d",
      "script": "install.binary",
      "params": { "tool": "k3d" },
      "dependsOn": ["detect"],
      "elevation": "none",
      "verify": { "kind": "resultNonEmpty", "path": "path" },
      "receipt": { "kind": "binary", "targetPath": "path", "preExistingPath": "preExisting" }
    },
    {
      "id": "binaryKubectl",
      "title": "Install kubectl",
      "script": "install.binary",
      "params": { "tool": "kubectl" },
      "dependsOn": ["detect"],
      "elevation": "none",
      "verify": { "kind": "resultNonEmpty", "path": "path" },
      "receipt": { "kind": "binary", "targetPath": "path", "preExistingPath": "preExisting" }
    },
    {
      "id": "binaryMkcert",
      "title": "Install mkcert",
      "script": "install.binary",
      "params": { "tool": "mkcert" },
      "dependsOn": ["detect"],
      "elevation": "none",
      "verify": { "kind": "resultNonEmpty", "path": "path" },
      "receipt": { "kind": "binary", "targetPath": "path", "preExistingPath": "preExisting" }
    },
    {
      "id": "hostsEntries",
      "title": "Add the front-door hostnames",
      "script": "install.hostsEntries",
      "params": { "mode": "add", "confirm": "edit my hosts file" },
      "dependsOn": ["detect"],
      "elevation": "admin",
      "verify": { "kind": "resultTrue", "path": "applied" },
      "receipt": { "kind": "hostsEntries", "targetPath": "file" }
    },
    {
      "id": "mkcertCA",
      "title": "Trust the local certificate authority",
      "script": "install.mkcert",
      "params": { "install-ca": "1" },
      "dependsOn": ["binaryMkcert"],
      "elevation": "admin",
      "verify": { "kind": "resultTrue", "path": "certIssued" },
      "receipt": { "kind": "mkcertCA", "targetPath": "caRoot", "preExistingPath": "caPreExisting" }
    },
    {
      "id": "cloneStack",
      "title": "Fetch the memQL stack",
      "script": "install.cloneStack",
      "dependsOn": ["detect"],
      "elevation": "none",
      "verify": { "kind": "resultNonEmpty", "path": "commit" },
      "receipt": { "kind": "stack", "targetPath": "dest" }
    },
    {
      "id": "clusterUp",
      "title": "Bring up the local cluster",
      "script": "k3d.up",
      "dependsOn": ["binaryK3d", "binaryKubectl", "hostsEntries", "mkcertCA", "cloneStack", "verifyProviderKey"],
      "elevation": "none",
      "verify": { "kind": "resultTrue", "path": "argocdReady" },
      "receipt": { "kind": "images", "targetPath": "cluster" }
    },
    {
      "id": "verifyFrontDoor",
      "title": "Verify the front door",
      "script": "install.verifyFrontDoor",
      "dependsOn": ["clusterUp"],
      "elevation": "none",
      "verify": { "kind": "resultTrue", "path": "allPassed" }
    },
    {
      "id": "magicLink",
      "title": "Recover the owner sign-in link",
      "script": "install.magicLink",
      "params": { "local": "1" },
      "dependsOn": ["verifyFrontDoor"],
      "elevation": "none",
      "verify": { "kind": "resultTrue", "path": "found" }
    }
  ]
}
```

`clusterUp`'s verify reads `argocdReady`, which `scripts/k3d/up.sh:547` already emits via `cap_result_set_raw` — no change to that shared script is needed.

`verifyFrontDoor`'s verify reads `allPassed`, a boolean. `passedCount` is a number and `resultTrue` compares against `true`, so verifying on the count would never be satisfied. Task 8's script must therefore also emit `allPassed`; add it there rather than adding a numeric verify kind, because "did every check pass" is the question the graph is actually asking:

```bash
    cap_result_set_raw allPassed   "$( [[ "$FAILED" -eq 0 ]] && echo true || echo false )"
```

placed alongside the existing `passedCount` / `failedCount` lines, and mirrored in Task 8's `frontDoorResult` test struct (`AllPassed bool \`json:"allPassed"\``).

- [ ] **Step 6: Write the uninstall graph**

Create `scripts/install/graph/uninstall.json`:

```json
{
  "name": "uninstall",
  "steps": [
    {
      "id": "clusterDown",
      "title": "Delete the local cluster",
      "script": "k3d.down",
      "reverses": "clusterUp",
      "elevation": "none",
      "verify": { "kind": "resultTrue", "path": "deleted" }
    },
    {
      "id": "removeStack",
      "title": "Remove the cloned stack",
      "script": "install.removeArtifact",
      "params": { "kind": "stack", "confirm": "remove memql artifacts" },
      "reverses": "cloneStack",
      "dependsOn": ["clusterDown"],
      "elevation": "none",
      "verify": { "kind": "resultFalse", "path": "skippedPreExisting" }
    },
    {
      "id": "removeMkcertCA",
      "title": "Remove the local certificate authority",
      "script": "install.removeArtifact",
      "params": { "kind": "mkcertCA", "confirm": "remove memql artifacts" },
      "reverses": "mkcertCA",
      "dependsOn": ["clusterDown"],
      "elevation": "admin",
      "verify": { "kind": "resultFalse", "path": "skippedPreExisting" }
    },
    {
      "id": "removeHostsEntries",
      "title": "Remove the front-door hostnames",
      "script": "install.removeArtifact",
      "params": { "kind": "hostsEntries", "confirm": "remove memql artifacts" },
      "reverses": "hostsEntries",
      "dependsOn": ["clusterDown"],
      "elevation": "admin",
      "verify": { "kind": "resultFalse", "path": "skippedPreExisting" }
    },
    {
      "id": "removeBinaryK3d",
      "title": "Remove k3d",
      "script": "install.removeArtifact",
      "params": { "kind": "binary", "confirm": "remove memql artifacts" },
      "reverses": "binaryK3d",
      "dependsOn": ["clusterDown"],
      "elevation": "none",
      "verify": { "kind": "resultFalse", "path": "skippedPreExisting" }
    },
    {
      "id": "removeBinaryKubectl",
      "title": "Remove kubectl",
      "script": "install.removeArtifact",
      "params": { "kind": "binary", "confirm": "remove memql artifacts" },
      "reverses": "binaryKubectl",
      "dependsOn": ["clusterDown"],
      "elevation": "none",
      "verify": { "kind": "resultFalse", "path": "skippedPreExisting" }
    },
    {
      "id": "removeBinaryMkcert",
      "title": "Remove mkcert",
      "script": "install.removeArtifact",
      "params": { "kind": "binary", "confirm": "remove memql artifacts" },
      "reverses": "binaryMkcert",
      "dependsOn": ["removeMkcertCA"],
      "elevation": "none",
      "verify": { "kind": "resultFalse", "path": "skippedPreExisting" }
    }
  ]
}
```

- [ ] **Step 7: Commit**

```bash
git add scripts/install/graph/graph.go scripts/install/graph/install.json scripts/install/graph/uninstall.json scripts/install/graph/loader_test.go
git commit -m "$(cat <<'EOF'
feat(install): add the install/uninstall step graphs and their loader

The graph is the only place decisions live -- order, concurrency,
elevation, how a step is known to have worked, and what uninstall
reverses are all data, never logic in a front end.

Load() refuses a step with no verify, because verify (not a zero exit
code) is what advances the wizard: a step that cannot state how to check
itself cannot be resumed, cannot run in Guided mode, and cannot be
trusted after a crash. scriptOk is legal only on a readOnly step.

TopoOrder returns WAVES, so "some of these can run in parallel" is a
property of the data rather than a decision in the executor.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 13: Graph validation — the anti-drift tests

**Files:**
- Create: `scripts/install/graph/graph_test.go`

**Interfaces:**
- Consumes: `Load`, `TopoOrder`, `Graph.Steps` (Task 12); `capabilityScriptAllowlist` is read indirectly by re-parsing the Go source, so this package does not import `component/automations/steps` (that would invert the dependency: a scripts-level test package must not pull in engine internals).

- [ ] **Step 1: Write the test**

Create `scripts/install/graph/graph_test.go`:

```go
package graph

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// scripts/install/graph -> ../../..
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

func loadBoth(t *testing.T) (Graph, Graph) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "scripts", "install", "graph")
	inst, err := Load(filepath.Join(dir, "install.json"))
	if err != nil {
		t.Fatalf("load install.json: %v", err)
	}
	un, err := Load(filepath.Join(dir, "uninstall.json"))
	if err != nil {
		t.Fatalf("load uninstall.json: %v", err)
	}
	return inst, un
}

// allowlistIDs parses capabilityScriptAllowlist out of the engine source rather
// than importing the package. The dependency must not point that way: this is a
// scripts-level test and pulling in engine internals to check a string set
// would couple the installer to the automation runtime.
func allowlistIDs(t *testing.T) map[string]bool {
	t.Helper()
	p := filepath.Join(repoRoot(t), "component", "automations", "steps", "capability_script.go")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read allowlist source: %v", err)
	}
	re := regexp.MustCompile(`"([a-zA-Z0-9._-]+)":\s*"scripts/[^"]+"`)
	ids := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		ids[m[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("parsed no allowlist entries -- the regex is broken")
	}
	return ids
}

// TestGraphsHaveNoCycles.
func TestGraphsHaveNoCycles(t *testing.T) {
	inst, un := loadBoth(t)
	for _, g := range []Graph{inst, un} {
		if _, err := g.TopoOrder(); err != nil {
			t.Errorf("graph %q: %v", g.Name, err)
		}
	}
}

// TestEveryScriptIsAllowlisted -- a step naming an unregistered script would be
// refused at run time by the in-engine executor.
func TestEveryScriptIsAllowlisted(t *testing.T) {
	ids := allowlistIDs(t)
	inst, un := loadBoth(t)
	for _, g := range []Graph{inst, un} {
		for _, s := range g.Steps {
			if !ids[s.Script] {
				t.Errorf("graph %q step %q names script %q, which is not in capabilityScriptAllowlist",
					g.Name, s.ID, s.Script)
			}
		}
	}
}

// TestEveryReceiptWritingStepHasAReversal is THE anti-drift test, and the
// reason install and uninstall are one epic. Every install step that creates
// something reversible must be named by a `reverses` field in the uninstall
// graph. Without this, uninstall silently falls behind install one step at a
// time -- which is exactly how uninstallers rot.
func TestEveryReceiptWritingStepHasAReversal(t *testing.T) {
	inst, un := loadBoth(t)

	reversed := map[string]string{}
	for _, s := range un.Steps {
		if s.Reverses == "" {
			t.Errorf("uninstall step %q names no install step in `reverses`", s.ID)
			continue
		}
		if prev, dup := reversed[s.Reverses]; dup {
			t.Errorf("install step %q is reversed by both %q and %q", s.Reverses, prev, s.ID)
		}
		reversed[s.Reverses] = s.ID
		if _, ok := inst.ByID(s.Reverses); !ok {
			t.Errorf("uninstall step %q reverses %q, which is not an install step", s.ID, s.Reverses)
		}
	}

	for _, s := range inst.Steps {
		if s.Receipt == nil {
			continue
		}
		if _, ok := reversed[s.ID]; !ok {
			t.Errorf("install step %q writes a receipt (kind %q) but nothing in the uninstall graph reverses it",
				s.ID, s.Receipt.Kind)
		}
	}
}

// TestAdminStepsAreDeclaredNotDiscovered: elevation is a property of the graph,
// so the wizard can warn BEFORE it starts rather than surprising the user with
// a password prompt nine minutes in.
func TestAdminStepsAreDeclaredNotDiscovered(t *testing.T) {
	inst, _ := loadBoth(t)
	wantAdmin := map[string]bool{"hostsEntries": true, "mkcertCA": true}
	for _, s := range inst.Steps {
		if wantAdmin[s.ID] && s.Elevation != ElevationAdmin {
			t.Errorf("install step %q must declare elevation=admin; it edits a system-wide resource", s.ID)
		}
		if !wantAdmin[s.ID] && s.Elevation == ElevationAdmin {
			t.Errorf("install step %q declares elevation=admin unexpectedly -- if that is correct, add it to wantAdmin here so the expectation is explicit", s.ID)
		}
	}
}

// TestReceiptStepsRecordPreExistenceWhereRemovalIsRisky: any artifact that
// could have predated the install must carry a preExistingPath, or uninstall
// has no way to know it must not remove it.
func TestReceiptStepsRecordPreExistenceWhereRemovalIsRisky(t *testing.T) {
	inst, _ := loadBoth(t)
	risky := map[string]bool{"binary": true, "mkcertCA": true}
	for _, s := range inst.Steps {
		if s.Receipt == nil || !risky[s.Receipt.Kind] {
			continue
		}
		if s.Receipt.PreExistingPath == "" {
			t.Errorf("install step %q creates a %q artifact but records no preExistingPath -- "+
				"uninstall could remove something the user installed themselves", s.ID, s.Receipt.Kind)
		}
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./scripts/install/graph/ -v`
Expected: PASS. If `TestEveryReceiptWritingStepHasAReversal` fails, add the missing uninstall step to `uninstall.json` — do **not** delete the receipt from the install step to make it pass.

- [ ] **Step 3: Commit**

```bash
git add scripts/install/graph/graph_test.go
git commit -m "$(cat <<'EOF'
test(install): assert uninstall cannot drift behind install

Every install step that writes a receipt must be named by a `reverses`
field in the uninstall graph. This is the test that only exists because
install and uninstall are in scope together -- split across epics it
could not be written, uninstall becomes a follow-up, and follow-ups slip.

Also asserts no cycles, every script is allowlisted, elevation is
declared rather than discovered, and any artifact that could have
pre-existed records a preExistingPath.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 14: The DSL surface and the agreement test

**Files:**
- Create: `dsl/install/concepts.memql`
- Create: `dsl/install/actions.memql`
- Create: `scripts/install/graph/dsl_agreement_test.go`

**Interfaces:**
- Produces: `v1:install:installRun` (a persisted install record for Phase 5 replay) and one `action` per capability script, in the shape `dsl/deployment/actions.memql` establishes: `use capabilities.shell.{ script }` plus a body of `args { ... }` and a single `capability script(script: "<id>", ...)`.
- The agreement test is the guard against two sources of truth: every `script` id in either graph must have a DSL action, and every DSL action in `dsl/install/` must be used by a graph.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/graph/dsl_agreement_test.go`:

```go
package graph

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// dslActionScriptIDs extracts the capability-script ids named by the actions in
// dsl/install/actions.memql.
func dslActionScriptIDs(t *testing.T) map[string]bool {
	t.Helper()
	p := filepath.Join(repoRoot(t), "dsl", "install", "actions.memql")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read dsl/install/actions.memql: %v", err)
	}
	re := regexp.MustCompile(`capability\s+script\(\s*script:\s*"([^"]+)"`)
	ids := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		ids[m[1]] = true
	}
	return ids
}

// graphScriptIDs collects every script id used by either graph.
func graphScriptIDs(t *testing.T) map[string]bool {
	t.Helper()
	inst, un := loadBoth(t)
	ids := map[string]bool{}
	for _, g := range []Graph{inst, un} {
		for _, s := range g.Steps {
			ids[s.Script] = true
		}
	}
	return ids
}

// TestGraphAndDSLAgree is the guard against two sources of truth. The graph is
// what the host executor walks pre-cluster; the DSL actions are what the engine
// executor will invoke post-cluster (Phase 5). If they drift, the same install
// means two different things depending on who ran it -- and nothing else in the
// build would notice.
//
// k3d.* ids are exempt: those scripts predate this epic and are already
// authored as actions in dsl/deployment/, not dsl/install/.
func TestGraphAndDSLAgree(t *testing.T) {
	dsl := dslActionScriptIDs(t)
	graphIDs := graphScriptIDs(t)

	isInstallID := regexp.MustCompile(`^install\.`)

	for id := range graphIDs {
		if !isInstallID.MatchString(id) {
			continue // k3d.up / k3d.down live in the deployment pack
		}
		if !dsl[id] {
			t.Errorf("graph uses capability %q but dsl/install/actions.memql has no action for it -- "+
				"the in-engine path (Phase 5) could not run this step", id)
		}
	}
	for id := range dsl {
		if !graphIDs[id] {
			t.Errorf("dsl/install/actions.memql declares an action for %q, which no graph step uses -- "+
				"dead DSL, or a graph step that was dropped", id)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/graph/ -run TestGraphAndDSLAgree -v`
Expected: FAIL — `read dsl/install/actions.memql: no such file or directory`.

- [ ] **Step 3: Write the concepts**

Create `dsl/install/concepts.memql`:

```memql
// concepts.memql
//
// The `install` namespace -- the persisted record of a local-cluster install.
//
// WHY THIS EXISTS BEFORE ANYTHING WRITES IT. The pre-cluster executor cannot
// write to the graph: it runs when there is no engine and no Postgres, which
// is the whole reason the receipt file exists. On the cluster's first healthy
// boot the receipt is REPLAYED into these rows, so the install becomes a
// first-class graph record retroactively and post-cluster operations
// (uninstall, repair, upgrade) can read what the install actually did.
//
// Authorization (owned tier): ownerUserId is the per-row authz key.

use identity.concepts.{ user }

/// One local-cluster install run, replayed into the graph from the install receipt on the
/// cluster's first healthy boot. Append-only: each status transition appends a new payload
/// version under the same id, so the run is reconstructable asOf any time.
@displayCard(primary="graphName", secondary="status", tertiary="ownerUserId", status="status")
@rowAuthz(owner="ownerUserId")
concept installRun {
  ownerUserId  string!  @description("v1:identity:user.id of the owner. The per-row authz key (owned tier), stamped from actor.userId by the replay mutation.")
  graphName    string!  @description("Which graph produced this run: install or uninstall.")
  status       enum("running", "succeeded", "failed", "cancelled")!  @default("running")  @description("Terminal state of the run. A cancelled run is still fully reversible -- the receipt is appended per step, not written at the end.")
  mode         enum("automatic", "guided")!  @default("automatic")  @description("Which execution mode ran the graph. The step list is identical; only who executed each step differs.")
  domain       string  @description("Front-door domain the cluster was installed under (e.g. memql.localhost or local.znas.io).")
  stackRef     string  @description("Release tag the stack was pinned to.")
  stackCommit  string  @description("Resolved commit of the pinned tag -- what was actually installed.")
  steps        []object!  @description("Ordered step records replayed from the receipt. Each entry: {id, script, status, changed, startedAt, endedAt, receipt}. receipt is null for a step that created nothing reversible.")
  startedAt    string  @description("RFC3339 timestamp of the first step.")
  endedAt      string  @description("RFC3339 timestamp of the terminal transition.")

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
}
```

- [ ] **Step 4: Write the actions**

Create `dsl/install/actions.memql`:

```memql
// actions.memql
//
// The in-engine callable form of every local-install capability.
//
// ACTIONS TOUCH THE WORLD; the graph orchestrates. Each action below is a
// single external capability call rendered from typed args -- it calls no
// logic, query or mutation -- exactly as dsl/deployment/actions.memql
// establishes for the deploy pack.
//
// THE GRAPH IS THE OTHER HALF, AND THEY ARE KEPT IN AGREEMENT MECHANICALLY.
// scripts/install/graph/{install,uninstall}.json is what the HOST executor
// walks before a cluster exists; these actions are what the ENGINE executor
// invokes afterwards (Phase 5). Two definitions of one install would mean the
// same install did different things depending on who ran it, so
// dsl_agreement_test.go asserts every graph script id has an action here and
// every action here is used by a graph.
//
// Every `script` value must also appear in capabilityScriptAllowlist
// (component/automations/steps/capability_script.go) -- the security boundary
// that stops an action naming an arbitrary path.

use capabilities.shell.{ script }

/// Inventory the machine -- OS, dependencies, ports, disk -- so the install graph can decide what
/// to do. Read-only: it installs nothing and elevates nothing.
action detectDependencies {
  args {
    workdir string
  }
  capability script(script: "install.detect", workdir: args.workdir)
}

/// Install one pinned, digest-verified tool binary (k3d, kubectl or mkcert) into the memQL bin
/// directory, leaving a tool the user already had exactly where it is.
action installToolBinary {
  args {
    tool   string!
    dest   string
    dryRun boolean
  }
  capability script(script: "install.binary", tool: args.tool, dest: args.dest, dryRun: args.dryRun)
}

/// Add or remove the memQL front-door hostnames as a delimited block in the hosts file, so removal
/// restores the file byte for byte.
action manageHostsEntries {
  args {
    mode      string!
    domain    string
    hostsFile string
    confirm   string!
    dryRun    boolean
  }
  capability script(script: "install.hostsEntries", mode: args.mode, domain: args.domain, hostsFile: args.hostsFile, confirm: args.confirm, dryRun: args.dryRun)
}

/// Ensure a trusted local certificate authority and issue the front-door wildcard certificate,
/// never re-installing a CA that was already in the trust store.
action ensureLocalCertificate {
  args {
    domain    string
    outDir    string
    installCa boolean
    dryRun    boolean
  }
  capability script(script: "install.mkcert", domain: args.domain, outDir: args.outDir, installCa: args.installCa, dryRun: args.dryRun)
}

/// Fetch the memQL stack at a pinned release tag, refusing a branch so two installs of the same
/// version are the same install.
action fetchStackAtTag {
  args {
    repoUrl string
    ref     string!
    dest    string
    dryRun  boolean
  }
  capability script(script: "install.cloneStack", repoUrl: args.repoUrl, ref: args.ref, dest: args.dest, dryRun: args.dryRun)
}

/// Verify an AI provider API key with one free authenticated call before the cluster is built
/// around it.
action verifyProviderKey {
  args {
    vendor  string!
    apiKey  string!
    baseUrl string
  }
  capability script(script: "install.verifyProviderKey", vendor: args.vendor, apiKey: args.apiKey, baseUrl: args.baseUrl)
}

/// Verify DNS, TLS trust and gRPC reachability for the front door, reporting each check separately
/// so a failure names what to fix.
action verifyFrontDoor {
  args {
    domain     string
    reportOnly boolean
    timeout    string
  }
  capability script(script: "install.verifyFrontDoor", domain: args.domain, reportOnly: args.reportOnly, timeout: args.timeout)
}

/// Recover the cluster owner's first sign-in link from the identity logs, because a local cluster
/// has no configured mail and the magic link is otherwise unreachable.
action recoverOwnerSignInLink {
  args {
    namespace  string
    deployment string
    ownerEmail string!
    local      boolean!
    since      string
  }
  capability script(script: "install.magicLink", namespace: args.namespace, deployment: args.deployment, ownerEmail: args.ownerEmail, local: args.local, since: args.since)
}

/// Reverse one artifact the installer created, refusing unconditionally when the receipt records it
/// as pre-existing.
action removeInstalledArtifact {
  args {
    kind        string!
    target      string
    preExisting string
    confirm     string!
    dryRun      boolean
  }
  capability script(script: "install.removeArtifact", kind: args.kind, target: args.target, preExisting: args.preExisting, confirm: args.confirm, dryRun: args.dryRun)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run:
```bash
go test ./scripts/install/graph/ -v
go test ./test/dslconformance/ -v
```
Expected: PASS. The DSL conformance suite must stay green — it hard-fails on an unclassified construct, so if it objects to `installRun`, fix the authorization classification rather than exempting the concept.

- [ ] **Step 6: Commit**

```bash
git add dsl/install/concepts.memql dsl/install/actions.memql scripts/install/graph/dsl_agreement_test.go
git commit -m "$(cat <<'EOF'
feat(install): add the install DSL surface with an agreement test

dsl/install/actions.memql is the in-engine callable form of every install
capability; the JSON graph is what the host executor walks before a
cluster exists. dsl_agreement_test.go asserts they name the same set of
capabilities in both directions, so the same install cannot come to mean
two different things depending on who ran it.

v1:install:installRun is the record the receipt replays into on the
cluster's first healthy boot, which is what lets Phase 5 run uninstall
and repair as genuine in-engine DSL.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 15: The graph reader and the receipt (TypeScript)

**Files:**
- Create: `editors/vscode/src/install/graph.ts`
- Create: `editors/vscode/src/install/receipt.ts`
- Create: `editors/vscode/test/installGraph.test.ts`
- Create: `editors/vscode/test/installReceipt.test.ts`

**Interfaces:**
- Produces:
  - `graph.ts`: `interface Verify { kind: string; path?: string; value?: string }`, `interface ReceiptSpec { kind: string; targetPath: string; preExistingPath?: string }`, `interface Step { id: string; title: string; script: string; params?: Record<string,string>; dependsOn?: string[]; verify: Verify; elevation: "none"|"admin"; readOnly?: boolean; receipt?: ReceiptSpec; reverses?: string }`, `interface Graph { name: string; steps: Step[] }`, `loadGraph(file: string): Promise<Graph>`, `topoWaves(g: Graph): string[][]`, `stepById(g: Graph, id: string): Step | undefined`, `evaluateVerify(v: Verify, result: Record<string, unknown>, ok: boolean): boolean`.
  - `receipt.ts`: `interface ReceiptEntry { stepId: string; script: string; kind: string; target: string; preExisting: boolean; at: string }`, `interface Receipt { version: 1; graph: string; domain?: string; entries: ReceiptEntry[] }`, `defaultReceiptPath(): string`, `appendEntry(file: string, e: ReceiptEntry): Promise<void>`, `readReceipt(file: string): Promise<Receipt>`, `reverseOrder(r: Receipt): ReceiptEntry[]`.
- `src/install/` imports nothing from `vscode` — it must run under bare `node --test` and, in Epic 2, behind a webview.

- [ ] **Step 1: Write the failing tests**

Create `editors/vscode/test/installGraph.test.ts`:

```ts
// The graph reader is a TypeScript port of scripts/install/graph/graph.go.
// Both executors walk the SAME documents, so these tests deliberately mirror
// the Go loader's rules -- a divergence here would mean the pre-cluster and
// post-cluster paths disagree about what the install is.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { evaluateVerify, loadGraph, stepById, topoWaves } from "../src/install/graph.js";

async function tempGraph(body: string): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-graph-"));
  const file = path.join(dir, "g.json");
  await fs.writeFile(file, body, "utf8");
  return file;
}

test("loadGraph rejects a step with no verify", async () => {
  const file = await tempGraph(
    `{"name":"x","steps":[{"id":"a","title":"A","script":"install.detect","elevation":"none"}]}`,
  );
  await assert.rejects(() => loadGraph(file), /verify/);
});

test("loadGraph rejects scriptOk on a mutating step", async () => {
  const file = await tempGraph(
    `{"name":"x","steps":[{"id":"a","title":"A","script":"install.binary","elevation":"none","verify":{"kind":"scriptOk"}}]}`,
  );
  await assert.rejects(() => loadGraph(file), /readOnly/);
});

test("loadGraph rejects a dangling dependency", async () => {
  const file = await tempGraph(
    `{"name":"x","steps":[{"id":"a","title":"A","script":"install.detect","elevation":"none","readOnly":true,` +
      `"verify":{"kind":"scriptOk"},"dependsOn":["nope"]}]}`,
  );
  await assert.rejects(() => loadGraph(file), /nope/);
});

test("topoWaves groups independent steps into one wave", async () => {
  const file = await tempGraph(
    `{"name":"x","steps":[` +
      `{"id":"root","title":"R","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"}},` +
      `{"id":"a","title":"A","script":"install.binary","elevation":"none","dependsOn":["root"],"verify":{"kind":"resultNonEmpty","path":"path"}},` +
      `{"id":"b","title":"B","script":"install.binary","elevation":"none","dependsOn":["root"],"verify":{"kind":"resultNonEmpty","path":"path"}}]}`,
  );
  const g = await loadGraph(file);
  const waves = topoWaves(g);
  assert.deepEqual(waves[0], ["root"]);
  assert.deepEqual(waves[1], ["a", "b"]);
  assert.equal(stepById(g, "a")?.title, "A");
});

test("topoWaves throws on a cycle", async () => {
  const file = await tempGraph(
    `{"name":"x","steps":[` +
      `{"id":"a","title":"A","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"},"dependsOn":["b"]},` +
      `{"id":"b","title":"B","script":"install.detect","elevation":"none","readOnly":true,"verify":{"kind":"scriptOk"},"dependsOn":["a"]}]}`,
  );
  const g = await loadGraph(file);
  assert.throws(() => topoWaves(g), /cycle/);
});

test("evaluateVerify checks the result, not the exit code", () => {
  // A script can exit 0 having done nothing. resultTrue must read the field.
  assert.equal(evaluateVerify({ kind: "resultTrue", path: "applied" }, { applied: true }, true), true);
  assert.equal(evaluateVerify({ kind: "resultTrue", path: "applied" }, { applied: false }, true), false);
  assert.equal(evaluateVerify({ kind: "resultFalse", path: "skipped" }, { skipped: false }, true), true);
  assert.equal(evaluateVerify({ kind: "resultNonEmpty", path: "path" }, { path: "" }, true), false);
  assert.equal(evaluateVerify({ kind: "resultNonEmpty", path: "path" }, { path: "/x" }, true), true);
  assert.equal(evaluateVerify({ kind: "scriptOk" }, {}, true), true);
  assert.equal(evaluateVerify({ kind: "scriptOk" }, {}, false), false);
});
```

Create `editors/vscode/test/installReceipt.test.ts`:

```ts
// The receipt is the uninstall source of truth AND the bridge between the two
// executors. The property these tests protect is that it is appended PER STEP:
// a half-finished install must be fully reversible, because complete installs
// rarely need uninstalling and broken ones always do.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { appendEntry, readReceipt, reverseOrder, type ReceiptEntry } from "../src/install/receipt.js";

async function tempReceipt(): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-receipt-"));
  return path.join(dir, "install-receipt.json");
}

function entry(stepId: string, target: string, preExisting = false): ReceiptEntry {
  return { stepId, script: "install.binary", kind: "binary", target, preExisting, at: "2026-08-08T00:00:00Z" };
}

test("appendEntry creates the file and accumulates entries", async () => {
  const file = await tempReceipt();
  await appendEntry(file, entry("binaryK3d", "/home/u/.memql/bin/k3d"));
  await appendEntry(file, entry("binaryKubectl", "/home/u/.memql/bin/kubectl"));

  const r = await readReceipt(file);
  assert.equal(r.version, 1);
  assert.equal(r.entries.length, 2);
  assert.equal(r.entries[0]?.stepId, "binaryK3d");
});

test("a receipt written by a killed run is still readable and reversible", async () => {
  // Simulates the crash case: two steps recorded, then the process died. The
  // file must be valid JSON at that point -- an append that leaves it truncated
  // would make a broken install unreversible, which is the case that matters.
  const file = await tempReceipt();
  await appendEntry(file, entry("binaryK3d", "/bin/k3d"));
  await appendEntry(file, entry("hostsEntries", "/etc/hosts"));

  const r = await readReceipt(file);
  const order = reverseOrder(r);
  assert.equal(order.length, 2);
  assert.equal(order[0]?.stepId, "hostsEntries", "uninstall must undo the newest artifact first");
  assert.equal(order[1]?.stepId, "binaryK3d");
});

test("readReceipt on a missing file returns an empty receipt, not an error", async () => {
  // Uninstall against a machine that never installed anything is a no-op, not
  // a crash.
  const file = await tempReceipt();
  const r = await readReceipt(file);
  assert.equal(r.entries.length, 0);
});

test("preExisting entries survive the round trip", async () => {
  // This flag is what stops uninstall removing a tool the user already had.
  const file = await tempReceipt();
  await appendEntry(file, entry("binaryK3d", "/usr/local/bin/k3d", true));
  const r = await readReceipt(file);
  assert.equal(r.entries[0]?.preExisting, true);
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd editors/vscode && npm test`
Expected: FAIL — `Cannot find module '../src/install/graph.js'`.

- [ ] **Step 3: Write `graph.ts`**

Create `editors/vscode/src/install/graph.ts`:

```ts
// Reading and validating the install/uninstall step graphs.
//
// This is a deliberate PORT of scripts/install/graph/graph.go, not an
// independent implementation. Both executors walk the same JSON documents --
// this one before a cluster exists, the Go one inside the engine afterwards --
// so the two must agree on what a valid graph is and on when a step counts as
// satisfied. When you change a rule here, change it there in the same commit.
//
// Nothing in src/install/ may import `vscode`: this code runs under bare
// `node --test` today and behind a webview in Epic 2.

import * as fs from "node:fs/promises";

export interface Verify {
  kind: "resultTrue" | "resultFalse" | "resultNonEmpty" | "scriptOk";
  path?: string;
  value?: string;
}

export interface ReceiptSpec {
  kind: string;
  targetPath: string;
  preExistingPath?: string;
}

export interface Step {
  id: string;
  title: string;
  script: string;
  params?: Record<string, string>;
  dependsOn?: string[];
  verify: Verify;
  elevation: "none" | "admin";
  readOnly?: boolean;
  receipt?: ReceiptSpec;
  reverses?: string;
}

export interface Graph {
  name: string;
  steps: Step[];
}

export async function loadGraph(file: string): Promise<Graph> {
  const raw = await fs.readFile(file, "utf8");
  const g = JSON.parse(raw) as Graph;
  validate(g, file);
  return g;
}

function validate(g: Graph, file: string): void {
  if (!Array.isArray(g.steps) || g.steps.length === 0) {
    throw new Error(`graph ${file} has no steps`);
  }
  const seen = new Set<string>();
  for (const s of g.steps) {
    if (!s.id) throw new Error(`graph ${file}: a step has an empty id`);
    if (seen.has(s.id)) {
      throw new Error(
        `graph ${file}: duplicate step id "${s.id}" -- ids address a step in the receipt and the executor`,
      );
    }
    seen.add(s.id);
    if (!s.title) throw new Error(`graph ${file}: step "${s.id}" has no title`);
    if (!s.script) throw new Error(`graph ${file}: step "${s.id}" names no capability script`);
    if (s.elevation !== "none" && s.elevation !== "admin") {
      throw new Error(`graph ${file}: step "${s.id}" has elevation "${s.elevation}", want none or admin`);
    }
    if (s.verify === undefined || s.verify === null) {
      throw new Error(
        `graph ${file}: step "${s.id}" declares no verify -- verify, not a zero exit code, is what advances the wizard`,
      );
    }
    switch (s.verify.kind) {
      case "resultTrue":
      case "resultFalse":
      case "resultNonEmpty":
        if (!s.verify.path) {
          throw new Error(`graph ${file}: step "${s.id}" verify kind ${s.verify.kind} needs a path`);
        }
        break;
      case "scriptOk":
        if (s.readOnly !== true) {
          throw new Error(
            `graph ${file}: step "${s.id}" uses verify kind scriptOk but is not readOnly -- ` +
              `for a mutating step that only proves the command exited 0, not that anything changed`,
          );
        }
        break;
      default:
        throw new Error(`graph ${file}: step "${s.id}" has unknown verify kind "${String(s.verify.kind)}"`);
    }
    if (s.receipt !== undefined && (!s.receipt.kind || !s.receipt.targetPath)) {
      throw new Error(`graph ${file}: step "${s.id}" receipt needs both kind and targetPath`);
    }
  }
  for (const s of g.steps) {
    for (const d of s.dependsOn ?? []) {
      if (!seen.has(d)) {
        throw new Error(`graph ${file}: step "${s.id}" depends on "${d}", which does not exist`);
      }
    }
  }
}

export function stepById(g: Graph, id: string): Step | undefined {
  return g.steps.find((s) => s.id === id);
}

// topoWaves returns execution waves: every id in a wave has all its
// dependencies satisfied by earlier waves, so a wave runs concurrently. This is
// how "some of these can be installed in parallel" stays a property of the data
// instead of becoming a decision in the executor or, worse, in the UI.
export function topoWaves(g: Graph): string[][] {
  const deps = new Map<string, string[]>();
  for (const s of g.steps) deps.set(s.id, [...(s.dependsOn ?? [])]);

  const done = new Set<string>();
  const waves: string[][] = [];

  while (done.size < deps.size) {
    const wave: string[] = [];
    for (const [id, d] of deps) {
      if (done.has(id)) continue;
      if (d.every((x) => done.has(x))) wave.push(id);
    }
    if (wave.length === 0) {
      const stuck = [...deps.keys()].filter((id) => !done.has(id)).sort();
      throw new Error(`dependency cycle among steps ${stuck.join(", ")}`);
    }
    wave.sort();
    for (const id of wave) done.add(id);
    waves.push(wave);
  }
  return waves;
}

// evaluateVerify decides whether a step is satisfied. `ok` is the capability
// envelope's ok flag; `result` is its result object.
//
// Only scriptOk consults `ok` -- and the loader permits that kind only on a
// read-only step. Everything else reads the RESULT, because a script can exit 0
// having changed nothing, and the whole point of a verify is to catch that.
export function evaluateVerify(v: Verify, result: Record<string, unknown>, ok: boolean): boolean {
  switch (v.kind) {
    case "scriptOk":
      return ok;
    case "resultTrue":
      return result[v.path as string] === true;
    case "resultFalse":
      return result[v.path as string] === false;
    case "resultNonEmpty": {
      const val = result[v.path as string];
      return typeof val === "string" && val !== "";
    }
    default:
      return false;
  }
}
```

- [ ] **Step 4: Write `receipt.ts`**

Create `editors/vscode/src/install/receipt.ts`:

```ts
// The install receipt: ~/.memql/install-receipt.json.
//
// THREE JOBS, ONE FILE. It is the pre-cluster executor's journal, the
// uninstall source of truth, and the bridge between the two executors (on the
// cluster's first healthy boot it is replayed into v1:install:installRun rows).
//
// APPENDED PER STEP, NOT WRITTEN AT THE END. That is the property that makes a
// HALF-FINISHED install fully reversible -- and the half-finished case is the
// one that matters, because complete installs rarely need uninstalling and
// broken ones always do. Every append is a read-modify-write of the whole
// document via a temp file and an atomic rename, so a process killed mid-write
// leaves the previous valid receipt rather than a truncated one.
//
// UNINSTALL READS THIS, NOT THE GRAPH. A dead cluster still has a receipt, and
// "the cluster is broken" is exactly when someone reaches for uninstall.

import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

export interface ReceiptEntry {
  stepId: string;
  script: string;
  // kind mirrors the graph's ReceiptSpec.kind and selects the uninstall backend
  // (binary | hostsEntries | mkcertCA | stack | images).
  kind: string;
  target: string;
  // preExisting is the flag that stops uninstall removing something the user
  // already had. It is carried per entry rather than recomputed at uninstall
  // time, because by then the evidence is gone.
  preExisting: boolean;
  at: string;
}

export interface Receipt {
  version: 1;
  graph: string;
  domain?: string;
  entries: ReceiptEntry[];
}

export function defaultReceiptPath(): string {
  return path.join(os.homedir(), ".memql", "install-receipt.json");
}

const EMPTY: Receipt = { version: 1, graph: "install", entries: [] };

// readReceipt returns an EMPTY receipt when the file is absent. Uninstall on a
// machine that never installed anything is a no-op, not an error.
export async function readReceipt(file: string): Promise<Receipt> {
  let raw: string;
  try {
    raw = await fs.readFile(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return { ...EMPTY, entries: [] };
    throw err;
  }
  if (raw.trim() === "") return { ...EMPTY, entries: [] };
  const r = JSON.parse(raw) as Receipt;
  if (!Array.isArray(r.entries)) r.entries = [];
  return r;
}

export async function appendEntry(file: string, entry: ReceiptEntry): Promise<void> {
  const current = await readReceipt(file);
  current.entries.push(entry);
  await writeAtomic(file, current);
}

export async function setMeta(file: string, meta: { graph?: string; domain?: string }): Promise<void> {
  const current = await readReceipt(file);
  if (meta.graph !== undefined) current.graph = meta.graph;
  if (meta.domain !== undefined) current.domain = meta.domain;
  await writeAtomic(file, current);
}

async function writeAtomic(file: string, r: Receipt): Promise<void> {
  await fs.mkdir(path.dirname(file), { recursive: true });
  const tmp = `${file}.tmp`;
  await fs.writeFile(tmp, `${JSON.stringify(r, null, 2)}\n`, "utf8");
  await fs.rename(tmp, file);
}

// reverseOrder returns entries newest-first. Uninstall undoes the most recent
// artifact first, which is the only order that respects the dependencies the
// install created (the CA must go before the mkcert binary that manages it).
export function reverseOrder(r: Receipt): ReceiptEntry[] {
  return [...r.entries].reverse();
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd editors/vscode && npm test`
Expected: PASS — all existing extension tests plus the ten new ones.

- [ ] **Step 6: Commit**

```bash
git add editors/vscode/src/install/graph.ts editors/vscode/src/install/receipt.ts \
        editors/vscode/test/installGraph.test.ts editors/vscode/test/installReceipt.test.ts
git commit -m "$(cat <<'EOF'
feat(install): add the TypeScript graph reader and install receipt

graph.ts is a deliberate port of scripts/install/graph/graph.go -- both
executors walk the same documents, so a rule change belongs in both in
one commit. evaluateVerify reads the RESULT, never the exit code, except
for scriptOk which the loader permits only on read-only steps.

receipt.ts appends per step through an atomic rename, which is what makes
a half-finished install fully reversible -- the case that matters, since
broken installs are the ones people uninstall. It carries preExisting per
entry because by uninstall time the evidence is gone.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 16: The runner and the executor

**Files:**
- Create: `editors/vscode/src/install/runner.ts`
- Create: `editors/vscode/src/install/executor.ts`
- Create: `editors/vscode/test/installExecutor.test.ts`

**Interfaces:**
- Consumes: `graph.ts` and `receipt.ts` (Task 15).
- Produces:
  - `runner.ts`: `interface CapabilityEnvelope { ok: boolean; capability: string; changed: boolean; result: Record<string, unknown>; error: { code: number; message: string } | null }`, `interface ScriptRunner { run(scriptId: string, params: Record<string,string>): Promise<CapabilityEnvelope> }`, `class ProcessScriptRunner implements ScriptRunner` (constructed with a repo root and an allowlist map), `parseEnvelope(stdout: string): CapabilityEnvelope`.
  - `executor.ts`: `type StepStatus = "pending"|"running"|"satisfied"|"failed"|"skipped"`, `interface StepOutcome { id: string; status: StepStatus; changed: boolean; envelope?: CapabilityEnvelope; error?: string }`, `interface RunOptions { mode: "automatic"|"guided"; receiptFile: string; onEvent?: (e: ExecutorEvent) => void }`, `type ExecutorEvent = { type: "stepStarted"|"stepFinished"|"waveStarted"; ... }`, `async function runGraph(g: Graph, runner: ScriptRunner, opts: RunOptions): Promise<StepOutcome[]>`.
- The executor never decides order or concurrency: it consumes `topoWaves` and runs each wave with `Promise.all`.

- [ ] **Step 1: Write the failing test**

Create `editors/vscode/test/installExecutor.test.ts`:

```ts
// Executor behaviour, driven by a fake ScriptRunner. There is no process
// spawning here on purpose: the things that can be wrong are the ORDER, the
// CONCURRENCY, the FAILURE BLAST RADIUS, and WHAT LANDS IN THE RECEIPT -- none
// of which need a real script to exercise, and all of which would be buried by
// one that did.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import type { Graph } from "../src/install/graph.js";
import type { CapabilityEnvelope, ScriptRunner } from "../src/install/runner.js";
import { runGraph } from "../src/install/executor.js";
import { readReceipt } from "../src/install/receipt.js";

function ok(result: Record<string, unknown>, changed = true): CapabilityEnvelope {
  return { ok: true, capability: "fake", changed, result, error: null };
}

function fail(code: number, message: string): CapabilityEnvelope {
  return { ok: false, capability: "fake", changed: false, result: {}, error: { code, message } };
}

// recordingRunner logs call order and start/end so concurrency is observable.
function recordingRunner(
  responses: Record<string, CapabilityEnvelope>,
  delays: Record<string, number> = {},
): { runner: ScriptRunner; order: string[]; overlapped: boolean } {
  const order: string[] = [];
  let active = 0;
  let overlapped = false;
  const runner: ScriptRunner = {
    async run(scriptId, params) {
      const key = params.tool ? `${scriptId}:${params.tool}` : scriptId;
      order.push(key);
      active += 1;
      if (active > 1) overlapped = true;
      await new Promise((r) => setTimeout(r, delays[key] ?? 1));
      active -= 1;
      return responses[key] ?? ok({});
    },
  };
  return { runner, order, get overlapped() { return overlapped; } } as never;
}

async function tempReceipt(): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-exec-"));
  return path.join(dir, "receipt.json");
}

const GRAPH: Graph = {
  name: "install",
  steps: [
    { id: "detect", title: "Detect", script: "install.detect", elevation: "none", readOnly: true, verify: { kind: "scriptOk" } },
    {
      id: "a", title: "A", script: "install.binary", params: { tool: "k3d" }, dependsOn: ["detect"],
      elevation: "none", verify: { kind: "resultNonEmpty", path: "path" },
      receipt: { kind: "binary", targetPath: "path", preExistingPath: "preExisting" },
    },
    {
      id: "b", title: "B", script: "install.binary", params: { tool: "kubectl" }, dependsOn: ["detect"],
      elevation: "none", verify: { kind: "resultNonEmpty", path: "path" },
      receipt: { kind: "binary", targetPath: "path", preExistingPath: "preExisting" },
    },
    { id: "c", title: "C", script: "install.cloneStack", dependsOn: ["a"], elevation: "none", verify: { kind: "resultNonEmpty", path: "commit" } },
  ],
};

test("independent steps in a wave run concurrently", async () => {
  const { runner, overlapped } = recordingRunner({
    "install.binary:k3d": ok({ path: "/bin/k3d", preExisting: false }),
    "install.binary:kubectl": ok({ path: "/bin/kubectl", preExisting: false }),
    "install.cloneStack": ok({ commit: "abc" }),
  }, { "install.binary:k3d": 20, "install.binary:kubectl": 20 });

  await runGraph(GRAPH, runner, { mode: "automatic", receiptFile: await tempReceipt() });
  assert.equal(overlapped, true, "a and b are independent and must overlap");
});

test("a failed step halts only its subtree", async () => {
  // `a` fails, so `c` (which depends on it) is skipped -- but `b` is
  // independent and must still complete. Abandoning unrelated work because one
  // branch failed is how a wizard turns one problem into three.
  const { runner } = recordingRunner({
    "install.binary:k3d": fail(5, "download failed"),
    "install.binary:kubectl": ok({ path: "/bin/kubectl", preExisting: false }),
  });

  const outcomes = await runGraph(GRAPH, runner, { mode: "automatic", receiptFile: await tempReceipt() });
  const by = (id: string) => outcomes.find((o) => o.id === id);

  assert.equal(by("a")?.status, "failed");
  assert.equal(by("b")?.status, "satisfied", "an independent branch must still finish");
  assert.equal(by("c")?.status, "skipped", "a dependent step must not run after its dependency failed");
});

test("a step whose verify fails is a failure even when the script exited 0", async () => {
  // The central rule: exit 0 is not evidence the world changed.
  const { runner } = recordingRunner({
    "install.binary:k3d": ok({ path: "" }), // ok=true, but the verify path is empty
    "install.binary:kubectl": ok({ path: "/bin/kubectl", preExisting: false }),
  });
  const outcomes = await runGraph(GRAPH, runner, { mode: "automatic", receiptFile: await tempReceipt() });
  assert.equal(outcomes.find((o) => o.id === "a")?.status, "failed");
});

test("the receipt is appended as each step succeeds, carrying preExisting", async () => {
  const file = await tempReceipt();
  const { runner } = recordingRunner({
    "install.binary:k3d": ok({ path: "/bin/k3d", preExisting: true }),
    "install.binary:kubectl": ok({ path: "/bin/kubectl", preExisting: false }),
    "install.cloneStack": ok({ commit: "abc" }),
  });

  await runGraph(GRAPH, runner, { mode: "automatic", receiptFile: file });
  const r = await readReceipt(file);

  // detect and c declare no receipt spec, so only a and b are recorded.
  assert.equal(r.entries.length, 2);
  const k3d = r.entries.find((e) => e.stepId === "a");
  assert.equal(k3d?.target, "/bin/k3d");
  assert.equal(k3d?.preExisting, true, "preExisting must survive into the receipt or uninstall will remove the user's own tool");
});

test("guided mode runs no scripts but still verifies", async () => {
  // In guided mode the human runs the command; the executor only polls the
  // verify. A runner call would mean we executed something we promised not to.
  const calls: string[] = [];
  const runner: ScriptRunner = {
    async run(scriptId) {
      calls.push(scriptId);
      return ok({ path: "/bin/x", preExisting: false });
    },
  };
  const outcomes = await runGraph(GRAPH, runner, {
    mode: "guided",
    receiptFile: await tempReceipt(),
  });
  assert.equal(calls.filter((c) => c !== "install.detect").length, 0,
    "guided mode must not execute mutating steps");
  assert.ok(outcomes.length > 0);
});
```

**How guided mode probes without doing (decided, implement exactly this):**

Guided mode needs to answer "is this step satisfied yet?" without performing it. Two mechanisms, in precedence order:

1. **`verifyScript` on the step (preferred).** An optional `verifyScript: string` + `verifyParams?: Record<string,string>` on `Step`. When present, guided mode — and every resume, in both modes — runs *that* capability instead of the action, and applies the step's `verify` to its result. Read-only by construction.
2. **`--dry-run` fallback.** When no `verifyScript` is declared, guided mode runs the action script with `--dry-run=1`. Every mutating script in Tasks 3–10 already supports it and reports current state without writing. The step counts as satisfied when the dry run returns `ok=true` **and** `changed=false` — i.e. the script found nothing left to do.

Add `verifyScript` / `verifyParams` to `Step` in both `graph.go` and `graph.ts` (optional, no validation beyond "if `verifyScript` is set it must be a non-empty string"). This replaces the placeholder ternary in `executor.ts` Step 4.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd editors/vscode && npm test`
Expected: FAIL — `Cannot find module '../src/install/runner.js'`.

- [ ] **Step 3: Write `runner.ts`**

Create `editors/vscode/src/install/runner.ts`:

```ts
// Dispatching capability scripts and parsing their result envelopes.
//
// NO SHELL, EVER. The script is executed directly by its allowlisted path and
// every param is its own argv element (`--name=value`), mirroring
// component/automations/steps/capability_script.go. That is what makes a value
// containing shell metacharacters inert: there is no shell to re-parse it.
//
// THE LAST JSON LINE IS THE ENVELOPE. The contract puts human logs on stderr
// and exactly one JSON object on stdout, but a script may legitimately print
// progress to stdout too; taking the last parseable line is what the Go side
// does (deploycontrol.ParseCapabilityResult) and keeps the two in step.

import { spawn } from "node:child_process";
import * as path from "node:path";

export interface CapabilityEnvelope {
  ok: boolean;
  capability: string;
  changed: boolean;
  result: Record<string, unknown>;
  error: { code: number; message: string } | null;
}

export interface ScriptRunner {
  run(scriptId: string, params: Record<string, string>): Promise<CapabilityEnvelope>;
}

export function parseEnvelope(stdout: string): CapabilityEnvelope {
  const lines = stdout.trim().split("\n");
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    const l = (lines[i] ?? "").trim();
    if (l.startsWith("{") && l.endsWith("}")) {
      const parsed = JSON.parse(l) as CapabilityEnvelope;
      if (parsed.result === undefined || parsed.result === null) parsed.result = {};
      return parsed;
    }
  }
  throw new Error("capability script emitted no JSON result envelope on stdout");
}

export class ProcessScriptRunner implements ScriptRunner {
  constructor(
    private readonly repoRoot: string,
    private readonly allowlist: Record<string, string>,
    private readonly timeoutMs = 15 * 60 * 1000,
  ) {}

  async run(scriptId: string, params: Record<string, string>): Promise<CapabilityEnvelope> {
    const rel = this.allowlist[scriptId];
    if (rel === undefined) {
      throw new Error(
        `capability script "${scriptId}" is not allowlisted -- a step may only run a registered script`,
      );
    }
    const abs = path.join(this.repoRoot, rel);
    const args = Object.entries(params).map(([k, v]) => `--${k}=${v}`);

    return await new Promise<CapabilityEnvelope>((resolve, reject) => {
      const child = spawn(abs, args, { stdio: ["ignore", "pipe", "pipe"] });
      let out = "";
      let err = "";
      const timer = setTimeout(() => {
        child.kill("SIGKILL");
        reject(new Error(`capability script "${scriptId}" exceeded ${this.timeoutMs}ms and was killed`));
      }, this.timeoutMs);

      child.stdout.on("data", (d: Buffer) => { out += d.toString(); });
      child.stderr.on("data", (d: Buffer) => { err += d.toString(); });
      child.on("error", (e) => { clearTimeout(timer); reject(e); });
      child.on("close", () => {
        clearTimeout(timer);
        try {
          resolve(parseEnvelope(out));
        } catch {
          // A script that died before emitting an envelope still has to produce
          // one, or the executor cannot tell "failed" from "crashed".
          reject(new Error(`capability script "${scriptId}" produced no envelope. stderr:\n${err}`));
        }
      });
    });
  }
}
```

- [ ] **Step 4: Write `executor.ts`**

Create `editors/vscode/src/install/executor.ts`:

```ts
// Walking the graph.
//
// THE EXECUTOR DECIDES NOTHING. Order and concurrency come from topoWaves;
// whether a step is satisfied comes from evaluateVerify; what to record comes
// from the step's receipt spec. Everything here is mechanism.
//
// FAILURE BLAST RADIUS IS THE SUBTREE, NOT THE RUN. When a step fails, only
// steps that (transitively) depend on it are skipped. Abandoning independent
// work turns one problem into several and throws away progress the user would
// otherwise keep on a resume.

import { evaluateVerify, topoWaves, type Graph, type Step } from "./graph.js";
import { appendEntry, setMeta } from "./receipt.js";
import type { CapabilityEnvelope, ScriptRunner } from "./runner.js";

export type StepStatus = "pending" | "running" | "satisfied" | "failed" | "skipped";

export interface StepOutcome {
  id: string;
  status: StepStatus;
  changed: boolean;
  envelope?: CapabilityEnvelope;
  error?: string;
}

export type ExecutorEvent =
  | { type: "waveStarted"; ids: string[] }
  | { type: "stepStarted"; id: string; title: string; elevation: "none" | "admin" }
  | { type: "stepFinished"; outcome: StepOutcome };

export interface RunOptions {
  mode: "automatic" | "guided";
  receiptFile: string;
  // params merged into every step's declared params (domain, owner email, the
  // provider key, ...). Collected up front by Stage 1 so the run is unattended.
  runParams?: Record<string, string>;
  onEvent?: (e: ExecutorEvent) => void;
  now?: () => string;
}

export async function runGraph(
  g: Graph,
  runner: ScriptRunner,
  opts: RunOptions,
): Promise<StepOutcome[]> {
  const waves = topoWaves(g);
  const outcomes = new Map<string, StepOutcome>();
  const now = opts.now ?? (() => new Date().toISOString());

  await setMeta(opts.receiptFile, { graph: g.name, domain: opts.runParams?.domain });

  for (const wave of waves) {
    opts.onEvent?.({ type: "waveStarted", ids: wave });

    await Promise.all(
      wave.map(async (id) => {
        const step = g.steps.find((s) => s.id === id) as Step;

        // Skip when any dependency did not reach "satisfied".
        const blocked = (step.dependsOn ?? []).some(
          (d) => outcomes.get(d)?.status !== "satisfied",
        );
        if (blocked) {
          const outcome: StepOutcome = { id, status: "skipped", changed: false };
          outcomes.set(id, outcome);
          opts.onEvent?.({ type: "stepFinished", outcome });
          return;
        }

        opts.onEvent?.({ type: "stepStarted", id, title: step.title, elevation: step.elevation });

        // In guided mode the human runs mutating steps; we only probe. A
        // read-only step is safe to run either way, so it always runs.
        const params = { ...(opts.runParams ?? {}), ...(step.params ?? {}) };
        const probeOnly = opts.mode === "guided" && step.readOnly !== true;
        if (probeOnly) params["dry-run"] = "1";

        let envelope: CapabilityEnvelope;
        try {
          envelope = probeOnly && step.readOnly !== true && opts.mode === "guided"
            ? await runner.run(step.script, params)
            : await runner.run(step.script, params);
        } catch (err) {
          const outcome: StepOutcome = {
            id, status: "failed", changed: false,
            error: err instanceof Error ? err.message : String(err),
          };
          outcomes.set(id, outcome);
          opts.onEvent?.({ type: "stepFinished", outcome });
          return;
        }

        const satisfied = evaluateVerify(step.verify, envelope.result, envelope.ok);
        if (!satisfied) {
          const outcome: StepOutcome = {
            id, status: "failed", changed: envelope.changed, envelope,
            error: envelope.error?.message
              ?? `verify failed: ${step.verify.kind}${step.verify.path ? ` on "${step.verify.path}"` : ""} `
                 + `was not satisfied even though the script reported ok=${envelope.ok}`,
          };
          outcomes.set(id, outcome);
          opts.onEvent?.({ type: "stepFinished", outcome });
          return;
        }

        // Record BEFORE reporting success: a crash between the two must leave
        // the artifact reversible, and an extra receipt entry for something
        // that did happen is harmless where a missing one is not.
        if (step.receipt !== undefined) {
          const target = envelope.result[step.receipt.targetPath];
          const pre = step.receipt.preExistingPath
            ? envelope.result[step.receipt.preExistingPath] === true
            : false;
          await appendEntry(opts.receiptFile, {
            stepId: step.id,
            script: step.script,
            kind: step.receipt.kind,
            target: typeof target === "string" ? target : "",
            preExisting: pre,
            at: now(),
          });
        }

        const outcome: StepOutcome = { id, status: "satisfied", changed: envelope.changed, envelope };
        outcomes.set(id, outcome);
        opts.onEvent?.({ type: "stepFinished", outcome });
      }),
    );
  }

  return g.steps.map(
    (s) => outcomes.get(s.id) ?? { id: s.id, status: "pending" as StepStatus, changed: false },
  );
}
```

Replace the `probeOnly` block with the decided design (see Step 1). Concretely:

```ts
        // Guided mode, and every resume, must be able to CHECK without DOING.
        // verifyScript is the read-only probe when a step declares one;
        // otherwise the action's own --dry-run reports current state, and
        // "nothing left to do" (ok && !changed) means already satisfied.
        const probe = opts.mode === "guided" && step.readOnly !== true;
        let envelope: CapabilityEnvelope;
        try {
          if (probe && step.verifyScript !== undefined) {
            envelope = await runner.run(step.verifyScript, { ...(opts.runParams ?? {}), ...(step.verifyParams ?? {}) });
          } else if (probe) {
            const dry = await runner.run(step.script, { ...params, "dry-run": "1" });
            envelope = { ...dry, ok: dry.ok && !dry.changed };
          } else {
            envelope = await runner.run(step.script, params);
          }
        } catch (err) {
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd editors/vscode && npm test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add editors/vscode/src/install/runner.ts editors/vscode/src/install/executor.ts \
        editors/vscode/test/installExecutor.test.ts
git commit -m "$(cat <<'EOF'
feat(install): add the capability-script runner and graph executor

The runner spawns the allowlisted path directly with each param as its
own argv element -- no shell, mirroring capability_script.go, so a value
containing shell metacharacters is inert.

The executor decides nothing: order and concurrency come from topoWaves,
satisfaction from evaluateVerify, recording from the step's receipt spec.
A failed step skips only its dependent subtree; independent branches
still finish, because abandoning unrelated work turns one problem into
several and discards progress a resume would have kept.

Receipt entries are written BEFORE success is reported: a crash between
the two must leave the artifact reversible.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 17: The CLI harness and the clean-runner end-to-end test

**Files:**
- Create: `editors/vscode/src/install/cli.ts`
- Modify: `editors/vscode/package.json` (add an `install-cli` build script)
- Create: `.github/workflows/install-e2e.yml`
- Create: `scripts/install/e2e-baseline.sh`

**Interfaces:**
- Consumes: everything from Tasks 1–16.
- Produces: `node dist/install/cli.js install|uninstall --graph-dir=… --receipt=… [--param k=v]…`, exiting 0 on a fully satisfied run and 1 otherwise, printing one line per step to stderr and a JSON summary to stdout.
- This is Epic 1's whole user surface. Epic 2 replaces the printing with a webview and reuses `runGraph` untouched.

- [ ] **Step 1: Write the baseline capture script**

Create `scripts/install/e2e-baseline.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/e2e-baseline.sh
# ===============================
#
# Capability: install.e2eBaseline -- snapshot the machine state the install/
# uninstall round trip must return to.
#
# WHY A SNAPSHOT DIFF AND NOT AN INSPECTION. "Uninstall looks clean" is an
# opinion. This records the four surfaces the installer touches -- the hosts
# file, ~/.memql, the docker image list, and the k3d cluster list -- so the E2E
# can assert byte equality before and after. That is the only thing that
# actually proves a receipt works.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/e2e-baseline.sh --out=/tmp/before.txt
#   scripts/install/e2e-baseline.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 5 snapshot failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.e2eBaseline" "Snapshot the machine state the install/uninstall round trip must restore."
cap_spec_param "out"        "file to write the snapshot to"
cap_spec_param "hosts-file" "hosts file to include in the snapshot"
cap_spec_param "memql-home" "memQL home directory to include in the snapshot"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local out hosts_file home
    out="$(cap_param out "")"
    hosts_file="$(cap_param hosts-file "/etc/hosts")"
    home="$(cap_param memql-home "${HOME}/.memql")"
    cap_require out "$out"

    {
        printf '## hosts\n'
        cat "$hosts_file" 2>/dev/null || true
        printf '## memql-home\n'
        # Names only: mtimes and sizes churn for reasons the installer does not
        # control, and the question is "is anything left behind", not "is every
        # byte identical".
        (cd "$home" 2>/dev/null && find . | sort) || true
        printf '## docker-images\n'
        (docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | sort) || true
        printf '## k3d-clusters\n'
        (k3d cluster list --no-headers 2>/dev/null | awk '{print $1}' | sort) || true
    } > "$out" || cap_fail 5 "could not write the snapshot to ${out}"

    cap_info "snapshot written to ${out}"
    cap_result_set out "$out"
    cap_ok
}

main "$@"
```

Register it in the allowlist (`component/automations/steps/capability_script.go`), alongside the Task 11 entries:

```go
	"install.e2eBaseline": "scripts/install/e2e-baseline.sh",
```

- [ ] **Step 2: Write the CLI**

Create `editors/vscode/src/install/cli.ts`:

```ts
// The bare CLI harness -- Epic 1's entire user surface.
//
// It exists so the graph, the runner and the receipt are exercised end to end
// BEFORE any UI is written. Epic 2 replaces the printing below with a webview
// and reuses runGraph untouched; if anything in this file grows a decision, it
// belongs in the graph instead.
//
// Human progress goes to STDERR and a machine-readable summary to STDOUT, the
// same split the capability scripts use, so the harness composes in CI.

import * as path from "node:path";

import { loadGraph } from "./graph.js";
import { runGraph, type ExecutorEvent, type StepOutcome } from "./executor.js";
import { ProcessScriptRunner } from "./runner.js";
import { defaultReceiptPath } from "./receipt.js";

// ALLOWLIST mirrors capabilityScriptAllowlist in
// component/automations/steps/capability_script.go. Keep the two in step: the
// Go map is the security boundary for the in-engine path, this one for the
// host path, and a step is only runnable where it is registered.
const ALLOWLIST: Record<string, string> = {
  "install.detect": "scripts/install/detect.sh",
  "install.binary": "scripts/install/install-binary.sh",
  "install.hostsEntries": "scripts/install/hosts-entries.sh",
  "install.mkcert": "scripts/install/mkcert-setup.sh",
  "install.cloneStack": "scripts/install/clone-stack.sh",
  "install.verifyProviderKey": "scripts/install/verify-provider-key.sh",
  "install.verifyFrontDoor": "scripts/install/verify-frontdoor.sh",
  "install.magicLink": "scripts/install/magic-link.sh",
  "install.removeArtifact": "scripts/install/remove-artifact.sh",
  "install.e2eBaseline": "scripts/install/e2e-baseline.sh",
  "k3d.up": "scripts/k3d/up.sh",
  "k3d.down": "scripts/k3d/down.sh",
};

interface Args {
  command: "install" | "uninstall";
  repoRoot: string;
  graphDir: string;
  receipt: string;
  mode: "automatic" | "guided";
  params: Record<string, string>;
}

function parseArgs(argv: string[]): Args {
  const command = argv[0];
  if (command !== "install" && command !== "uninstall") {
    throw new Error(`usage: cli.js install|uninstall [--repo-root=…] [--graph-dir=…] [--receipt=…] [--mode=automatic|guided] [--param k=v]…`);
  }
  const out: Args = {
    command,
    repoRoot: process.cwd(),
    graphDir: path.join(process.cwd(), "scripts", "install", "graph"),
    receipt: defaultReceiptPath(),
    mode: "automatic",
    params: {},
  };
  for (let i = 1; i < argv.length; i += 1) {
    const a = argv[i] ?? "";
    if (a === "--param") {
      const kv = argv[i + 1] ?? "";
      const [k, ...rest] = kv.split("=");
      if (!k || rest.length === 0) throw new Error(`--param needs k=v, got "${kv}"`);
      out.params[k] = rest.join("=");
      i += 1;
      continue;
    }
    const [flag, ...rest] = a.replace(/^--/, "").split("=");
    const value = rest.join("=");
    switch (flag) {
      case "repo-root": out.repoRoot = value; break;
      case "graph-dir": out.graphDir = value; break;
      case "receipt": out.receipt = value; break;
      case "mode":
        if (value !== "automatic" && value !== "guided") throw new Error(`--mode must be automatic or guided`);
        out.mode = value;
        break;
      default: throw new Error(`unknown flag --${flag}`);
    }
  }
  return out;
}

function render(e: ExecutorEvent): void {
  switch (e.type) {
    case "waveStarted":
      process.stderr.write(`==> wave: ${e.ids.join(", ")}\n`);
      break;
    case "stepStarted":
      process.stderr.write(`  - ${e.title}${e.elevation === "admin" ? "  [needs admin]" : ""}\n`);
      break;
    case "stepFinished": {
      const o = e.outcome;
      const mark = o.status === "satisfied" ? "OK  " : o.status === "skipped" ? "SKIP" : "FAIL";
      process.stderr.write(`    ${mark} ${o.id}${o.error ? `: ${o.error}` : ""}\n`);
      break;
    }
  }
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2));
  const file = path.join(args.graphDir, `${args.command}.json`);
  const graph = await loadGraph(file);
  const runner = new ProcessScriptRunner(args.repoRoot, ALLOWLIST);

  const outcomes: StepOutcome[] = await runGraph(graph, runner, {
    mode: args.mode,
    receiptFile: args.receipt,
    runParams: args.params,
    onEvent: render,
  });

  const failed = outcomes.filter((o) => o.status !== "satisfied");
  process.stdout.write(`${JSON.stringify({ graph: graph.name, outcomes }, null, 2)}\n`);
  process.exitCode = failed.length === 0 ? 0 : 1;
}

main().catch((err: unknown) => {
  process.stderr.write(`${err instanceof Error ? err.message : String(err)}\n`);
  process.exitCode = 1;
});
```

Add the build script to `editors/vscode/package.json` (inside `"scripts"`):

```json
    "install-cli": "tsc -p tsconfig.test.json && node esbuild.test.js",
```

- [ ] **Step 3: Run it locally against the graph**

Run:
```bash
cd editors/vscode && npm run install-cli
cd ../.. && node editors/vscode/dist-test/src/install/cli.js install --mode=guided --receipt=/tmp/memql-receipt.json
```
Expected: the wave/step lines print to stderr and a JSON summary to stdout. Failures on a machine without Docker are the expected outcome here — what is being verified is that the graph loads, waves order correctly, and the receipt is written.

- [ ] **Step 4: Write the E2E workflow**

Create `.github/workflows/install-e2e.yml`:

```yaml
# Clean-runner install -> uninstall -> baseline diff.
#
# This is the only test that meaningfully proves the receipt works. Everything
# else checks a part; this checks that the machine comes back.
name: install-e2e

on:
  pull_request:
    paths:
      - "scripts/install/**"
      - "editors/vscode/src/install/**"
      - "dsl/install/**"
      - ".github/workflows/install-e2e.yml"
  workflow_dispatch:

jobs:
  round-trip:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - uses: actions/setup-node@v4
        with:
          node-version: "20"

      - name: Build the install CLI
        working-directory: editors/vscode
        run: |
          npm ci
          npm run install-cli

      - name: Capture the baseline
        run: |
          chmod +x scripts/install/*.sh
          scripts/install/e2e-baseline.sh --out=/tmp/before.txt

      - name: Install
        env:
          # A rejected key must not be what fails this job -- the AI provider
          # step is verified by its own unit test. Point it at a stub so the
          # round trip under test is the INSTALL, not the vendor's uptime.
          MEMQL_AI_ANTHROPIC_API_KEY: ${{ secrets.MEMQL_E2E_ANTHROPIC_KEY }}
        run: |
          node editors/vscode/dist-test/src/install/cli.js install \
            --repo-root="$PWD" \
            --receipt=/tmp/receipt.json \
            --param domain=memql.localhost \
            --param owner-email=e2e@example.com \
            --param ref="${GITHUB_SHA}" \
            --param vendor=anthropic

      - name: Show the receipt
        if: always()
        run: cat /tmp/receipt.json || true

      - name: Uninstall
        if: always()
        run: |
          node editors/vscode/dist-test/src/install/cli.js uninstall \
            --repo-root="$PWD" \
            --receipt=/tmp/receipt.json \
            --param confirm="remove memql artifacts"

      - name: Assert the machine is back to baseline
        if: always()
        run: |
          scripts/install/e2e-baseline.sh --out=/tmp/after.txt
          if ! diff -u /tmp/before.txt /tmp/after.txt; then
            echo "::error::uninstall did not restore the machine to its pre-install state"
            exit 1
          fi
          echo "baseline restored"
```

**On the ref the E2E installs.** `install.cloneStack` refuses anything that is not a tag, and CI is not exempt — an exemption would mean the one job that exercises the real path exercises a different path. So the workflow **creates a tag on the checkout** and clones from the local repo. Replace the `Install` step's checkout wiring with:

```yaml
      - name: Tag the checkout so cloneStack has a real tag to pin
        run: |
          git config user.email "e2e@example.com"
          git config user.name "install-e2e"
          git tag "e2e-${GITHUB_SHA::12}"

      - name: Install
        run: |
          node editors/vscode/dist-test/src/install/cli.js install \
            --repo-root="$PWD" \
            --receipt=/tmp/receipt.json \
            --param domain=memql.localhost \
            --param owner-email=e2e@example.com \
            --param owner-first-name=E2E \
            --param owner-last-name=Runner \
            --param repo-url="$PWD" \
            --param ref="e2e-${GITHUB_SHA::12}" \
            --param vendor=anthropic \
            --param api-key="${{ secrets.MEMQL_E2E_ANTHROPIC_KEY }}"
```

The tag is local to the runner's checkout and never pushed, and `--param repo-url="$PWD"` makes `git ls-remote --tags` resolve against it — so the tag rule holds without a special case.

- [ ] **Step 5: Verify the workflow parses and the unit suites are green**

Run:
```bash
go test ./scripts/... ./component/automations/steps/ -v
cd editors/vscode && npm test
python3 -c "import sys,yaml; yaml.safe_load(open('.github/workflows/install-e2e.yml')); print('workflow parses')"
```
Expected: PASS on all three.

- [ ] **Step 6: Commit**

```bash
git add editors/vscode/src/install/cli.ts editors/vscode/package.json \
        scripts/install/e2e-baseline.sh .github/workflows/install-e2e.yml \
        component/automations/steps/capability_script.go
git commit -m "$(cat <<'EOF'
feat(install): add the CLI harness and clean-runner round-trip E2E

The CLI is Epic 1's whole user surface and exists so the graph, runner
and receipt are exercised end to end before any UI is written. Epic 2
replaces the printing with a webview and reuses runGraph untouched.

The E2E snapshots the four surfaces the installer touches, installs,
uninstalls, and diffs. "Uninstall looks clean" is an opinion; a byte diff
is the only thing that actually proves a receipt works.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 18: `install.seedBootstrap` — the owner, the domain, and the AI key

**Files:**
- Create: `scripts/install/seed-bootstrap.sh`
- Create: `scripts/install/seed_bootstrap_test.go`
- Modify: `deploy/k8s/overlays/local` (reference the new secret from the identity and AI-consuming Deployments)
- Modify: `scripts/install/graph/install.json` (insert the step)
- Modify: `dsl/install/actions.memql` (add the action)
- Modify: `component/automations/steps/capability_script.go` (allowlist entry)

**Why this task exists:** the self-review found it missing. Spec §5 Stage 4 says bring-up seeds "bootstrap variables, AI key, TLS", and D7 depends on `MEMQL_IDENTITY_BOOTSTRAP_*` being set *before* identity first starts — that is what makes the cluster self-bootstrap with no `/setup` visit. Nothing in Tasks 1–17 wrote them, so `install.magicLink` would have had no owner to find a link for.

**Interfaces:**
- Produces: capability id `install.seedBootstrap`. Params: `--namespace`, `--domain`, `--owner-email`, `--owner-first-name`, `--owner-last-name`, `--registration-mode`, `--vendor`, `--api-key`, `--tls-cert`, `--tls-key`, `--kubectl-bin`, `--dry-run`. Result: `{"namespace":string,"secretName":string,"seeded":bool,"ownerEmail":string}`.
- Must run **before** `clusterUp`, since identity reads these at first start; after it, they are inert (`clusterSettings.bootstrappedAt` closes the wizard).
- The API key goes in via `--from-file` on a 0600 temp file, never `--from-literal`, for the same `ps`-visibility reason as Task 7.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/seed_bootstrap_test.go`:

```go
package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type seedResult struct {
	Namespace  string `json:"namespace"`
	SecretName string `json:"secretName"`
	Seeded     bool   `json:"seeded"`
	OwnerEmail string `json:"ownerEmail"`
}

// fakeKubectlRecording writes a stub kubectl that appends its argv to a log and
// succeeds, so the test can assert what was sent without a cluster.
func fakeKubectlRecording(t *testing.T, argvLog string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kubectl")
	body := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "` + argvLog + `"
exit 0
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	return bin
}

// TestSeedBootstrapKeepsTheApiKeyOffTheCommandLine -- same rule as Task 7:
// kubectl argv is visible in `ps` to every user on the machine.
func TestSeedBootstrapKeepsTheApiKeyOffTheCommandLine(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/seed-bootstrap.sh"
	log := filepath.Join(t.TempDir(), "argv.log")
	secret := "sk-ant-SEEDSECRET999"

	env, out := runScript(t, script,
		"--domain=memql.localhost", "--owner-email=o@example.com",
		"--owner-first-name=O", "--owner-last-name=Wner",
		"--vendor=anthropic", "--api-key="+secret,
		"--kubectl-bin="+fakeKubectlRecording(t, log))
	if !env.OK {
		t.Fatalf("seed failed: %s\noutput:\n%s", env.Error.Message, out)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("the API key appeared in kubectl argv:\n%s", b)
	}
}

// TestSeedBootstrapRequiresEveryBootstrapField: identity auto-bootstraps only
// when ALL required fields are set. A partial seed silently falls back to the
// interactive /setup page, which a headless install can never complete.
func TestSeedBootstrapRequiresEveryBootstrapField(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/seed-bootstrap.sh"
	log := filepath.Join(t.TempDir(), "argv.log")

	env, _ := runScript(t, script,
		"--domain=memql.localhost", "--owner-email=o@example.com",
		"--vendor=anthropic", "--api-key=x",
		"--kubectl-bin="+fakeKubectlRecording(t, log)) // no owner names
	if env.OK {
		t.Fatal("seeded with an incomplete bootstrap set")
	}
	if env.Error == nil || env.Error.Code != 2 {
		t.Errorf("want error.code 2 (bad param); got %+v", env.Error)
	}
}

// TestSeedBootstrapResultShape guards the fields the graph's verify reads.
func TestSeedBootstrapResultShape(t *testing.T) {
	script := repoRoot(t) + "/scripts/install/seed-bootstrap.sh"
	log := filepath.Join(t.TempDir(), "argv.log")
	env, _ := runScript(t, script,
		"--domain=memql.localhost", "--owner-email=o@example.com",
		"--owner-first-name=O", "--owner-last-name=Wner",
		"--vendor=openai", "--api-key=x",
		"--kubectl-bin="+fakeKubectlRecording(t, log))
	var r seedResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not the seed schema: %v", err)
	}
	if !r.Seeded || r.OwnerEmail != "o@example.com" {
		t.Errorf("unexpected result: %+v", r)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./scripts/install/ -run TestSeedBootstrap -v`
Expected: FAIL — no JSON envelope (script missing).

- [ ] **Step 3: Write the script**

Create `scripts/install/seed-bootstrap.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/install/seed-bootstrap.sh
# =================================
#
# Capability: install.seedBootstrap -- seed the identity bootstrap values and
# the AI provider key so the cluster self-bootstraps on first start.
#
# WHY THIS MUST RUN BEFORE THE CLUSTER COMES UP. component/identity/config.go
# auto-bootstraps only when Domain, OwnerEmail, OwnerFirstName, OwnerLastName
# and RegistrationMode are ALL set when identity first starts; miss one and it
# falls back to waiting for an operator on /setup, which a headless install can
# never complete. After bootstrap the values are inert
# (clusterSettings.bootstrappedAt closes the wizard), so this is a
# one-shot-before-boot step, not configuration.
#
# ALL-OR-NOTHING ON PURPOSE. A partial set is the worst outcome: it looks like
# it worked and then silently produces a cluster nobody can sign into. So a
# missing field is exit 2, not a warning.
#
# THE API KEY NEVER TOUCHES ARGV. kubectl arguments are visible in `ps` exactly
# as curl's are, so the key goes in through --from-file with a 0600 temp file
# removed on every exit path.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param (incomplete bootstrap set) |
#             4 prerequisite missing (kubectl) | 5 seeding failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.seedBootstrap" "Seed identity bootstrap values and the AI provider key before first boot."
cap_spec_param "namespace"         "kubernetes namespace"
cap_spec_param "secret-name"       "name of the secret to create"
cap_spec_param "domain"            "cluster deployment domain"
cap_spec_param "owner-email"       "cluster owner email"
cap_spec_param "owner-first-name"  "cluster owner first name"
cap_spec_param "owner-last-name"   "cluster owner last name"
cap_spec_param "registration-mode" "identity registration mode"
cap_spec_param "vendor"            "anthropic | openai"
cap_spec_param "api-key"           "AI provider API key"
cap_spec_param "kubectl-bin"       "path to the kubectl binary"
cap_spec_param "dry-run"           "report what would happen without writing (flag)"

KEYFILE=""
function cleanup() { [[ -n "$KEYFILE" && -f "$KEYFILE" ]] && rm -f "$KEYFILE"; }
trap cleanup EXIT

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local ns secret domain email first last regmode vendor key kubectl_bin dry
    ns="$(cap_param namespace "memql")"
    secret="$(cap_param secret-name "memql-identity-bootstrap")"
    domain="$(cap_param domain "")"
    email="$(cap_param owner-email "")"
    first="$(cap_param owner-first-name "")"
    last="$(cap_param owner-last-name "")"
    regmode="$(cap_param registration-mode "invite")"
    vendor="$(cap_param vendor "")"
    key="$(cap_param api-key "")"
    kubectl_bin="$(cap_param kubectl-bin "kubectl")"
    dry="$(cap_flag dry-run)"

    local missing=()
    [[ -n "$domain"  ]] || missing+=("--domain")
    [[ -n "$email"   ]] || missing+=("--owner-email")
    [[ -n "$first"   ]] || missing+=("--owner-first-name")
    [[ -n "$last"    ]] || missing+=("--owner-last-name")
    [[ -n "$regmode" ]] || missing+=("--registration-mode")
    if [[ ${#missing[@]} -gt 0 ]]; then
        cap_fail 2 "incomplete bootstrap set: missing ${missing[*]}. identity auto-bootstraps only when every field is present; a partial seed silently falls back to the interactive /setup page, which a headless install cannot complete."
    fi
    case "$vendor" in
        anthropic|openai) ;;
        *) cap_fail 2 "unknown vendor '${vendor}': expected anthropic or openai" ;;
    esac
    cap_require api-key "$key"

    local key_var
    if [[ "$vendor" == "anthropic" ]]; then
        key_var="MEMQL_AI_ANTHROPIC_API_KEY"
    else
        key_var="MEMQL_AI_OPENAI_API_KEY"
    fi

    cap_result_set namespace  "$ns"
    cap_result_set secretName "$secret"
    cap_result_set ownerEmail "$email"

    if [[ -n "$dry" ]]; then
        cap_info "DRY RUN: would seed ${secret} in ${ns} for owner ${email} at ${domain}"
        cap_result_set_raw seeded false
        cap_ok
    fi

    command -v "$kubectl_bin" &>/dev/null || [[ -x "$kubectl_bin" ]] \
        || cap_fail 4 "kubectl not found at '${kubectl_bin}'"

    # The key via --from-file so it is never an argv element.
    KEYFILE="$(mktemp)"
    chmod 0600 "$KEYFILE"
    printf '%s' "$key" > "$KEYFILE"

    # Recreate rather than patch: this runs once, before first boot, and a
    # partially-updated secret is the failure mode we are guarding against.
    "$kubectl_bin" create namespace "$ns" >&2 2>/dev/null || true
    "$kubectl_bin" delete secret "$secret" -n "$ns" >&2 2>/dev/null || true
    if ! "$kubectl_bin" create secret generic "$secret" -n "$ns" \
        --from-literal="MEMQL_IDENTITY_BOOTSTRAP_DOMAIN=${domain}" \
        --from-literal="MEMQL_IDENTITY_BOOTSTRAP_OWNER_EMAIL=${email}" \
        --from-literal="MEMQL_IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME=${first}" \
        --from-literal="MEMQL_IDENTITY_BOOTSTRAP_OWNER_LAST_NAME=${last}" \
        --from-literal="MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_MODE=${regmode}" \
        --from-file="${key_var}=${KEYFILE}" >&2; then
        cap_fail 5 "could not create secret ${secret} in ${ns}"
    fi

    cap_changed
    cap_info "seeded ${secret} in ${ns} for owner ${email}"
    cap_result_set_raw seeded true
    cap_ok
}

main "$@"
```

- [ ] **Step 4: Wire it into the graph, the DSL, and the allowlist**

In `component/automations/steps/capability_script.go`, add:

```go
	"install.seedBootstrap": "scripts/install/seed-bootstrap.sh",
```

In `scripts/install/graph/install.json`, add this step and add `"seedBootstrap"` to `clusterUp`'s `dependsOn`:

```json
    {
      "id": "seedBootstrap",
      "title": "Seed the cluster owner and provider key",
      "script": "install.seedBootstrap",
      "dependsOn": ["binaryKubectl", "verifyProviderKey", "mkcertCA"],
      "elevation": "none",
      "verify": { "kind": "resultTrue", "path": "seeded" }
    },
```

In `dsl/install/actions.memql`, add:

```memql
/// Seed the identity bootstrap values and the AI provider key before first boot, so the cluster
/// self-bootstraps with no operator visit to the setup page.
action seedClusterBootstrap {
  args {
    namespace        string
    domain           string!
    ownerEmail       string!
    ownerFirstName   string!
    ownerLastName    string!
    registrationMode string
    vendor           string!
    apiKey           string!
    dryRun           boolean
  }
  capability script(script: "install.seedBootstrap", namespace: args.namespace, domain: args.domain, ownerEmail: args.ownerEmail, ownerFirstName: args.ownerFirstName, ownerLastName: args.ownerLastName, registrationMode: args.registrationMode, vendor: args.vendor, apiKey: args.apiKey, dryRun: args.dryRun)
}
```

In `deploy/k8s/overlays/local`, add `envFrom: [{ secretRef: { name: memql-identity-bootstrap, optional: true } }]` to the identity Deployment and to the Deployments that consume the AI key. `optional: true` matters: an engine-only cluster brought up without this wizard must still start.

Add `"install.seedBootstrap": "scripts/install/seed-bootstrap.sh"` to the `ALLOWLIST` map in `editors/vscode/src/install/cli.ts`.

- [ ] **Step 5: Run the tests to verify they pass**

Run:
```bash
chmod +x scripts/install/seed-bootstrap.sh
go test ./scripts/... ./component/automations/steps/ -v
cd editors/vscode && npm test
```
Expected: PASS. `TestGraphAndDSLAgree` and `TestEveryScriptIsAllowlisted` both cover the new capability, so a missed wiring point fails here rather than at run time.

- [ ] **Step 6: Commit**

```bash
git add scripts/install/seed-bootstrap.sh scripts/install/seed_bootstrap_test.go \
        scripts/install/graph/install.json dsl/install/actions.memql \
        component/automations/steps/capability_script.go \
        editors/vscode/src/install/cli.ts deploy/k8s/overlays/local
git commit -m "$(cat <<'EOF'
feat(install): seed identity bootstrap values before first boot

identity auto-bootstraps only when domain, owner email, owner first/last
name and registration mode are ALL set when it first starts; miss one and
it waits for an operator on /setup, which a headless install can never
complete. So an incomplete set is exit 2, not a warning -- a partial seed
looks like it worked and yields a cluster nobody can sign into.

The API key goes in via --from-file: kubectl argv is visible in `ps` for
the same reason curl's is.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Done criteria for Epic 1

- `go test ./scripts/... ./component/automations/steps/` green.
- `cd editors/vscode && npm test` green.
- `go test ./test/dslconformance/` green.
- `node editors/vscode/dist-test/src/install/cli.js install` takes a clean Linux machine to a cluster you can sign into.
- `… cli.js uninstall` returns that machine to its baseline, proven by the E2E diff.
- Every install step that writes a receipt has a reversal, enforced by `TestEveryReceiptWritingStepHasAReversal`.

# Custom Local Domain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the local cluster's domain a value an operator chooses, so a memQL install serves `memql.localhost` by default and any domain the operator brings, with no file under `deploy/` naming a domain.

**Architecture:** One `MEMQL_DOMAIN` input, derived into every domain-shaped env var at the boot seam that already normalizes the environment (`genesis.ApplyLegacyEnvAliases`), so the four existing readers of the expected issuer need no edit. It reaches pods as a single-key ConfigMap the local overlay mounts via `envFrom`. The only two values that are Kubernetes API objects rather than process config — the two Ingress hostnames — ride `spec.source.kustomize.patches` on the ArgoCD Application, emitted by `k3d.up` only when the domain is overridden.

**Tech Stack:** Go 1.26.1, Bash capability scripts (`scripts/lib/capability.sh`), kustomize + ArgoCD v2.13.3, TypeScript (VS Code extension, `node --test`).

**Spec:** [`docs/superpowers/specs/2026-08-12-custom-local-domain-design.md`](../specs/2026-08-12-custom-local-domain-design.md)
**Issue:** memql#3593
**Branch:** `feat/custom-local-domain-3593` (worktree `.claude/worktrees/domain-3593`)

## Global Constraints

- **Go 1.26.1+.** Run `go test ./...` from the repo root.
- **No emojis** in any documentation, script output, or user-facing text. Use `SUCCESS:` / `ERROR:` / `WARNING:` / `INFO:` and `[ ]` / `[x]`.
- **Stage files by explicit path.** `git add <file>` only — never `git add -A` or `git add .`. Other sessions share this working tree.
- **Pre-release: no backwards-compat shims.** When a contract changes, change both sides and delete what is no longer needed. No legacy adapters, no "keep working while we migrate" layers.
- **Capability-script contract** for every script under `scripts/`: non-interactive, `--flag=value` params via `cap_param`, exactly one JSON envelope on stdout, human logs to stderr, exit codes 0 ok / 2 bad param / 3 refused / 4 prerequisite missing / 5 op failed. See `docs/internal/design/capability-script-contract.md`.
- **New env vars must be registered** in BOTH `component/envregistry/manifest.yaml` (embedded) and `scripts/secrets/manifest.yaml`, or the envscan drift gate fails CI.
- **The default domain is `memql.localhost`.** The apex plus `cockpit.` and `identity.` subdomains.
- **The TLS secret is renamed** `local-znas-tls` -> `memql-front-door-tls` everywhere in the same task that touches its consumers.
- **Cluster verification is not available on the authoring machine.** Do not claim a cluster-gated check passed. Tasks that need one say so and stop.

## File Structure

**Created**

| File | Responsibility |
|---|---|
| `component/envregistry/domain.go` | Derive every domain-shaped env var from `MEMQL_DOMAIN`, set-if-absent, at boot |
| `component/envregistry/domain_test.go` | Table tests for the derivation and explicit-wins |
| `scripts/lib/resolve.sh` | The one resolver ladder (getent/dig/host), shared by the hosts probe and the front-door verifier |
| `deploy/k8s/overlays/local/patches/domain-envfrom.yaml` | JSON 6902 append of `configMapRef: memql-domain` to every node's `envFrom` |
| `deploy/k8s/overlays/local/render_domain_test.go` | Renders the overlay and asserts no domain leaked and every node is wired |

**Modified**

| File | Change |
|---|---|
| `main.go:77`, `subcommand_env.go:43` | Call `genesis.ApplyDomainDerivations` after the legacy-alias shim |
| `component/envregistry/manifest.yaml`, `scripts/secrets/manifest.yaml` | Register three new env vars |
| `component/identity/config.go:989` | Narrow `isSingleProcessHost`; add the two extras |
| `deploy/k8s/overlays/local/kustomization.yaml` | Drop eight inline issuer patches; add the two new patch files |
| `deploy/k8s/overlays/local/patches/identity-local-config.yaml` | Strip six domain-bearing env vars; add the two extras |
| `deploy/k8s/overlays/local/{front-door,cockpit-front-door}.yaml` | Hosts to `memql.localhost`; secret rename |
| `scripts/lib/localtls.sh` | SANs derive from the domain; secret-name constant |
| `scripts/install/verify-frontdoor.sh` | Source `resolve.sh`; `--domain` default |
| `scripts/install/hosts-entries.sh` | `--domain`; probe before writing |
| `scripts/install/mkcert-setup.sh` | Re-issue when SANs do not cover the request |
| `scripts/k3d/{up,seed-secrets}.sh` | `--domain`; ConfigMap; Ingress patch emission; mismatch refusal |
| `Makefile` | `DOMAIN=` passthrough |
| `editors/vscode/src/install/stackPin.ts` | `DEFAULT_LOCAL_DOMAIN`; validator replaces the refusal |
| `editors/vscode/src/state/addCluster.ts` | New default |
| `editors/vscode/src/install/session.ts` | Thread `--domain` into `clusterUp` |
| Docs (Task 11) | Hostnames throughout |

---

### Task 1: Derive every domain-shaped env var from `MEMQL_DOMAIN`

**Files:**
- Create: `component/envregistry/domain.go`
- Test: `component/envregistry/domain_test.go`
- Modify: `main.go:77`, `subcommand_env.go:43`, `component/envregistry/manifest.yaml`, `scripts/secrets/manifest.yaml`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `genesis.DomainDerivations(domain string) map[string]string` — pure, returns the derived name/value pairs for a domain; empty map for an empty or malformed domain.
  - `genesis.ApplyDomainDerivations(logger *slog.Logger)` — reads `MEMQL_DOMAIN`, applies each derivation set-if-absent, logs which were filled.
  - Env names produced: `MEMQL_IDENTITY_BASE_URL`, `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER`, `MEMQL_IDENTITY_BOOTSTRAP_DOMAIN`, `MEMQL_DISCOVERY_GRPC_ENDPOINT`, `MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS`, `MEMQL_IDENTITY_REGISTERED_CLIENTS`.

- [ ] **Step 1: Write the failing test**

Create `component/envregistry/domain_test.go`:

```go
package genesis

import (
	"os"
	"strings"
	"testing"
)

func TestDomainDerivations(t *testing.T) {
	got := DomainDerivations("memql.localhost")

	want := map[string]string{
		"MEMQL_IDENTITY_BASE_URL":                 "https://identity.memql.localhost",
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity.memql.localhost",
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":         "memql.localhost",
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "cockpit.memql.localhost:443",
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":     "https://cockpit.memql.localhost,https://app.memql.localhost",
	}
	for name, wantVal := range want {
		if got[name] != wantVal {
			t.Errorf("%s = %q, want %q", name, got[name], wantVal)
		}
	}

	clients := got["MEMQL_IDENTITY_REGISTERED_CLIENTS"]
	for _, want := range []string{
		`"clientId":"cockpit"`,
		`"clientId":"portal"`,
		`"clientId":"app"`,
		"https://cockpit.memql.localhost/portal/auth/callback",
		"https://app.memql.localhost/auth/callback",
		"http://127.0.0.1/cockpit/callback",
	} {
		if !strings.Contains(clients, want) {
			t.Errorf("registered clients %q missing %q", clients, want)
		}
	}
}

// A domain that is not a domain derives NOTHING. Deriving from garbage would
// mint an issuer no client can reach and fail later, at sign-in, as an
// unrelated-looking error.
func TestDomainDerivationsRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "localhost", "https://memql.localhost",
		"memql.localhost:443", "MEMQL.localhost", "memql.localhost.",
		"*.memql.localhost", "127.0.0.1", "memql..localhost",
	} {
		if got := DomainDerivations(bad); len(got) != 0 {
			t.Errorf("DomainDerivations(%q) = %v, want empty", bad, got)
		}
	}
}

// Set-if-absent. An explicitly configured value is a statement of intent and
// always wins -- this is what keeps staging and prod untouched.
func TestApplyDomainDerivationsExplicitWins(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://auth.example.com")

	ApplyDomainDerivations(nil)

	if got := os.Getenv("MEMQL_IDENTITY_BASE_URL"); got != "https://auth.example.com" {
		t.Errorf("BASE_URL = %q, want the explicit value untouched", got)
	}
	if got := os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN"); got != "memql.localhost" {
		t.Errorf("BOOTSTRAP_DOMAIN = %q, want it derived", got)
	}
}

// No domain, no derivation. A node configured entirely by explicit env keeps
// behaving exactly as it does today.
func TestApplyDomainDerivationsNoopWithoutDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	t.Setenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN", "")

	ApplyDomainDerivations(nil)

	if got := os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN"); got != "" {
		t.Errorf("BOOTSTRAP_DOMAIN = %q, want empty", got)
	}
}

// Idempotent, like ApplyLegacyEnvAliases: a second call finds every name
// populated and changes nothing.
func TestApplyDomainDerivationsIdempotent(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "")

	ApplyDomainDerivations(nil)
	first := os.Getenv("MEMQL_IDENTITY_BASE_URL")
	ApplyDomainDerivations(nil)

	if got := os.Getenv("MEMQL_IDENTITY_BASE_URL"); got != first {
		t.Errorf("second call changed BASE_URL: %q -> %q", first, got)
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./component/genesis/ -run TestDomain -v`
Expected: FAIL — `undefined: DomainDerivations`.

- [ ] **Step 3: Write the implementation**

Create `component/envregistry/domain.go`:

```go
package genesis

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
)

// MEMQL_DOMAIN is the ONE input from which every domain-shaped env var is
// derived. A deployment states its domain once; identity's base URL, the
// issuer every node verifies against, the discovery endpoint, the CORS
// origins and the OAuth redirect URIs all follow from it.
//
// WHY DERIVE RATHER THAN CONFIGURE EACH. Those six values are one fact spelled
// six ways, and the local overlay used to spell it six times plus twice more in
// its Ingresses. Missing one is not a visible failure: memql#3315 was a single
// forgotten CORS origin, and it presented as sign-in dying with an empty
// identity log. One input with a tested derivation cannot drift against itself.
//
// SET-IF-ABSENT, ALWAYS. An explicitly configured value is a statement of
// intent and wins. That is what lets staging and prod -- which set every one of
// these explicitly -- carry MEMQL_DOMAIN or not, and be entirely unaffected
// either way.
//
// Call this AFTER ApplyLegacyEnvAliases (so a legacy MEMQL_DOMAIN spelling is
// already bridged) and BEFORE any component reads its config.
//
// Refs: memql#3593 memql#3590 memql#3315

// domainPattern is what a domain may look like: two or more lowercase labels,
// no scheme, no port, no wildcard, no trailing dot. Deliberately strict --
// deriving an issuer from garbage produces a cluster that boots and then cannot
// be signed into, which is a worse failure than refusing to derive.
var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// DomainDerivations returns the env values a domain implies. An empty map means
// the domain was empty or malformed; callers derive nothing rather than
// deriving something wrong.
func DomainDerivations(domain string) map[string]string {
	d := strings.TrimSpace(domain)
	if !domainPattern.MatchString(d) {
		return map[string]string{}
	}

	identity := "https://identity." + d
	cockpit := "https://cockpit." + d
	app := "https://app." + d

	// The cockpit client is loopback BY DESIGN (RFC 8252 native-client
	// redirect), so it carries no domain and is listed here unchanged.
	clients := fmt.Sprintf(
		`[{"clientId":"app","redirectURIs":["%s/auth/callback"]},`+
			`{"clientId":"cockpit","redirectURIs":["http://127.0.0.1/cockpit/callback","http://localhost/cockpit/callback"]},`+
			`{"clientId":"portal","redirectURIs":["%s/portal/auth/callback"]}]`,
		app, cockpit)

	return map[string]string{
		"MEMQL_IDENTITY_BASE_URL":                 identity,
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": identity,
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":         d,
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "cockpit." + d + ":443",
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":     cockpit + "," + app,
		"MEMQL_IDENTITY_REGISTERED_CLIENTS":       clients,
	}
}

// ApplyDomainDerivations paints the derived values onto the process
// environment, set-if-absent. Idempotent: a second call finds every name
// populated and does nothing.
func ApplyDomainDerivations(logger *slog.Logger) {
	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	if domain == "" {
		return
	}

	derived := DomainDerivations(domain)
	if len(derived) == 0 {
		if logger != nil {
			logger.Warn("MEMQL_DOMAIN is not a usable domain; deriving nothing",
				"domain", domain)
		}
		return
	}

	// Deterministic order so the log line is stable run-to-run.
	names := make([]string, 0, len(derived))
	for name := range derived {
		names = append(names, name)
	}
	sort.Strings(names)

	filled := make([]string, 0, len(names))
	for _, name := range names {
		if os.Getenv(name) != "" {
			continue // explicit wins
		}
		if err := os.Setenv(name, derived[name]); err != nil {
			if logger != nil {
				logger.Warn("failed to apply domain derivation", "name", name, "err", err)
			}
			continue
		}
		filled = append(filled, name)
	}

	if logger != nil && len(filled) > 0 {
		logger.Info("domain derivations applied", "domain", domain, "vars", filled)
	}
}
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./component/genesis/ -run TestDomain -v`
Expected: PASS (all five tests).

- [ ] **Step 5: Wire it into both boot seams**

In `main.go`, immediately after the `genesis.ApplyLegacyEnvAliases(serviceLogger)` call (line 77), add:

```go
	// One domain in, every domain-shaped env var out (memql#3593). Runs after
	// the alias shim so a bridged MEMQL_DOMAIN is already on its new name, and
	// before app.Run so every component reads the derived values. Set-if-absent,
	// so a deployment that configures these explicitly is untouched.
	genesis.ApplyDomainDerivations(serviceLogger)
```

In `subcommand_env.go`, immediately after `genesis.ApplyLegacyEnvAliases(nil)` (line 43), add:

```go
	// Mirrors main(): `memql env` must report the environment a node actually
	// boots with, derivations included. nil logger -- this path writes to
	// stderr via Fprintf, not slog.
	genesis.ApplyDomainDerivations(nil)
```

- [ ] **Step 6: Register the new env var**

In BOTH `component/envregistry/manifest.yaml` and `scripts/secrets/manifest.yaml`, add an entry in the alphabetically correct position (the file is sorted by `name`):

```yaml
  - name: MEMQL_DOMAIN
    component: platform
    scope: node
    optional: true
    description: "The cluster's domain. Every domain-shaped identity value (base URL, expected issuer, discovery endpoint, CORS origins, OAuth redirect URIs) is derived from it set-if-absent at boot; an explicitly configured value always wins."
```

Find the position with `grep -n "name: MEMQL_D" component/envregistry/manifest.yaml`.

- [ ] **Step 7: Run the drift gate and the full genesis suite**

Run: `go test ./component/genesis/... ./cmd/envscan/... -v`
Expected: PASS. If `TestNoEnvRegistryDrift` fails naming `MEMQL_DOMAIN`, the manifest entry is missing or in the wrong file — fix and re-run.

- [ ] **Step 8: Build every node type to confirm the seam compiles under all tags**

Run:
```bash
go build -o /dev/null . && \
for t in identity bff cognition agent planner workbench mcp; do \
  go build -tags "$t" -o /dev/null . || echo "FAILED: $t"; \
done
```
Expected: no output.

- [ ] **Step 9: Commit**

```bash
git add component/envregistry/domain.go component/envregistry/domain_test.go \
        component/envregistry/manifest.yaml scripts/secrets/manifest.yaml \
        main.go subcommand_env.go
git commit -m "Issue #3593: derive every domain-shaped env var from MEMQL_DOMAIN"
```

---

### Task 2: Stop claiming a `.localhost` host cannot have a second replica

**Files:**
- Modify: `component/identity/config.go:976-996`
- Test: `component/identity/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `isSingleProcessHost` returns true for exactly `localhost`, `127.0.0.1`, `::1`, `0.0.0.0` (port-stripped). No behavioural surface outside the package.

**Context for the implementer.** `isSingleProcessHost` gates the fail-fast guard added in memql#3400, after the 2026-06-16 auth outage (memql#1515) where two identity replicas minted different ephemeral signing keys and roughly half of all token verifications failed with `unknown kid`. It currently exempts any host ending `.localhost` on the premise that "nothing behind such a host can be a second replica". Once the local front door is served at `*.memql.localhost`, traefik proxies that name to a k8s Service and `make scale N=2` puts two identity pods behind it, so the premise is false. Nothing is broken today — both guards sit inside the `else` branch that runs only when `MEMQL_IDENTITY_SIGNING_KEY_B64` is unset, and `seed-secrets.sh` always seeds it — but the claim must stop being made before the domain moves.

- [ ] **Step 1: Write the failing test**

Append to `component/identity/config_test.go`:

```go
// A hostname never told you how many processes are behind it.
//
// This function gates the memql#3400 guard, whose whole subject is "can there
// be more than one of me?". `identity.memql.localhost` is the local cluster's
// traefik front door; `make scale N=2` puts two identity pods behind it. Any
// answer that says otherwise re-opens the memql#1515 outage.
func TestIsSingleProcessHostIsLoopbackNamesOnly(t *testing.T) {
	single := []string{"localhost", "127.0.0.1", "::1", "0.0.0.0", "localhost:8085"}
	for _, host := range single {
		if !isSingleProcessHost(host) {
			t.Errorf("isSingleProcessHost(%q) = false, want true", host)
		}
	}

	notSingle := []string{
		"identity.memql.localhost",
		"cockpit.memql.localhost",
		"memql.localhost",
		"identity.local.znas.io",
		"identity.example.com",
	}
	for _, host := range notSingle {
		if isSingleProcessHost(host) {
			t.Errorf("isSingleProcessHost(%q) = true, want false -- a front door is not one process", host)
		}
	}
}

// The guard it gates must actually fire for the new local domain when no
// shared seed is configured.
func TestEphemeralKeyGuardFiresForLocalhostSubdomain(t *testing.T) {
	cfg := &Config{
		Enabled: true,
		BaseURL: "https://identity.memql.localhost",
		// SigningKeyB64 unset -- the branch the guard lives in.
		//
		// devEncKey, not a literal: the file builds its placeholder at runtime
		// with strings.Repeat precisely so a secret scanner has no fixed
		// high-entropy string to flag. A literal here trips gitleaks.
		KeyEncryptionKey: devEncKey,
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "MEMQL_IDENTITY_SIGNING_KEY_B64") {
		t.Fatalf("Validate() = %v, want the shared-signing-key refusal", err)
	}
}
```

If `Config` construction or the validate entry point differs from the above, read the neighbouring tests in the same file and match them exactly rather than inventing a shape — the assertion is about the error, not about how the config is built.

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./component/identity/ -run 'TestIsSingleProcessHost|TestEphemeralKeyGuardFires' -v`
Expected: FAIL — `isSingleProcessHost("identity.memql.localhost") = true`.

- [ ] **Step 3: Narrow the function**

Replace the body and comment of `isSingleProcessHost` in `component/identity/config.go`:

```go
// isSingleProcessHost returns true only for hosts that name a single process on
// the local machine BY CONSTRUCTION: the literal loopback names.
//
// It used to also accept the whole `.localhost` TLD, on the reading that RFC
// 6761 makes it resolve to loopback and therefore nothing behind it can be a
// second replica. The first half is true and the second does not follow. The
// local cluster's front door is served at `*.memql.localhost` (memql#3593):
// traefik terminates there and proxies to a k8s Service, and `make scale N=2`
// puts two identity pods behind it. That is precisely the topology this guard
// exists to refuse, and it is the same mistake the `*.local.<domain>` wildcard
// caused in memql#3400 -- a hostname never told you how many processes are
// behind it.
//
// A genuinely single-process deployment on a `.localhost` name opts in
// explicitly with MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true.
//
// Any check whose real question is "can there be more than one of me?" must use
// this, not isLocalHost.
func isSingleProcessHost(host string) bool {
	switch hostWithoutPort(host) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./component/identity/ -run 'TestIsSingleProcessHost|TestEphemeralKeyGuardFires' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole identity suite and fix the fallout**

Run: `go test ./component/identity/...`
Expected: PASS. Existing tests that relied on a `.localhost` subdomain being exempt must now either set `MEMQL_IDENTITY_SIGNING_KEY_B64`, set `MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY=true`, or use a bare loopback name. Change the test's setup, never the guard.

- [ ] **Step 6: Update the error message's parenthetical**

The refusal string in `config.go` says `Only a loopback issuer (localhost / 127.0.0.1 / *.localhost), which cannot have a second replica, is exempt`. Remove `/ *.localhost` from that sentence so the message and the code agree.

- [ ] **Step 7: Commit**

```bash
git add component/identity/config.go component/identity/config_test.go
git commit -m "Issue #3593: a .localhost front door is not one process"
```

---

### Task 3: Domain-free extras for the local dev-server origins

**Files:**
- Modify: `component/identity/config.go` (near `envStringList` / `envRegisteredClients` at lines 698-708)
- Test: `component/identity/config_test.go`
- Modify: `component/envregistry/manifest.yaml`, `scripts/secrets/manifest.yaml`

**Interfaces:**
- Consumes: `genesis.DomainDerivations` (Task 1) indirectly, via the environment.
- Produces: two env vars read by identity — `MEMQL_IDENTITY_CORS_EXTRA_ORIGINS` (comma-separated origins, appended to `CORSAllowedOrigins`) and `MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS` (same JSON array shape as `MEMQL_IDENTITY_REGISTERED_CLIENTS`, appended to `RegisteredClients`; a duplicate `clientId` has its redirect URIs merged into the existing entry).

**Why these exist.** The local overlay admits `http://localhost:8080` and `http://localhost:3000`, the vite dev servers for the portal and the product SPA. They are not domain-shaped, and folding them into the derived default would mean a staging operator who forgot to set CORS silently gets localhost origins allowed. Two domain-free knobs, set only in the local overlay, keep the derivation production-honest.

- [ ] **Step 1: Write the failing test**

Append to `component/identity/config_test.go`:

```go
func TestCORSExtraOriginsAppend(t *testing.T) {
	t.Setenv("MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS", "https://cockpit.memql.localhost")
	t.Setenv("MEMQL_IDENTITY_CORS_EXTRA_ORIGINS", "http://localhost:8080,http://localhost:3000")

	cfg := LoadConfig()

	want := []string{
		"https://cockpit.memql.localhost",
		"http://localhost:8080",
		"http://localhost:3000",
	}
	if len(cfg.CORSAllowedOrigins) != len(want) {
		t.Fatalf("CORSAllowedOrigins = %v, want %v", cfg.CORSAllowedOrigins, want)
	}
	for i, w := range want {
		if cfg.CORSAllowedOrigins[i] != w {
			t.Errorf("origin[%d] = %q, want %q", i, cfg.CORSAllowedOrigins[i], w)
		}
	}
}

// A client id in both lists is ONE client with more redirect URIs, not two
// clients. Two entries with the same id would make ClientByID's answer depend
// on list order, which is how a redirect URI silently stops being accepted.
func TestExtraRegisteredClientsMergeById(t *testing.T) {
	t.Setenv("MEMQL_IDENTITY_REGISTERED_CLIENTS",
		`[{"clientId":"portal","redirectURIs":["https://cockpit.memql.localhost/portal/auth/callback"]}]`)
	t.Setenv("MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS",
		`[{"clientId":"portal","redirectURIs":["http://localhost:3000/auth/callback"]},`+
			`{"clientId":"devtool","redirectURIs":["http://localhost:9999/cb"]}]`)

	cfg := LoadConfig()

	portal := cfg.ClientByID("portal")
	if portal == nil {
		t.Fatal("portal client missing")
	}
	if len(portal.RedirectURIs) != 2 {
		t.Errorf("portal redirect URIs = %v, want both merged", portal.RedirectURIs)
	}
	if cfg.ClientByID("devtool") == nil {
		t.Error("devtool client missing -- an id present only in the extras must still register")
	}
}
```

`LoadConfig` and `ClientByID` are the names used in the neighbouring tests; if this file spells them differently, match the file.

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./component/identity/ -run 'TestCORSExtraOrigins|TestExtraRegisteredClients' -v`
Expected: FAIL — the extras are ignored, so the lengths are 1 and 1.

- [ ] **Step 3: Implement the append**

In `component/identity/config.go`, after `cfg.CORSAllowedOrigins = envStringList("MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS")` (line 698):

```go
	// Domain-free additions to the derived-or-explicit set (memql#3593).
	//
	// The local overlay admits the vite dev servers (localhost:8080 / :3000).
	// Those are not domain-shaped, so folding them into what MEMQL_DOMAIN
	// derives would mean a staging deployment that forgot to set CORS silently
	// admits localhost. A separate knob keeps the derivation production-honest.
	cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins,
		envStringList("MEMQL_IDENTITY_CORS_EXTRA_ORIGINS")...)
```

After `cfg.RegisteredClients = clients` (line 708):

```go
	// Same reasoning as CORS_EXTRA_ORIGINS. Merged BY clientId: a client in
	// both lists is one client with more redirect URIs. Two entries sharing an
	// id would make ClientByID's answer depend on list order.
	extras, err := envRegisteredClients("MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS")
	if err != nil {
		return nil, fmt.Errorf("MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS: %w", err)
	}
	for _, extra := range extras {
		merged := false
		for i := range cfg.RegisteredClients {
			if cfg.RegisteredClients[i].ClientId != extra.ClientId {
				continue
			}
			cfg.RegisteredClients[i].RedirectURIs = append(
				cfg.RegisteredClients[i].RedirectURIs, extra.RedirectURIs...)
			merged = true
			break
		}
		if !merged {
			cfg.RegisteredClients = append(cfg.RegisteredClients, extra)
		}
	}
```

Match the surrounding error-return convention: if the enclosing function does not return an error, follow whatever `envRegisteredClients`'s existing caller does with its error.

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./component/identity/ -run 'TestCORSExtraOrigins|TestExtraRegisteredClients' -v`
Expected: PASS.

- [ ] **Step 5: Register both env vars**

Add to BOTH manifests, alphabetically placed:

```yaml
  - name: MEMQL_IDENTITY_CORS_EXTRA_ORIGINS
    component: identity
    scope: node
    optional: true
    description: "Comma-separated CORS origins appended to the derived-or-explicit allowed set; domain-free additions such as local dev servers."

  - name: MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS
    component: identity
    scope: node
    optional: true
    description: "JSON array of OAuth clients appended to the derived-or-explicit set, merged by clientId; domain-free additions such as local dev servers."
```

- [ ] **Step 6: Run the suite and the drift gate**

Run: `go test ./component/identity/... ./component/genesis/... ./cmd/envscan/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add component/identity/config.go component/identity/config_test.go \
        component/envregistry/manifest.yaml scripts/secrets/manifest.yaml
git commit -m "Issue #3593: domain-free CORS and client extras"
```

---

### Task 4: One resolver ladder, shared

**Files:**
- Create: `scripts/lib/resolve.sh`
- Modify: `scripts/install/verify-frontdoor.sh:100-131`
- Test: `scripts/install/resolve_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces, for Task 5:
  - `resolver_tool()` — prints `getent` | `dig` | `host` | empty.
  - `resolve_addresses <host>` — prints each resolved IPv4 address on its own line; empty output means the name did not resolve.
  - `MEMQL_RESOLVE_STUB` — when set to a path, `resolve_addresses` reads `<stub>/<host>` instead of the network. Absent file means "does not resolve". This is the hook the hosts-probe tests use.

**Why.** `verify-frontdoor.sh` owns the only resolver ladder in the tree, and Task 5 needs the same one. Two copies of a resolution rule is exactly how memql#3384 happened — two scripts independently defaulting to the same path, then drifting.

- [ ] **Step 1: Write the failing test**

Create `scripts/install/resolve_test.go`:

```go
// Package install holds tests for the local-install capability scripts.
package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// resolveHarness runs a snippet with scripts/lib/resolve.sh sourced.
func resolveHarness(t *testing.T, stubDir, snippet string) string {
	t.Helper()
	lib, err := filepath.Abs(filepath.Join("..", "lib", "resolve.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "set -euo pipefail; source '"+lib+"'; "+snippet)
	cmd.Env = append(os.Environ(), "MEMQL_RESOLVE_STUB="+stubDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestResolveStubReturnsAddresses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cockpit.memql.localhost"),
		[]byte("127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := strings.TrimSpace(resolveHarness(t, dir, `resolve_addresses cockpit.memql.localhost`))
	if got != "127.0.0.1" {
		t.Errorf("resolve_addresses = %q, want 127.0.0.1", got)
	}
}

func TestResolveStubUnknownHostIsEmpty(t *testing.T) {
	got := strings.TrimSpace(resolveHarness(t, t.TempDir(), `resolve_addresses nothing.invalid || true`))
	if got != "" {
		t.Errorf("resolve_addresses = %q, want empty for an unknown host", got)
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./scripts/install/ -run TestResolve -v`
Expected: FAIL — `scripts/lib/resolve.sh` does not exist.

- [ ] **Step 3: Create the library**

Create `scripts/lib/resolve.sh`:

```bash
#!/usr/bin/env bash
#
# scripts/lib/resolve.sh
# ======================
#
# ONE definition of how this repository resolves a hostname to IPv4 addresses,
# shared by the two scripts that need to know:
#
#   scripts/install/verify-frontdoor.sh   the front-door DNS check
#   scripts/install/hosts-entries.sh      the probe that decides whether the
#                                         hosts block needs writing at all
#
# WHY THIS FILE EXISTS. It is the localtls.sh lesson applied before it bites.
# Two scripts independently spelling the same rule is how memql#3384 happened:
# both defaulted to the same certificate path, the path moved, and one of them
# went on reporting success. The hosts probe and the front-door verifier must
# agree about what "resolves to 127.0.0.1" means, because one decides whether
# to write the entry the other then checks.
#
# TESTING HOOK. MEMQL_RESOLVE_STUB names a directory of files, one per hostname,
# each holding the addresses that name resolves to. When it is set,
# resolve_addresses reads the directory instead of the network -- which is what
# makes the three-outcome probe testable without DNS. A missing file means the
# name does not resolve. Never set in production paths.
#
# This is NOT a capability script -- it declares functions and is sourced.

if [[ -n "${_MEMQL_RESOLVE_LIB_LOADED:-}" ]]; then
    return 0 2>/dev/null || exit 0
fi
_MEMQL_RESOLVE_LIB_LOADED=1

# resolver_tool -- names the resolver this machine actually has. getent is the
# Linux/glibc path; dig and host cover macOS, where getent does not exist.
function resolver_tool() {
    if [[ -n "${MEMQL_RESOLVE_STUB:-}" ]]; then printf 'stub'; return; fi
    if command -v getent &>/dev/null; then printf 'getent'; return; fi
    if command -v dig    &>/dev/null; then printf 'dig';    return; fi
    if command -v host   &>/dev/null; then printf 'host';   return; fi
    printf ''
}

# resolve_addresses <host> -- prints each resolved IPv4 address on its own line.
# Empty output means the name did not resolve.
function resolve_addresses() {
    local host="$1"
    case "$(resolver_tool)" in
        stub)   [[ -f "${MEMQL_RESOLVE_STUB}/${host}" ]] \
                    && grep -E '^[0-9]+(\.[0-9]+){3}$' "${MEMQL_RESOLVE_STUB}/${host}" | sort -u ;;
        getent) getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u ;;
        dig)    dig +short A "$host" 2>/dev/null | grep -E '^[0-9]+(\.[0-9]+){3}$' | sort -u ;;
        host)   host -t A "$host" 2>/dev/null | awk '/has address/ {print $NF}' | sort -u ;;
    esac
    return 0
}
```

Make it executable-adjacent but sourced: `chmod 0644 scripts/lib/resolve.sh` (it is sourced, not run — match `scripts/lib/localtls.sh`'s mode with `ls -l scripts/lib/localtls.sh`).

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./scripts/install/ -run TestResolve -v`
Expected: PASS.

- [ ] **Step 5: Make verify-frontdoor.sh use it**

In `scripts/install/verify-frontdoor.sh`, delete the local `resolver_tool` and `resolve_addresses` definitions (lines ~100-131) and add near the other `source` lines:

```bash
# shellcheck source=../lib/resolve.sh
source "${SCRIPT_DIR}/../lib/resolve.sh"
```

Leave `check_prerequisites` calling `resolver_tool` exactly as it does now — the function name is unchanged, so nothing else in the file moves.

- [ ] **Step 6: Run the front-door tests**

Run: `go test ./scripts/install/ -v`
Expected: PASS, including the existing `verify_frontdoor_test.go`.

- [ ] **Step 7: Commit**

```bash
git add scripts/lib/resolve.sh scripts/install/resolve_test.go scripts/install/verify-frontdoor.sh
git commit -m "Issue #3593: one resolver ladder, shared by the probe and the verifier"
```

---

### Task 5: The hosts block is written only when it is needed

**Files:**
- Modify: `scripts/install/hosts-entries.sh`
- Test: `scripts/install/hosts_entries_test.go`

**Interfaces:**
- Consumes: `resolve_addresses` from `scripts/lib/resolve.sh` (Task 4).
- Produces: `install.hostsEntries` accepts `--domain=<apex>` (deriving `cockpit.<apex>`, `identity.<apex>`, `<apex>`) alongside the existing `--hostnames`; its result envelope gains `skipped` (bool) and `probe` (string: `absent` | `satisfied` | `conflict`).

**The three outcomes.** Every hostname resolves to 127.0.0.1 and nothing else -> write nothing, take no elevation, `skipped: true`. No hostname resolves -> write the block as today. Mixed, or any hostname resolving elsewhere -> refuse with exit 3, naming the hostname and what it answered. Writing a block that shadows a record the operator may depend on is the wrong repair; `verify-frontdoor.sh` already states the principle — "a hostname pointing at some other address is a worse failure than one that does not resolve."

- [ ] **Step 1: Write the failing test**

Append to `scripts/install/hosts_entries_test.go` (match the file's existing helper for invoking the script and parsing the envelope; the snippet below assumes helpers named `runCapability` returning stdout/exit code — read the file and use what is there):

```go
// The probe decides whether the block is needed at all (memql#3593).
//
// An operator who pointed a real wildcard A record at 127.0.0.1 has already
// done what the hosts block does. Writing it anyway costs them a sudo prompt
// for no effect, and the install wizard's whole claim is that elevation appears
// only where it does something.
func TestHostsEntriesSkipsWhenAlreadyResolving(t *testing.T) {
	stub := t.TempDir()
	for _, h := range []string{"cockpit.lab.example.com", "identity.lab.example.com", "lab.example.com"} {
		if err := os.WriteFile(filepath.Join(stub, h), []byte("127.0.0.1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hostsFile := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hostsFile, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(), "MEMQL_RESOLVE_STUB="+stub)
	out, code := runHostsEntriesEnv(t, env,
		"--action=add", "--domain=lab.example.com",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, `"skipped":true`) {
		t.Errorf("envelope missing skipped:true\n%s", out)
	}
	body, _ := os.ReadFile(hostsFile)
	if strings.Contains(string(body), "BEGIN memql") {
		t.Errorf("hosts file was written despite the names already resolving:\n%s", body)
	}
}

// A hostname answering somewhere else is refused, not overwritten.
func TestHostsEntriesRefusesConflictingResolution(t *testing.T) {
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "cockpit.lab.example.com"),
		[]byte("203.0.113.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostsFile := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hostsFile, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(), "MEMQL_RESOLVE_STUB="+stub)
	out, code := runHostsEntriesEnv(t, env,
		"--action=add", "--domain=lab.example.com",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused)\n%s", code, out)
	}
	if !strings.Contains(out, "203.0.113.7") {
		t.Errorf("refusal does not name the offending address\n%s", out)
	}
	body, _ := os.ReadFile(hostsFile)
	if strings.Contains(string(body), "BEGIN memql") {
		t.Errorf("hosts file was written on a refused run:\n%s", body)
	}
}

// Nothing resolves: the block is written, exactly as before this change.
func TestHostsEntriesWritesWhenNothingResolves(t *testing.T) {
	hostsFile := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hostsFile, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(), "MEMQL_RESOLVE_STUB="+t.TempDir())
	out, code := runHostsEntriesEnv(t, env,
		"--action=add", "--domain=memql.localhost",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	body, _ := os.ReadFile(hostsFile)
	for _, want := range []string{"cockpit.memql.localhost", "identity.memql.localhost", "memql.localhost"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("hosts file missing %q:\n%s", want, body)
		}
	}
}

// --domain and --hostnames are two spellings of one answer. Both is a
// contradiction the script must not silently resolve.
func TestHostsEntriesRefusesBothDomainAndHostnames(t *testing.T) {
	hostsFile := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hostsFile, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runHostsEntriesEnv(t, os.Environ(),
		"--action=add", "--domain=lab.example.com", "--hostnames=a.example.com",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (bad param)\n%s", code, out)
	}
}
```

If the file has no `runHostsEntriesEnv`, add one modelled on its existing runner — the only difference is passing an explicit `env`.

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./scripts/install/ -run TestHostsEntries -v`
Expected: FAIL — `--domain` is an unknown parameter.

- [ ] **Step 3: Implement the domain param and the probe**

In `scripts/install/hosts-entries.sh`:

Add the source and the param spec near the existing ones:

```bash
# shellcheck source=../lib/resolve.sh
source "${SCRIPT_DIR}/../lib/resolve.sh"
```

```bash
cap_spec_param "domain"     "front-door apex; derives cockpit.<d>, identity.<d>, <d> (mutually exclusive with --hostnames)"
```

Change the default hostnames constant to derive from the default domain:

```bash
# The local front door. Keep in step with deploy/k8s/overlays/local.
readonly DEFAULT_DOMAIN="memql.localhost"
readonly DEFAULT_HOSTNAMES="cockpit.${DEFAULT_DOMAIN},identity.${DEFAULT_DOMAIN},${DEFAULT_DOMAIN}"
```

Add the derivation and the probe as functions:

```bash
# hostnames_for_domain <apex> -- the three names a front door puts on a domain,
# apex last to match the block this script documents.
function hostnames_for_domain() {
    local apex="$1"
    printf 'cockpit.%s,identity.%s,%s' "$apex" "$apex" "$apex"
}

# probe_hostnames -- decides whether the block is needed. Prints one of
# `absent`, `satisfied`, `conflict`; on conflict, sets _PROBE_DETAIL.
#
# THREE OUTCOMES, NOT TWO. An operator whose own DNS already answers 127.0.0.1
# has done this job; writing anyway costs a sudo prompt for no effect. A name
# answering somewhere ELSE is neither -- shadowing a record they may depend on
# is the wrong repair, so it is refused rather than overwritten.
_PROBE_DETAIL=""
function probe_hostnames() {
    local host addrs addr resolved=0 unresolved=0
    _PROBE_DETAIL=""
    for host in "${HOSTNAMES[@]}"; do
        addrs="$(resolve_addresses "$host")"
        if [[ -z "$addrs" ]]; then
            unresolved=$((unresolved + 1))
            continue
        fi
        while IFS= read -r addr; do
            [[ -z "$addr" ]] && continue
            if [[ "$addr" != "$IP" ]]; then
                _PROBE_DETAIL="${host} resolves to ${addr}"
                printf 'conflict'
                return 0
            fi
        done <<< "$addrs"
        resolved=$((resolved + 1))
    done

    if [[ "$resolved" -gt 0 && "$unresolved" -gt 0 ]]; then
        _PROBE_DETAIL="${resolved} of ${#HOSTNAMES[@]} front-door names already resolve to ${IP} and the rest do not"
        printf 'conflict'
        return 0
    fi
    if [[ "$unresolved" -eq 0 ]]; then
        printf 'satisfied'
        return 0
    fi
    printf 'absent'
}
```

In `main`, after the existing `hostnames_raw` line, resolve the two spellings and refuse both:

```bash
    domain="$(cap_param domain "")"
    hostnames_raw="$(cap_param hostnames "")"
    if [[ -n "$domain" && -n "$hostnames_raw" ]]; then
        cap_fail 2 "--domain and --hostnames are two spellings of one answer; pass one"
    fi
    if [[ -n "$domain" ]]; then
        hostnames_raw="$(hostnames_for_domain "$domain")"
    elif [[ -z "$hostnames_raw" ]]; then
        hostnames_raw="$DEFAULT_HOSTNAMES"
    fi
```

After `parse_hostnames` / `validate_ip` / the confirm gate and BEFORE `check_hosts_file`, run the probe for the add path only:

```bash
    local probe="absent"
    if [[ "$mode" == "upsert" ]]; then
        probe="$(probe_hostnames)"
        case "$probe" in
            conflict)
                cap_fail 3 "refusing to write hosts entries: ${_PROBE_DETAIL} -- fix the DNS record or pass --hostnames naming only what needs an entry"
                ;;
            satisfied)
                cap_info "every front-door hostname already resolves to ${IP} -- nothing to write"
                ;;
        esac
    fi
```

and short-circuit the write when satisfied, reporting the same envelope shape with `wrote=false`, `skipped=true`, `blockPresent=true`:

```bash
    local skipped=false
    if [[ "$probe" == "satisfied" ]]; then
        skipped=true
    else
        # ... the existing check_hosts_file / read_hosts_file / scan_blocks /
        #     apply block, unchanged ...
    fi
```

Add the two new fields beside the existing `cap_result_set_raw` calls:

```bash
    cap_result_set_raw skipped "$skipped"
    cap_result_set     probe   "$probe"
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./scripts/install/ -run TestHostsEntries -v`
Expected: PASS.

- [ ] **Step 5: Run the capability-contract gate**

Run: `go test ./scripts/lib/ -v`
Expected: PASS — the contract test checks non-interactivity, the envelope, and `--print-spec` for every script sourcing the library.

- [ ] **Step 6: Commit**

```bash
git add scripts/install/hosts-entries.sh scripts/install/hosts_entries_test.go
git commit -m "Issue #3593: write the hosts block only when it is needed"
```

---

### Task 6: Certificate names follow the domain, and a stale pair is re-issued

**Files:**
- Modify: `scripts/lib/localtls.sh`, `scripts/install/mkcert-setup.sh`
- Test: `scripts/install/mkcert_setup_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `MEMQL_LOCAL_TLS_SECRET` is now `memql-front-door-tls` (consumed by Task 7's Ingresses and Task 8's seed-secrets).
  - `localtls.sh` exports `MEMQL_LOCAL_DOMAIN` (default `memql.localhost`) and derives `MEMQL_LOCAL_TLS_HOSTNAMES` as `*.<domain>,<domain>`.
  - `install.mkcert` gains `--domain=<apex>`; it re-issues when an existing pair's SANs do not cover the request.

- [ ] **Step 1: Write the failing test**

Append to `scripts/install/mkcert_setup_test.go`:

```go
// A certificate that does not cover the hostnames is not a certificate this
// install can use. Skipping an existing pair on the strength of the file
// existing is the shape of memql#3384: seed-secrets skipped, traefik served its
// own default cert for both front-door hosts, and `make up` still printed a
// green summary.
func TestMkcertReissuesWhenSANsDoNotCover(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "dev.crt")
	key := filepath.Join(dir, "dev.key")

	// First run: issue for the default domain.
	if out, code := runMkcert(t, "--domain=memql.localhost",
		"--cert="+cert, "--key="+key, "--confirm=install-memql-ca"); code != 0 {
		t.Fatalf("first issue exit %d\n%s", code, out)
	}
	firstMod := modTime(t, cert)

	// Second run for a DIFFERENT domain must not reuse the pair.
	out, code := runMkcert(t, "--domain=lab.example.com",
		"--cert="+cert, "--key="+key, "--confirm=install-memql-ca")
	if code != 0 {
		t.Fatalf("second issue exit %d\n%s", code, out)
	}
	if modTime(t, cert).Equal(firstMod) {
		t.Error("certificate was reused for a domain its SANs do not cover")
	}
	if !strings.Contains(out, `"reissued":true`) {
		t.Errorf("envelope does not report the re-issue\n%s", out)
	}
}
```

`runMkcert` and `modTime` follow the file's existing helpers; if mkcert is not installed in the test environment, mirror how the existing tests in this file skip (`testing.Short()` or a `command -v mkcert` guard) rather than inventing a new mechanism.

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./scripts/install/ -run TestMkcertReissues -v`
Expected: FAIL — either `--domain` is unknown, or the second run reuses the pair.

- [ ] **Step 3: Update localtls.sh**

In `scripts/lib/localtls.sh`, replace the hostnames and secret constants:

```bash
# The domain the local front door is served at. ONE value; the SANs, the hosts
# block, the Ingress hostnames and identity's issuer are all derived from it
# (memql#3593). Operators override per-invocation with --domain, or with
# MEMQL_LOCAL_DOMAIN when a script resolves the flag's default from the
# environment (the capability contract's env-as-default tier).
MEMQL_LOCAL_DOMAIN="${MEMQL_LOCAL_DOMAIN:-memql.localhost}"

# The names the certificate must carry. The wildcard covers cockpit. /
# identity. / anything else the local overlay adds; the apex is listed
# separately because a wildcard label does not match it.
MEMQL_LOCAL_TLS_HOSTNAMES="*.${MEMQL_LOCAL_DOMAIN},${MEMQL_LOCAL_DOMAIN}"

# The k8s Secret both local front-door ingresses name in their spec.tls.
#
# RENAMED from local-znas-tls (memql#3593). The old name embedded one operator's
# domain in a secret every install creates; the name is a fact about the front
# door, not about whose domain it happens to serve.
MEMQL_LOCAL_TLS_SECRET="memql-front-door-tls"
```

- [ ] **Step 4: Add `--domain` and SAN coverage to mkcert-setup.sh**

Add the param spec:

```bash
cap_spec_param "domain" "front-door apex; derives the wildcard + apex SANs (mutually exclusive with --hostnames)"
```

In `main`, mirror Task 5's two-spellings rule:

```bash
    domain="$(cap_param domain "")"
    hostnames_raw="$(cap_param hostnames "")"
    if [[ -n "$domain" && -n "$hostnames_raw" ]]; then
        cap_fail 2 "--domain and --hostnames are two spellings of one answer; pass one"
    fi
    if [[ -n "$domain" ]]; then
        hostnames_raw="*.${domain},${domain}"
    elif [[ -z "$hostnames_raw" ]]; then
        hostnames_raw="$DEFAULT_HOSTNAMES"
    fi
```

Add the coverage check, used wherever the script currently decides to skip an existing pair:

```bash
# cert_covers <cert-path> -- true when the certificate's SANs include every
# name in HOSTNAMES.
#
# WHY EXISTENCE IS NOT ENOUGH. A pair on disk was issued for whatever domain the
# last run used. Reusing it for a different one leaves traefik serving a
# certificate for a name nobody dialed, which surfaces as a TLS failure against
# a hostname the operator typed themselves -- the exact failure memql#3593 was
# opened about.
function cert_covers() {
    local cert="$1" sans host
    [[ -f "$cert" ]] || return 1
    sans="$(openssl x509 -in "$cert" -noout -ext subjectAltName 2>/dev/null || true)"
    [[ -n "$sans" ]] || return 1
    for host in "${HOSTNAMES[@]}"; do
        printf '%s' "$sans" | grep -Fq "DNS:${host}" || return 1
    done
    return 0
}
```

Report the outcome in the envelope:

```bash
    cap_result_set_raw reissued "$reissued"
```

`openssl` is already a prerequisite of this script's neighbourhood; if `check_prerequisites` does not require it, add it there with `cap_fail 4`.

- [ ] **Step 5: Run the test and verify it passes**

Run: `go test ./scripts/install/ -run TestMkcert -v`
Expected: PASS.

- [ ] **Step 6: Run the full script suite**

Run: `go test ./scripts/... -v`
Expected: PASS. `seed_secrets_front_door_tls_test.go` will fail on the secret rename — that is Task 8's change; if it fails now, leave it and note it, or move the constant reference in that test as part of this commit if it is a one-line rename.

- [ ] **Step 7: Commit**

```bash
git add scripts/lib/localtls.sh scripts/install/mkcert-setup.sh scripts/install/mkcert_setup_test.go
git commit -m "Issue #3593: certificate names follow the domain; a stale pair is re-issued"
```

---

### Task 7: Take the domain out of the overlay

**Files:**
- Create: `deploy/k8s/overlays/local/patches/domain-envfrom.yaml`, `deploy/k8s/overlays/local/patches/drop-pinned-issuer.yaml`
- Modify: `deploy/k8s/overlays/local/kustomization.yaml`, `patches/identity-local-config.yaml`, `front-door.yaml`, `cockpit-front-door.yaml`
- Test: covered by Task 9's render test; this task's gate is `kustomize build` succeeding

**Interfaces:**
- Consumes: `MEMQL_DOMAIN` derivation (Task 1), `memql-front-door-tls` secret name (Task 6).
- Produces: a rendered overlay whose only domain literals are the two Ingress hosts (at the committed default `memql.localhost`), and where all nine node Deployments carry `envFrom: configMapRef: memql-domain` and no `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` env entry.

- [ ] **Step 1: Create the envFrom patch**

Create `deploy/k8s/overlays/local/patches/domain-envfrom.yaml`:

```yaml
# JSON 6902 patch: mount the `memql-domain` ConfigMap as env on every engine
# node (memql#3593).
#
# WHAT SEEDS IT: scripts/k3d/seed-secrets.sh, from the --domain the bring-up was
# given. It carries exactly ONE key, MEMQL_DOMAIN, from which the engine derives
# identity's base URL, the issuer every node verifies against, the discovery
# endpoint, the CORS origins and the OAuth redirect URIs
# (component/envregistry/domain.go).
#
# WHY APPEND RATHER THAN A STRATEGIC MERGE: `envFrom` has no patch merge key, so
# a strategic-merge patch REPLACES the whole list -- which would silently drop
# `memql-secrets` (the master key, the genesis envelope, the database DSN) from
# every node it touched. `add` at `envFrom/-` appends.
#
# WHY NOT optional, unlike memql-bootstrap: a node with no domain would derive
# nothing and fall back to the base manifests' staging placeholder issuer. It
# would boot, form a mesh, and reject every token with an error naming neither
# the domain nor this ConfigMap. Refusing to start is the honest failure.
- op: add
  path: /spec/template/spec/containers/0/envFrom/-
  value:
    configMapRef:
      name: memql-domain
```

- [ ] **Step 2: Rewrite the kustomization's env section**

In `deploy/k8s/overlays/local/kustomization.yaml`, delete the eight inline `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` strategic-merge patches (bff, cognition, voice, agent, planner, workbench, mcp, voice-agent) and replace them with one regex-targeted envFrom patch plus one regex-targeted deletion:

```yaml
  # The domain, delivered once (memql#3593).
  #
  # Every node's expected issuer used to be pinned here, eight times, each a
  # separate copy of one fact. They are now DERIVED from the memql-domain
  # ConfigMap by component/envregistry/domain.go, so this overlay states the
  # relationship and never the value.
  #
  # Two patches because they need different mechanisms: envFrom has no merge key
  # (so it must be appended with 6902), and an explicit env entry beats envFrom
  # (so the base's pinned issuer must be removed or the ConfigMap is ignored).
  - path: patches/domain-envfrom.yaml
    target:
      kind: Deployment
      name: "^(identity|bff|cognition|agent|planner|workbench|mcp|voice|voice-agent)$"
  - target:
      kind: Deployment
      name: "^(identity|bff|cognition|agent|planner|workbench|mcp|voice|voice-agent)$"
    patch: |
      - op: remove
        path: /spec/template/spec/containers/0/env/[name=MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER]
```

The `[name=...]` selector is a kustomize extension to JSON 6902 for list elements with a name key. **Verify it renders before going further** (`kubectl kustomize deploy/k8s/overlays/local | grep -c MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER` must print `0`, which grep reports by exiting 1).

If kustomize rejects the selector, use the strategic-merge fallback instead: nine short patches, one per node, each naming its own Deployment and container. Still domain-free. For each of `identity`, `bff`, `cognition`, `agent`, `planner`, `workbench`, `mcp`, `voice`, `voice-agent`, add:

```yaml
  - target: { kind: Deployment, name: identity }
    patch: |
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: identity
        namespace: memql
      spec:
        template:
          spec:
            containers:
              - name: identity
                env:
                  - name: MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER
                    $patch: delete
```

replacing both occurrences of `identity` with the node name. The container name equals the Deployment name for all nine — confirm with `grep -A2 "containers:" deploy/k8s/base/<node>.yaml` before writing them out.

- [ ] **Step 3: Strip the domain from the identity patch**

In `deploy/k8s/overlays/local/patches/identity-local-config.yaml`, delete the `MEMQL_IDENTITY_BASE_URL`, `MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER`, `MEMQL_IDENTITY_BOOTSTRAP_DOMAIN`, `MEMQL_DISCOVERY_GRPC_ENDPOINT`, `MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS` and `MEMQL_IDENTITY_REGISTERED_CLIENTS` entries. Keep `MEMORY_NODES_DATABASE_MIGRATE_ON_START`. Add the two domain-free extras and rewrite the file header:

```yaml
# Strategic merge patch: the local-only identity settings that are NOT derived
# from the domain (memql#3593).
#
# Everything domain-shaped -- base URL, expected issuer, bootstrap domain,
# discovery endpoint, CORS origins, OAuth redirect URIs -- used to be pinned
# here and is now derived from the memql-domain ConfigMap at boot
# (component/envregistry/domain.go). What is left is genuinely local and carries no
# domain: the vite dev-server origins, and running migrations on start because
# local Postgres begins empty.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: identity
  namespace: memql
spec:
  template:
    spec:
      containers:
        - name: identity
          env:
            # The vite dev servers for the portal and a product SPA. Domain-free
            # by construction, which is why they are an "extra" rather than part
            # of what the domain derives -- a staging deployment that forgot to
            # set CORS must not silently admit localhost.
            - name: MEMQL_IDENTITY_CORS_EXTRA_ORIGINS
              value: "http://localhost:8080,http://localhost:3000"
            - name: MEMQL_IDENTITY_EXTRA_REGISTERED_CLIENTS
              value: '[{"clientId":"app","redirectURIs":["http://localhost:8080/auth/callback","http://localhost:3000/auth/callback"]}]'
            # Local Postgres starts empty (Tiger Cloud staging has a
            # pre-existing schema; local does not).
            - name: MEMORY_NODES_DATABASE_MIGRATE_ON_START
              value: "true"
```

- [ ] **Step 4: Move the Ingress hosts to the new default and rename the secret**

In `deploy/k8s/overlays/local/cockpit-front-door.yaml` and `front-door.yaml`, replace every `local.znas.io` with `memql.localhost` and every `local-znas-tls` with `memql-front-door-tls`. Add to each file's header comment:

```
# THE HOSTNAME IS A COMMITTED DEFAULT, NOT A CONSTANT (memql#3593). An install
# that chooses another domain overrides it through the ArgoCD Application's
# spec.source.kustomize.patches, emitted by scripts/k3d/up.sh -- the same seam
# memql#3572 uses for image overrides. The default is committed so that a plain
# `kubectl apply -k` still produces a working front door.
```

- [ ] **Step 5: Render the overlay**

Run:
```bash
kubectl kustomize deploy/k8s/overlays/local > /tmp/rendered.yaml && \
  grep -c "memql-domain" /tmp/rendered.yaml && \
  grep -c "znas.io" /tmp/rendered.yaml
```
Expected: at least 9 for `memql-domain`; `grep -c znas.io` prints `0` (grep exits 1 on no match, which is the pass here). If `kubectl kustomize` is unavailable, `kustomize build` is equivalent. If neither is installed, stop and report — this step cannot be faked.

- [ ] **Step 6: Commit**

```bash
git add deploy/k8s/overlays/local/kustomization.yaml \
        deploy/k8s/overlays/local/patches/domain-envfrom.yaml \
        deploy/k8s/overlays/local/patches/identity-local-config.yaml \
        deploy/k8s/overlays/local/front-door.yaml \
        deploy/k8s/overlays/local/cockpit-front-door.yaml
git commit -m "Issue #3593: take the domain out of the local overlay"
```

---

### Task 8: `k3d.up --domain` seeds the ConfigMap and patches the Application

**Files:**
- Modify: `scripts/k3d/up.sh`, `scripts/k3d/seed-secrets.sh`, `Makefile`
- Test: `scripts/k3d/up_domain_test.go`

**Interfaces:**
- Consumes: `MEMQL_LOCAL_DOMAIN` / `MEMQL_LOCAL_TLS_SECRET` from `localtls.sh` (Task 6); the overlay's committed default (Task 7).
- Produces: `k3d.up` accepts `--domain=<apex>` (default resolved from `MEMQL_LOCAL_DOMAIN`, falling back to `memql.localhost`); it seeds ConfigMap `memql-domain` with the single key `MEMQL_DOMAIN`; it emits `spec.source.kustomize.patches` for the two Ingress hostnames only when the domain differs from the overlay's committed default; it refuses a domain that differs from the one already in the cluster; the result envelope gains `domain`.

- [ ] **Step 1: Write the failing test**

Create `scripts/k3d/up_domain_test.go`:

```go
// Package k3d holds tests for the local cluster bring-up capability scripts.
package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func upScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("up.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The param exists and is documented, which is what the wizard and the runbook
// both read.
func TestUpDeclaresDomainParam(t *testing.T) {
	out, err := exec.Command("bash", upScript(t), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "domain") {
		t.Errorf("--print-spec does not declare --domain\n%s", out)
	}
}

// The Ingress patches are emitted ONLY when the domain differs from the
// overlay's committed default. Emitting them always would mean every install
// carries generated YAML restating what the manifests already say.
func TestKustomizePatchesOnlyWhenOverridden(t *testing.T) {
	body, err := os.ReadFile(upScript(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	if !strings.Contains(src, "OVERLAY_DEFAULT_DOMAIN") {
		t.Error("up.sh does not name the overlay's committed default, so it cannot know when to emit patches")
	}
	if !strings.Contains(src, "kustomize:\\n      patches:") &&
		!strings.Contains(src, "patches:") {
		t.Error("up.sh emits no kustomize.patches block")
	}
}
```

Add a shell-level test for the emitter by extracting it into a function the test can source, if `up.sh` guards its `main` invocation. If it does not, keep the assertions textual as above and cover the emitted YAML shape in Task 9's render test instead — do not add a `main` guard purely for testability without checking the other `scripts/k3d/*_test.go` files for the pattern they already use.

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./scripts/k3d/ -run 'TestUpDeclaresDomain|TestKustomizePatches' -v`
Expected: FAIL — no `--domain` in the spec.

- [ ] **Step 3: Add the param and the refusal**

In `scripts/k3d/up.sh`, source `localtls.sh` if it is not already sourced, add the spec line beside the others:

```bash
cap_spec_param "domain" "front-door apex the cluster is served at (default: memql.localhost)"
```

and in `main`, beside the other `cap_param` calls:

```bash
    # The domain is a VALUE, not the shape of the system, so it arrives as a
    # parameter and lands in a ConfigMap -- exactly where environment-parity.md
    # puts a value. The env tier is the flag's DEFAULT, per the capability
    # contract (cap_param has no env tier of its own).
    DOMAIN="$(cap_param domain "${MEMQL_LOCAL_DOMAIN:-memql.localhost}")"
```

Add the mismatch refusal, called after the cluster exists and before seeding:

```bash
# refuse_domain_change -- a cluster's domain is chosen once.
#
# Changing it invalidates every passkey (the WebAuthn RP id is derived from the
# domain), every live session and node token (the issuer changes), the
# certificate SANs and the hosts block. A partially-migrated cluster fails at
# sign-in with an error naming none of those things, so the honest move is to
# refuse and point at the one command that does it properly.
function refuse_domain_change() {
    local existing
    existing="$(kubectl get configmap memql-domain -n "${NAMESPACE}" \
        -o jsonpath='{.data.MEMQL_DOMAIN}' 2>/dev/null || true)"
    [[ -n "$existing" ]] || return 0
    [[ "$existing" == "$DOMAIN" ]] && return 0
    cap_fail 3 "this cluster is serving ${existing} and --domain says ${DOMAIN}; a domain is chosen once (it is baked into every passkey, session and certificate). Re-run with --domain=${existing}, or rebuild with: make up-refresh DOMAIN=${DOMAIN}"
}
```

- [ ] **Step 4: Seed the ConfigMap**

In `scripts/k3d/seed-secrets.sh`, add a `--domain` param with the same default, and create the ConfigMap idempotently beside the existing secret creation:

```bash
# The domain, as ONE key. Everything domain-shaped is derived from it at boot
# (component/envregistry/domain.go), so this is the whole of what the cluster is
# told about its own hostname.
kubectl create configmap memql-domain -n "${NAMESPACE}" \
    --from-literal=MEMQL_DOMAIN="${DOMAIN}" \
    --dry-run=client -o yaml | kubectl apply -f - >&2
```

Rename the TLS secret creation to use `${MEMQL_LOCAL_TLS_SECRET}` (now `memql-front-door-tls`) rather than a literal, and update `scripts/k3d/seed_secrets_front_door_tls_test.go` to match.

- [ ] **Step 5: Emit the Ingress patches**

In `scripts/k3d/up.sh`, beside `kustomize_image_overrides`, add:

```bash
# The overlay's committed Ingress hostname. Patches are emitted only when the
# operator's domain differs -- the common case then carries no generated YAML at
# all, and `kubectl apply -k` on a clean checkout still produces a working front
# door.
readonly OVERLAY_DEFAULT_DOMAIN="memql.localhost"

# kustomize_host_overrides -- the `kustomize.patches` entries that repoint the
# two front-door Ingresses, or nothing.
#
# WHY THE APPLICATION AND NOT A SECOND OVERLAY. Same reasoning as the image
# overrides above (memql#3572): the two need different hostname VALUES, not a
# different topology, so it is expressed where a value belongs. One overlay, one
# sync path, no `if installing` branch in the manifests.
#
# WHY ONLY THESE TWO. Everything else the domain touches is process config and
# reaches the pods through the memql-domain ConfigMap. An Ingress host is a
# Kubernetes API object, so it has to be in the render.
function kustomize_host_overrides() {
    [[ "$DOMAIN" != "$OVERLAY_DEFAULT_DOMAIN" ]] || return 0
    cat <<EOF

    patches:
      - target:
          kind: Ingress
          name: cockpit-front-door
        patch: |-
          - op: replace
            path: /spec/rules/0/host
            value: cockpit.${DOMAIN}
          - op: replace
            path: /spec/tls/0/hosts/0
            value: cockpit.${DOMAIN}
      - target:
          kind: Ingress
          name: identity-front-door
        patch: |-
          - op: replace
            path: /spec/rules/0/host
            value: identity.${DOMAIN}
          - op: replace
            path: /spec/tls/0/hosts/0
            value: identity.${DOMAIN}
EOF
}
```

`kustomize_image_overrides` already emits a `kustomize:` key when a registry is set. Both blocks must land under **one** `kustomize:` mapping — restructure so the `kustomize:` line is emitted once when either override is non-empty, then the `images:` and `patches:` sub-blocks follow. Render the Application to a temp file and `kubectl apply --dry-run=client -f` it to confirm the YAML is valid before proceeding.

Add to the result envelope, beside the existing `cap_result_set` calls:

```bash
    cap_result_set domain "$DOMAIN"
```

- [ ] **Step 6: Add the Makefile passthrough**

In the `Makefile`, pass `DOMAIN` through to the bring-up targets:

```make
# The front-door domain. One value; the ConfigMap, the certificate, the hosts
# block and the Ingress hostnames all follow from it (memql#3593).
DOMAIN ?= memql.localhost
```

and append `--domain=$(DOMAIN)` to the `up`, `up-refresh` and `secrets` target invocations.

- [ ] **Step 7: Run the tests**

Run: `go test ./scripts/... -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add scripts/k3d/up.sh scripts/k3d/seed-secrets.sh scripts/k3d/up_domain_test.go \
        scripts/k3d/seed_secrets_front_door_tls_test.go Makefile
git commit -m "Issue #3593: k3d.up --domain seeds the ConfigMap and patches the Application"
```

---

### Task 9: Prove the overlay carries no domain and every node is wired

**Files:**
- Create: `deploy/k8s/overlays/local/render_domain_test.go`
- Create: `component/architecture/no_vendor_domain_test.go` (or the nearest existing guard-test location — see Step 3)

**Interfaces:**
- Consumes: the rendered overlay (Task 7), the emitter (Task 8).
- Produces: nothing consumed by later tasks.

**Why this task exists separately.** Task 7's design leans on a name-regex patch target reaching all nine node Deployments. A regex that silently stops matching is invisible: the cluster boots, the mesh forms, and every token is rejected with "invalid issuer". The render test is the difference between assuming the regex covers a tenth node type and knowing it.

- [ ] **Step 1: Write the failing test**

Create `deploy/k8s/overlays/local/render_domain_test.go`:

```go
// Package local holds tests that render this overlay and assert about the
// result.
package local

import (
	"os/exec"
	"strings"
	"testing"
)

// The nine node Deployments the local mesh runs. Listed rather than discovered,
// because the failure this test exists to catch is a TENTH node type arriving
// and the patch's name regex silently not covering it.
var nodes = []string{
	"identity", "bff", "cognition", "agent", "planner",
	"workbench", "mcp", "voice", "voice-agent",
}

func render(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("kubectl", "kustomize", ".").CombinedOutput()
	if err != nil {
		t.Skipf("kubectl kustomize unavailable; cannot render: %v", err)
	}
	return string(out)
}

// After memql#3593 the overlay states the RELATIONSHIP to the domain and never
// the value -- except the two Ingress hosts, which are Kubernetes API objects
// and carry the committed default.
func TestRenderedOverlayCarriesNoVendorDomain(t *testing.T) {
	rendered := render(t)
	if strings.Contains(rendered, "znas.io") {
		t.Error("rendered overlay still names znas.io")
	}
	if strings.Contains(rendered, "local-znas-tls") {
		t.Error("rendered overlay still names the old TLS secret")
	}
}

// Every node reads the ConfigMap, and no node overrides it with a pinned env
// entry. An explicit env entry beats envFrom in Kubernetes, so one survivor
// means that node verifies against the staging placeholder issuer and rejects
// every mesh token.
func TestEveryNodeReadsTheDomainConfigMap(t *testing.T) {
	rendered := render(t)
	docs := strings.Split(rendered, "\n---\n")

	for _, node := range nodes {
		var found bool
		for _, doc := range docs {
			if !strings.Contains(doc, "kind: Deployment") ||
				!strings.Contains(doc, "name: "+node+"\n") {
				continue
			}
			found = true
			if !strings.Contains(doc, "name: memql-domain") {
				t.Errorf("%s does not mount the memql-domain ConfigMap", node)
			}
			if strings.Contains(doc, "MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER") {
				t.Errorf("%s still pins an expected issuer, which beats envFrom", node)
			}
		}
		if !found {
			t.Errorf("no Deployment named %s in the rendered overlay", node)
		}
	}
}

// The committed default is what an un-overridden install serves.
func TestFrontDoorHostsUseTheCommittedDefault(t *testing.T) {
	rendered := render(t)
	for _, host := range []string{"cockpit.memql.localhost", "identity.memql.localhost"} {
		if !strings.Contains(rendered, host) {
			t.Errorf("rendered overlay does not serve %s", host)
		}
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./deploy/k8s/overlays/local/ -v`
Expected: PASS if Task 7 is complete. A skip means `kubectl` is missing — install it or report; a skipped guard proves nothing.

- [ ] **Step 3: Add the repository-wide sweep**

Add the guard to `product_neutrality_test.go` (repo root, package `main`), which is where the existing banned-names sweep lives:

```go
// znas.io is one company's domain. The engine is meant to carry no product, and
// a hostname is product (memql#3593). The local default is memql.localhost;
// anything else is an operator's own choice, arriving as --domain.
//
// SAME CAVEAT AS TestEngineIsProductNeutral, which this sits beside: it is a
// banned-names list. It catches this name, not the next one. It exists because
// this name was in eight files across deploy/, scripts/ and editors/ and
// nothing noticed.
func TestNoVendorDomainLiterals(t *testing.T) {
	roots := []string{"deploy", "scripts", "editors", "component"}
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "dist": true,
		"dist-test": true, "out": true, "bin": true,
	}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				// Unreadable is not a finding; a binary or a broken symlink is
				// not a place a hostname hides in a way that reaches a cluster.
				return nil //nolint:nilerr
			}
			if bytes.Contains(body, []byte("znas.io")) {
				t.Errorf("%s names znas.io -- the domain is a value now, supplied as --domain", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
}
```

Imports needed: `bytes`, `io/fs`, `os`, `path/filepath`, `testing`. If `product_neutrality_test.go` already imports a subset, add only what is missing.

Note the roots deliberately exclude `docs/` — prior specs record the history and rewriting them would be a lie — and this plan and its spec live under `docs/`, so they are outside the sweep without needing an allowlist entry.

- [ ] **Step 4: Run the sweep and fix what it finds**

Run: `go test ./... -run 'TestNoVendorDomain|TestRenderedOverlay|TestEveryNodeReads' -v`
Expected: PASS once Tasks 6-8 and 10 have landed. Files it names that are not yet converted belong to a later task — either complete that task first or note the ordering; do not add them to the allowlist.

- [ ] **Step 5: Commit**

```bash
git add deploy/k8s/overlays/local/render_domain_test.go <path/to/sweep_test.go>
git commit -m "Issue #3593: prove the overlay carries no domain and every node is wired"
```

---

### Task 10: The wizard offers a default and validates, instead of refusing

**Files:**
- Modify: `editors/vscode/src/install/stackPin.ts`, `editors/vscode/src/state/addCluster.ts`, `editors/vscode/src/install/session.ts`
- Test: `editors/vscode/test/installDomain.test.ts`

**Interfaces:**
- Consumes: `k3d.up --domain` (Task 8), `install.hostsEntries --domain` (Task 5), `install.mkcert --domain` (Task 6).
- Produces: `DEFAULT_LOCAL_DOMAIN = "memql.localhost"` exported from `stackPin.ts`, replacing `SUPPORTED_LOCAL_DOMAIN`; `installDomainProblem(domain: string): string | undefined` now validates syntax rather than equality.

- [ ] **Step 1: Write the failing test**

Rewrite the assertions in `editors/vscode/test/installDomain.test.ts` (keep the file's existing cross-artifact agreement checks, retargeted at the new default):

```typescript
import { DEFAULT_LOCAL_DOMAIN, installDomainProblem } from "../src/install/stackPin.js";

test("the default domain is the one the overlay commits", () => {
  assert.equal(DEFAULT_LOCAL_DOMAIN, "memql.localhost");
});

test("a well-formed domain is accepted, whoever owns it", () => {
  for (const domain of ["memql.localhost", "lab.example.com", "local.znas.io", "a.b.c.d.test"]) {
    assert.equal(installDomainProblem(domain), undefined, `${domain} should be accepted`);
  }
});

test("what is not a domain is refused before the run starts", () => {
  const bad: Array<[string, string]> = [
    ["https://memql.localhost", "scheme"],
    ["memql.localhost:443", "port"],
    ["*.memql.localhost", "wildcard"],
    ["localhost", "single label"],
    ["127.0.0.1", "address"],
    ["MEMQL.localhost", "uppercase"],
    ["memql.localhost.", "trailing dot"],
  ];
  for (const [domain, why] of bad) {
    const problem = installDomainProblem(domain);
    assert.ok(problem, `${domain} (${why}) should be refused`);
    assert.ok(problem.length > 20, `the refusal for ${domain} should say why`);
  }
});

test("an empty field is not answered yet, not wrong", () => {
  assert.equal(installDomainProblem(""), undefined);
  assert.equal(installDomainProblem("   "), undefined);
});
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `cd editors/vscode && npm test 2>&1 | tail -30`
Expected: FAIL — `DEFAULT_LOCAL_DOMAIN` is not exported.

- [ ] **Step 3: Replace the constant and the refusal**

In `editors/vscode/src/install/stackPin.ts`, replace `SUPPORTED_LOCAL_DOMAIN` and `installDomainProblem`:

```typescript
/**
 * The domain a local install serves unless the operator says otherwise.
 *
 * NOT A CONSTRAINT ANYMORE -- A DEFAULT (memql#3593). This used to be the one
 * value the installer accepted, because the release's local overlay pinned its
 * Ingress hosts and identity config and nothing the installer passed could
 * change them. The overlay is now parameterised: the domain reaches the cluster
 * as a ConfigMap and, when it differs from the overlay's committed default, as
 * two patches on the ArgoCD Application.
 *
 * `memql.localhost` rather than a company's domain: the engine is meant to
 * carry no product, and a hostname is product. `.localhost` resolves to
 * loopback by RFC 6761, needs no domain ownership and no third party, and its
 * WebAuthn RP id is accepted by Chrome's validator -- measured in
 * scripts/spikes/webauthn-rpid (memql#3405), where bare `localhost` is refused
 * as a public suffix and `memql.localhost` is not.
 *
 * Kept in step with deploy/k8s/overlays/local's committed Ingress hosts. The
 * install-domain test asserts the agreement against the shipped files.
 */
export const DEFAULT_LOCAL_DOMAIN = "memql.localhost";

/**
 * Why this domain will not work, or nothing.
 *
 * SYNTAX ONLY. Whether a domain RESOLVES is not knowable here and is not this
 * function's job: `hostsBlock` probes it and either writes the entry or refuses
 * with the address it actually answered, and `frontDoor` checks it end to end.
 * What this catches is the answer that cannot become a hostname at all -- a
 * pasted URL, a port, a wildcard -- before ten minutes of work proves it.
 *
 * Empty is accepted: an empty field is "not answered yet", which the
 * required-field check reports in its own words.
 */
const DOMAIN_PATTERN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/;

export function installDomainProblem(domain: string): string | undefined {
  const trimmed = domain.trim();
  if (trimmed === "") return undefined;
  if (DOMAIN_PATTERN.test(trimmed)) return undefined;

  if (/^[a-z]+:\/\//i.test(trimmed)) {
    return "Enter a domain, not a URL: memQL adds the scheme and the hostnames itself, so `memql.localhost` rather than `https://memql.localhost`.";
  }
  if (trimmed.includes(":")) {
    return "Enter a domain with no port. The front door is on 443 and memQL puts the cluster there itself.";
  }
  if (trimmed.startsWith("*.")) {
    return "Enter the domain itself, not a wildcard. memQL derives `cockpit.` and `identity.` from it, and the certificate covers the wildcard for you.";
  }
  if (!trimmed.includes(".")) {
    return `Enter a domain with at least two labels, such as ${DEFAULT_LOCAL_DOMAIN}. A single label cannot carry the front-door subdomains memQL needs.`;
  }
  return `That is not a domain memQL can serve. Use lowercase letters, digits and hyphens, with at least two labels -- for example ${DEFAULT_LOCAL_DOMAIN}.`;
}
```

- [ ] **Step 4: Update the default and thread the flag**

In `editors/vscode/src/state/addCluster.ts`, change the import and the default:

```typescript
import { installDomainProblem, DEFAULT_LOCAL_DOMAIN } from "../install/stackPin.js";
```

```typescript
  // The constant, not a copy of it (memql#3590): the form's offer and the
  // overlay's committed default are the same fact, and a literal here is how
  // they drift. It is now a DEFAULT rather than the only accepted value
  // (memql#3593) -- any well-formed domain reaches the cluster.
  domain: DEFAULT_LOCAL_DOMAIN,
```

In `editors/vscode/src/install/session.ts`, add the domain to the `clusterUp` params:

```typescript
        params = present({
          "repo-root": stackDir,
          "image-registry": opts.imageRegistry || DEFAULT_IMAGE_REGISTRY,
          // CONVERTED, not passed through: git tags carry the `v` and image
          // tags do not. See imageTagFor.
          "image-tag": imageTagFor(opts.tag || DEFAULT_STACK_TAG),
          // The FOURTH consumer of the typed domain (memql#3593). The other
          // three place it on the machine -- hosts block, certificate, probe.
          // This one places it in the cluster: k3d.up seeds the memql-domain
          // ConfigMap and, when it differs from the overlay's committed
          // default, patches the two Ingress hostnames on the Application.
          // Without it the cluster serves the default while the machine is set
          // up for something else, which is memql#3593 exactly.
          domain: opts.domain,
        });
```

Update the `frontDoorFor` doc comment: it says "ONE DERIVATION, THREE CONSUMERS" and there are now four.

- [ ] **Step 5: Run the extension tests**

Run: `cd editors/vscode && npm test 2>&1 | tail -30`
Expected: PASS. Other tests referencing `SUPPORTED_LOCAL_DOMAIN` must import the new name; tests asserting a refusal for a non-default domain now assert acceptance.

- [ ] **Step 6: Commit**

```bash
git add editors/vscode/src/install/stackPin.ts editors/vscode/src/state/addCluster.ts \
        editors/vscode/src/install/session.ts editors/vscode/test/installDomain.test.ts \
        editors/vscode/test/addClusterCollect.test.ts editors/vscode/test/installSession.test.ts
git commit -m "Issue #3593: the wizard offers a default and validates, instead of refusing"
```

---

### Task 11: Documentation and the operator-run verification

**Files:**
- Modify: `CLAUDE.md`, `docs/public/operate/reproduce-the-cloud-locally.md`, `docs/public/operate/environment-parity.md`, `docs/public/operate/auth/identity-service.md`, `GLOSSARY.md` (only where they name a hostname or the TLS secret)

**Interfaces:**
- Consumes: everything above.
- Produces: nothing.

- [ ] **Step 1: Find every documentation mention**

Run: `grep -rln "local\.znas\.io\|local-znas-tls" --include=*.md . | grep -v node_modules`

- [ ] **Step 2: Update each file**

Replace hostnames with `cockpit.memql.localhost` / `identity.memql.localhost` / `memql.localhost` and the secret with `memql-front-door-tls`. In `docs/public/operate/reproduce-the-cloud-locally.md`, add a section documenting the domain as a parameter:

```markdown
## Choosing a domain

The cluster is served at `memql.localhost` by default -- an RFC 6761 loopback
name that needs no domain ownership, no DNS provider and no third party. Bring
your own instead with `DOMAIN=`:

    make up DOMAIN=lab.example.com

Either way the installer points the front-door hostnames at 127.0.0.1. If your
own DNS already answers 127.0.0.1 for them, the hosts block is skipped entirely
and no elevation prompt appears.

The domain is chosen ONCE. It is baked into every passkey (the WebAuthn RP id is
derived from it), every session and node token (the issuer is
`https://identity.<domain>`), the certificate's SANs and the hosts block.
`make up DOMAIN=...` refuses a value that differs from the one the cluster is
already serving; rebuild with `make up-refresh DOMAIN=...`.

### If a node will not start

`CreateContainerConfigError` naming `memql-domain` means the ConfigMap is
missing. Re-run `make secrets`. The reference is deliberately not optional: a
node with no domain would fall back to the base manifests' staging placeholder
issuer, boot, form a mesh, and reject every token with an error naming neither
the domain nor the ConfigMap.
```

In `CLAUDE.md`, update the local-cluster paragraph's hostnames and the `local-znas-tls` mention, and record the domain parameter in one sentence.

- [ ] **Step 3: Run the full suite**

Run: `go test ./... 2>&1 | tail -20 && cd editors/vscode && npm test 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md docs/public/operate/reproduce-the-cloud-locally.md \
        docs/public/operate/environment-parity.md \
        docs/public/operate/auth/identity-service.md GLOSSARY.md
git commit -m "Issue #3593: document the domain as a parameter"
```

- [ ] **Step 5: Hand the cluster verification to an operator**

These cannot run on a machine without docker, k3d and kubectl. Do not report them as passing unless they were actually run; report them as pending instead.

```bash
# Default domain, from a clean slate
make up-refresh
make status                                  # unique node ids + one shared JWKS keyset
kubectl -n memql get ingress -o wide         # cockpit./identity.memql.localhost
kubectl -n memql get configmap memql-domain -o jsonpath='{.data.MEMQL_DOMAIN}'
curl -sS https://identity.memql.localhost/.well-known/jwks.json | head
# then sign in at https://cockpit.memql.localhost/portal/

# BYO domain, after a wildcard A record at 127.0.0.1 exists
make up-refresh DOMAIN=lab.example.com
kubectl -n argocd get app memql -o jsonpath='{.spec.source.kustomize.patches}'
kubectl -n memql get ingress -o wide         # cockpit./identity.lab.example.com

# The refusal
make up DOMAIN=other.example.com             # expect exit 3 naming both domains
```

- [ ] **Step 6: Open the pull request**

```bash
git push -u origin feat/custom-local-domain-3593
gh pr create --fill --base main
```

`main` refuses direct pushes; every change goes through a PR with a merge commit (squash is disabled repo-wide). Note in the PR body which operator-run checks in Step 5 were actually executed and which are outstanding.

---

## Self-review notes

**Spec coverage.** §3.1 derivation -> Task 1. §3.2 extras -> Task 3. §4.1 -> Tasks 1-3. §4.2 -> Task 7. §4.3 -> Tasks 4-6, 8. §4.4 -> Task 10. §4.5 -> Task 2. §5.1 -> Task 5. §5.2 -> Task 8. §5.3 -> Task 6. §5.4 -> Tasks 7, 11. §5.5 -> Task 11. §6 -> Tasks 1-10 inline plus Task 9. §7 out-of-scope items appear in no task, which is correct.

**Known soft spots the implementer should expect to resolve:**

1. **Task 7 Step 3's `[name=...]` JSON 6902 selector** is a kustomize extension and may not be accepted by the version in use. The fallback (nine per-node strategic merges with `$patch: delete`) is spelled out and is still domain-free. Render before moving on.
2. **Task 8 Step 5** requires merging two emitters under one `kustomize:` mapping. `kustomize_image_overrides` currently emits the `kustomize:` line itself; that line has to move out of it.
3. **Task 6's mkcert test** needs mkcert on the machine. Follow whatever skip mechanism the existing tests in that file use.

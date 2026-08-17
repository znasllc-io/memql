# Front Door Rules Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the cluster front door five static host rules — `api.`, `identity.`, `mcp.`, `*.`, apex — with the bff's HTTP path set *derived* from the code rather than hand-authored, and with no second entrances.

**Architecture:** All routing stays in one L7 proxy (traefik locally, nginx in the cloud) on port 443, hostname-routed. The only generated artifact is the list of HTTP paths on `api.<domain>`, produced by a `cmd/` tool from `component/server`'s own path declarations and gated by a staleness test — the same generate-and-check shape `make arch-model` / `make arch-model-check` already uses. Everything else is committed YAML that never changes as customers, apps or sites are added.

**Tech Stack:** Kubernetes Ingress (networking.k8s.io/v1), kustomize overlays, k3d + traefik locally, Go (generator + render tests), bash capability scripts.

**Spec:** [`docs/superpowers/specs/2026-08-13-cluster-front-door-design.md`](../specs/2026-08-13-cluster-front-door-design.md)

## Global Constraints

- **No emojis** in documentation, script output, or user-facing text (CLAUDE.md). Use `SUCCESS:` / `ERROR:` / `WARNING:` / `INFO:` and `[ ]` / `[x]`.
- **Stage files by explicit path.** Never `git add -A` or `git add .` — the repo owner runs multiple sessions against this working tree.
- **Every change is a branch + PR.** `main` refuses direct pushes (repository ruleset). Squash merges are disabled; use a merge commit.
- **Pre-release: no backwards-compat shims, no deprecation windows.** When a contract changes, change it and delete what is no longer needed. No `cockpit.` alias, no redirect.
- **Environment parity is non-negotiable.** No env-specific command, no `if env == "local"` branch, no port-forward in a connection path. Ask of every change: is this the *shape* of the system (→ base/component, everywhere) or a *value* (→ overlay, per env)?
- **Documentation is an acceptance criterion per task**, not a trailing task (`docs/CLAUDE.md`: update the affected page in the same commit).
- **No file under `deploy/` may name a domain**, except the Ingress hosts, which carry the committed default `memql.localhost` (memql#3593).
- Go 1.26.1. The repo is a Go **workspace** (`go.work`); the `module-boundaries` CI lane runs each module with `GOWORK=off`, so an import that only works in workspace mode is a lane failure.

---

### Task 1: The front-door render test

The gate that defines "done" for Task 2. It renders the local overlay and asserts the five hosts, the absence of `cockpit.`, and the absence of the second entrances. Written first, and expected to fail.

**Files:**
- Create: `deploy/k8s/overlays/local/render_frontdoor_test.go`

**Interfaces:**
- Consumes: `render(t *testing.T) string` — already defined in `deploy/k8s/overlays/local/render_domain_test.go:28`, same package `local`. Do **not** redefine it.
- Produces: `hostsIn(rendered string) map[string]bool` and `frontDoorHosts []string`, used by Task 4.

- [ ] **Step 1: Write the failing test**

```go
// Package local — see render_domain_test.go for why these are tests and not
// reviews. This file covers the front door itself: which hosts it serves, and
// which entrances no longer exist.
package local

import (
	"sort"
	"strings"
	"testing"
)

// The five hosts the front door serves (design D3). The COUNT is the
// invariant: it must not grow with customers, apps or sites. Listed rather
// than discovered, for the same reason `nodes` is listed in
// render_domain_test.go — discovery would grow the list along with the
// mistake and assert nothing.
var frontDoorHosts = []string{
	"api.memql.localhost",
	"identity.memql.localhost",
	"mcp.memql.localhost",
	"*.memql.localhost",
	"memql.localhost",
}

// hostsIn returns every Ingress rule host in the rendered overlay. Parsed by
// line rather than with a YAML decoder because the rendered stream is many
// documents of many kinds, and the only thing wanted is `host:` under
// spec.rules — which is unambiguous at the text level and needs no schema.
func hostsIn(rendered string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(rendered, "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "- ")
		if !strings.HasPrefix(t, "host: ") {
			continue
		}
		h := strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "host: ")), `"'`)
		if h != "" {
			out[h] = true
		}
	}
	return out
}

func TestFrontDoorServesExactlyTheFiveHosts(t *testing.T) {
	got := hostsIn(render(t))

	for _, want := range frontDoorHosts {
		if !got[want] {
			t.Errorf("front door does not serve %q", want)
		}
	}
	if len(got) != len(frontDoorHosts) {
		var extra []string
		want := map[string]bool{}
		for _, h := range frontDoorHosts {
			want[h] = true
		}
		for h := range got {
			if !want[h] {
				extra = append(extra, h)
			}
		}
		sort.Strings(extra)
		t.Errorf("front door serves %d hosts, want %d; unexpected: %v",
			len(got), len(frontDoorHosts), extra)
	}
}

// D4: the endpoint is named for its role. Pre-release, so there is no alias
// and no redirect — the old name is simply gone.
func TestCockpitHostIsGone(t *testing.T) {
	if strings.Contains(render(t), "cockpit.") {
		t.Error("the rendered overlay still names a cockpit. host; D4 renamed it to api.")
	}
}

// A second entrance is a connection path that exists in one environment and
// not the others, which is what environment-parity.md forbids. identity was
// reachable both through the front door and directly on host port 8085.
func TestNoSecondEntranceToIdentity(t *testing.T) {
	if strings.Contains(render(t), "identity-external") {
		t.Error("identity-external still exists; identity is reachable only through the front door")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./deploy/k8s/overlays/local/ -run 'TestFrontDoor|TestCockpitHostIsGone|TestNoSecondEntrance' -v
```

Expected: `TestFrontDoorServesExactlyTheFiveHosts` FAILs (`does not serve "api.memql.localhost"`), `TestCockpitHostIsGone` FAILs, `TestNoSecondEntranceToIdentity` FAILs.

If it SKIPs with "neither kustomize nor kubectl is installed", install one — this task cannot be verified without a renderer, and a skipped gate is not a gate.

- [ ] **Step 3: Commit the failing test**

```bash
git add deploy/k8s/overlays/local/render_frontdoor_test.go
git commit -m "test: assert the front door's five hosts and absent second entrances

Fails against the current overlay by design -- it states the target of
design D3/D4 so the change in the next commit is verified rather than
reviewed."
```

---

### Task 2: The rename and the deletions

**One task because it is one atomic change.** Every step below must land in a single PR: the domain derivation, the Ingress hosts, the hosts file, the probe defaults, the k3d port table and the VS Code extension all name the same thing, and a partial landing leaves a developer's machine unable to reach its own cluster.

**Files:**
- Modify: `component/envregistry/domain.go:55-74`
- Modify: `component/envregistry/domain_test.go` (add cases)
- Rename: `deploy/k8s/overlays/local/cockpit-front-door.yaml` → `api-front-door.yaml`
- Modify: `deploy/k8s/overlays/local/kustomization.yaml:53-57`
- Delete: `deploy/k8s/overlays/local/identity-external.yaml`
- Modify: `scripts/k3d/up.sh:236-245`
- Modify: `scripts/install/hosts-entries.sh:83,384`
- Modify: `scripts/install/verify-frontdoor.sh` (the `DEFAULT_HOSTS` constant)
- Modify: `editors/vscode/src/extension.ts:1550,1560`
- Test: `deploy/k8s/overlays/local/render_frontdoor_test.go` (from Task 1 — must go green)

**Interfaces:**
- Consumes: `frontDoorHosts` and `hostsIn` from Task 1.
- Produces: `genesis.DomainDerivations(domain)` returns `MEMQL_DISCOVERY_GRPC_ENDPOINT = "api." + d + ":443"` and CORS origins containing `https://api.<d>`. Task 4 and Plan 2 both rely on the `api.` prefix being the derived one.

- [ ] **Step 1: Write the failing derivation test**

Append to `component/envregistry/domain_test.go`:

```go
// D4: the API edge is named for its role. Six consumers dial this endpoint --
// the Cockpit, the VS Code extension, sdk/go, sdk/ts, workers, the portal --
// and a seventh reads it out of a List-Unsubscribe header we send.
func TestDomainDerivationsUseApiHost(t *testing.T) {
	got := DomainDerivations("memql.localhost")

	if want := "api.memql.localhost:443"; got["MEMQL_DISCOVERY_GRPC_ENDPOINT"] != want {
		t.Errorf("MEMQL_DISCOVERY_GRPC_ENDPOINT = %q, want %q",
			got["MEMQL_DISCOVERY_GRPC_ENDPOINT"], want)
	}
	for name, v := range got {
		if strings.Contains(v, "cockpit.") {
			t.Errorf("%s still derives a cockpit. host: %q", name, v)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./component/genesis/ -run TestDomainDerivationsUseApiHost -v
```

Expected: FAIL — `MEMQL_DISCOVERY_GRPC_ENDPOINT = "cockpit.memql.localhost:443", want "api.memql.localhost:443"`.

- [ ] **Step 3: Change the derivation**

In `component/envregistry/domain.go`, replace the `cockpit` variable and its uses:

```go
	identity := "https://identity." + d
	api := "https://api." + d
	app := "https://app." + d

	// The cockpit CLIENT is loopback BY DESIGN (RFC 8252 native-client
	// redirect), so it carries no domain and is spelled out unchanged. Note
	// that the client is still called "cockpit" — what was renamed is the
	// HOST it dials, not the OAuth client id.
	clients := fmt.Sprintf(
		`[{"clientId":"app","redirectURIs":["%s/auth/callback"]},`+
			`{"clientId":"cockpit","redirectURIs":["http://127.0.0.1/cockpit/callback","http://localhost/cockpit/callback"]},`+
			`{"clientId":"portal","redirectURIs":["%s/auth/callback"]}]`,
		app, "https://portal."+d)

	return map[string]string{
		"MEMQL_IDENTITY_BASE_URL":                 identity,
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": identity,
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":         d,
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "api." + d + ":443",
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":     api + "," + app + ",https://portal." + d,
		"MEMQL_IDENTITY_REGISTERED_CLIENTS":       clients,
	}
```

The portal's redirect URI moves from `https://cockpit.<d>/portal/auth/callback` to `https://portal.<d>/auth/callback` because Plan 2 gives the portal its own origin. Doing it here rather than in Plan 2 keeps every domain-derived value changing once.

- [ ] **Step 4: Run the derivation tests**

```bash
go test ./component/genesis/ -v
```

Expected: PASS, including the pre-existing derivation tests. If an existing test asserts the old `cockpit.` values, update it — it is asserting the behaviour this task changes, and its assertion is the change.

- [ ] **Step 5: Rewrite the Ingress**

```bash
git mv deploy/k8s/overlays/local/cockpit-front-door.yaml deploy/k8s/overlays/local/api-front-door.yaml
```

Replace the whole file body (keep the file's explanatory header style, updated):

```yaml
# The API front door. One host, two backends -- the constraint that shapes
# this file is that an ingress controller's backend protocol is a per-SERVICE
# setting, which is why `bff` and `bff-http` exist as two Services over one
# Deployment (see deploy/k8s/components/engine-bff/bff.yaml).
#
# THE HTTP PATH LIST IS GENERATED, NOT AUTHORED. `make frontdoor-paths` emits
# it from server.PublicPaths() + HandlerAuthorizedPaths() +
# SelfAuthenticatedPaths(); `make frontdoor-paths-check` fails CI when the two
# disagree. Hand-editing the block below will be reverted by the generator.
# The reason is memql's own history: /inbound/{source} and /unsubscribe are
# documented public HTTP exceptions that third parties dial, and NO overlay in
# this repo routed either one -- a missing rule does not 404, it hands an
# HTTP/1.1 request to an h2c backend.
#
# THE HOSTNAME IS A COMMITTED DEFAULT, NOT A CONSTANT (memql#3593). An install
# choosing another domain overrides it through the ArgoCD Application's
# spec.source.kustomize.patches, emitted by scripts/k3d/up.sh.
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: api-front-door
  namespace: memql
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: websecure
spec:
  tls:
    - hosts: ["api.memql.localhost"]
      secretName: memql-front-door-tls
  rules:
    - host: api.memql.localhost
      http:
        paths:
          # BEGIN generated bff HTTP paths -- make frontdoor-paths
          # END generated bff HTTP paths
          - path: /
            pathType: Prefix
            backend:
              service:
                name: bff
                port:
                  number: 50051
```

Leave the generated block empty for now; Task 3 fills it. Traefik orders rules by specificity, so the longer generated paths win over `/`.

- [ ] **Step 6: Delete the second entrances**

```bash
git rm deploy/k8s/overlays/local/identity-external.yaml
```

In `deploy/k8s/overlays/local/kustomization.yaml`, change the `resources:` list:

```yaml
resources:
  - front-door.yaml
  - api-front-door.yaml
  - ../../base
  # Local-only infrastructure (absent in base/staging/prod)
  - postgres.yaml
  - azurite.yaml
```

In `scripts/k3d/up.sh`, replace the port table and its comment:

```bash
    # Port-forward table. The FRONT DOOR is the connection path: traefik
    # terminates TLS on 443 and routes by hostname, exactly as the cloud
    # ingress does. 80 is kept for redirects.
    #
    # 5432 is DEBUG ONLY and is not a connection path -- see
    # docs/public/operate/environment-parity.md. The identity (8085) and
    # livekit (7880) mappings were deleted: 8085 was a second entrance to a
    # service the front door already serves, and 7880 pointed at a Deployment
    # this overlay removes (local voice uses LiveKit Cloud).
    local port_args=(
        --port "443:443@loadbalancer"
        --port "80:80@loadbalancer"
        --port "5432:5432@loadbalancer"
    )
```

- [ ] **Step 7: Run the Task 1 gate**

```bash
go test ./deploy/k8s/overlays/local/ -run 'TestFrontDoor|TestCockpitHostIsGone|TestNoSecondEntrance' -v
```

Expected: `TestCockpitHostIsGone` PASS, `TestNoSecondEntranceToIdentity` PASS, `TestFrontDoorServesExactlyTheFiveHosts` still FAILs on `mcp.memql.localhost`, `*.memql.localhost` and `memql.localhost` — those arrive in Task 4 and Plan 2. That is the expected intermediate state.

- [ ] **Step 8: Update the scripts that carry the hostname set**

`scripts/install/hosts-entries.sh` — the default hostnames and the derivation helper:

```bash
readonly DEFAULT_HOSTNAMES="api.${DEFAULT_DOMAIN},identity.${DEFAULT_DOMAIN},portal.${DEFAULT_DOMAIN},${DEFAULT_DOMAIN}"
```

```bash
# hostnames_for_domain <apex> -- the names a front door puts on a domain, apex
# last to match the block this script documents. The wildcard *.<apex> cannot
# go in a hosts file (no wildcard semantics), so each name is listed; sites
# added later need their own entry, which the site-hosting runbook covers.
function hostnames_for_domain() {
    local apex="$1"
    printf 'api.%s,identity.%s,portal.%s,%s' "$apex" "$apex" "$apex" "$apex"
}
```

`scripts/install/verify-frontdoor.sh`:

```bash
DEFAULT_HOSTS="api.${DEFAULT_DOMAIN},identity.${DEFAULT_DOMAIN}"
```

- [ ] **Step 9: Run the script tests**

```bash
go test ./scripts/... -run 'Hosts|FrontDoor|Capability' -v
```

Expected: PASS. `hosts_entries_test.go` and `verify_frontdoor_test.go` assert the defaults; update their expectations to the new set — they are asserting the behaviour this task changes.

- [ ] **Step 10: Update the VS Code extension**

`editors/vscode/src/extension.ts` around line 1550:

```ts
    prompt: 'Domain (e.g. memql.localhost). The endpoint is composed as api.<domain>:443.',
```

and wherever `composeEndpointFromDomain` builds the endpoint, change the prefix from `cockpit.` to `api.`. Grep first — the file comment at 1560 says the composition lives in one place deliberately:

```bash
grep -rn "cockpit\." editors/vscode/src/ | grep -v test
```

- [ ] **Step 11: Run the extension tests**

```bash
make vscode-test
```

Expected: PASS. `clustersRegistry.test.ts` carries `cockpit.memql.localhost:443` fixtures — update them.

- [ ] **Step 12: Update the documentation this task changes**

- `docs/public/operate/environment-parity.md` — the connection-model code block and the "The connection model in practice" narrative both name `cockpit.<domain>`.
- `docs/public/operate/reproduce-the-cloud-locally.md` — hostnames and the port table.
- `docs/public/operate/install-prerequisites.md` — the hosts-block row.
- `docs/public/overview/quickstart.md` — any front-door URL.
- `docs/public/operate/campaign-sending.md` — the `MEMQL_CAMPAIGNS_UNSUBSCRIBE_BASE_URL` example and the `List-Unsubscribe` sample both use `cockpit.example.com`.
- `CLAUDE.md` — the local-cluster paragraph names `cockpit.memql.localhost` several times.

Verify none survive:

```bash
grep -rn "cockpit\." docs/ CLAUDE.md deploy/ scripts/ editors/ | grep -v "memql-cockpit\|cockpit worker\|clientId.*cockpit"
```

Expected: no output. The exclusions are the *product* (the Cockpit binary), its worker subcommand, and the OAuth client id — none of which are renamed.

- [ ] **Step 13: Verify against a real cluster**

```bash
make up-refresh
kubectl -n memql get ingress -o wide
scripts/install/verify-frontdoor.sh
curl -sS https://api.memql.localhost/healthz
```

Expected: two Ingresses, all probes pass, `/healthz` answers. This cannot be run on a machine without docker/k3d/kubectl — if that is the case, say so explicitly rather than marking the step done.

- [ ] **Step 14: Commit**

```bash
git add component/envregistry/domain.go component/envregistry/domain_test.go \
        deploy/k8s/overlays/local/api-front-door.yaml \
        deploy/k8s/overlays/local/kustomization.yaml \
        scripts/k3d/up.sh scripts/install/hosts-entries.sh \
        scripts/install/verify-frontdoor.sh \
        editors/vscode/src/extension.ts \
        docs/ CLAUDE.md
git commit -m "feat: rename the API edge to api.<domain> and delete the second entrances

The endpoint is named for its role rather than for the first client that
connected to it. Six consumers dial it -- the Cockpit, the VS Code
extension, sdk/go, sdk/ts, workers, the portal -- and Gmail reads it out of
the List-Unsubscribe header we send.

Deletes identity-external (a second entrance to a service the front door
already serves, existing in no other environment) and the 7880 mapping (it
pointed at a Deployment the local overlay removes).

Atomic by necessity: the derivation, the Ingress, the hosts block, the
probe defaults, the k3d port table and the extension all name one thing."
```

---

### Task 3: Derive the bff's HTTP path set

Closes the defect that let `/inbound/{source}` and `/unsubscribe` go unrouted. The path list becomes generated output with a staleness gate, so a new HTTP path either reaches the front door or breaks the build.

**Files:**
- Create: `cmd/frontdoorpaths/main.go`
- Create: `cmd/frontdoorpaths/main_test.go`
- Modify: `deploy/k8s/overlays/local/api-front-door.yaml` (the generated block)
- Modify: `Makefile` (two targets, near `arch-model` at line 408)
- Create: `deploy/k8s/overlays/local/frontdoor_paths_staleness_test.go`

**Interfaces:**
- Consumes: `server.PublicPaths()`, `server.HandlerAuthorizedPaths()`, `server.SelfAuthenticatedPaths()` — all in `github.com/znasllc-io/memql/component/server`, which the root module already requires with a `replace` directive (`go.mod:182,303`).
- Produces: `make frontdoor-paths` (regenerate) and `make frontdoor-paths-check` (gate). Mirrors `arch-model` / `arch-model-check`.

- [ ] **Step 1: Write the failing generator test**

```go
// cmd/frontdoorpaths/main_test.go
package main

import (
	"strings"
	"testing"
)

func TestRenderProducesIngressPathEntries(t *testing.T) {
	got := render([]string{"/healthz", "/inbound/", "/unsubscribe"})

	for _, want := range []string{
		"- path: /healthz",
		"- path: /inbound/",
		"- path: /unsubscribe",
		"pathType: Prefix",
		"name: bff-http",
		"number: 8085",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered block missing %q\n---\n%s", want, got)
		}
	}
}

// The generated block is spliced into a YAML list at a fixed indentation.
// Getting this wrong produces a manifest kustomize rejects, so it is asserted
// rather than eyeballed.
func TestRenderIndentsForTheIngressPathList(t *testing.T) {
	for _, line := range strings.Split(strings.TrimRight(render([]string{"/healthz"}), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "          ") {
			t.Errorf("line is not indented to the path-list level: %q", line)
		}
	}
}

// The paths the front door must carry no matter what else changes. These are
// the documented public HTTP exceptions third parties dial -- the ones that
// were routed by no overlay at all before this generator existed.
func TestCollectIncludesTheThirdPartyPaths(t *testing.T) {
	got := map[string]bool{}
	for _, p := range collect() {
		got[p] = true
	}
	for _, want := range []string{"/healthz", "/inbound/", "/unsubscribe", "/memql/ws"} {
		if !got[want] {
			t.Errorf("collect() omits %q; a third party dials it and nothing else routes it", want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./cmd/frontdoorpaths/ -v
```

Expected: FAIL to build — `undefined: render`, `undefined: collect`.

- [ ] **Step 3: Write the generator**

```go
// Command frontdoorpaths emits the Ingress path entries for the bff's HTTP
// edge on api.<domain>.
//
// WHY GENERATED. An ingress controller's backend protocol is a per-SERVICE
// setting, so the bff's gRPC edge (:50051, h2c) and its HTTP edge (:8085)
// must be reached through two Services -- and every HTTP path therefore needs
// its own Ingress rule. Hand-maintaining that list failed exactly the way
// hand-maintained lists fail: /inbound/{source} and GET+POST /unsubscribe are
// documented public HTTP exceptions that third parties dial, and no overlay in
// the repository routed either one. The failure is not a 404 -- it is an
// HTTP/1.1 request handed to an h2c backend, which fails naming nothing.
//
// The path declarations already exist and are already verified against real
// registration by TestContractRoutesMatchesRegistration. This tool makes the
// front door read from them instead of from somebody's memory.
//
// Usage:
//
//	go run ./cmd/frontdoorpaths                # print the block
//	go run ./cmd/frontdoorpaths --write <file> # splice it into the markers
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/server"
)

const (
	beginMarker = "          # BEGIN generated bff HTTP paths -- make frontdoor-paths"
	endMarker   = "          # END generated bff HTTP paths"
)

// grpcSurface lists paths served by the gRPC edge rather than the HTTP one.
// "/" is the catch-all rule the Ingress carries after the generated block;
// /memql/query is served from middleware ahead of the mux and rides the gRPC
// gateway, so it belongs to the h2c backend.
var grpcSurface = map[string]bool{
	"/":            true,
	"/memql/query": true,
}

// collect returns the deduplicated, sorted HTTP paths the front door must
// route to bff-http.
func collect() []string {
	seen := map[string]bool{}
	for _, set := range [][]string{
		server.PublicPaths(),
		server.HandlerAuthorizedPaths(),
		server.SelfAuthenticatedPaths(),
	} {
		for _, p := range set {
			p = strings.TrimSpace(p)
			if p == "" || grpcSurface[p] {
				continue
			}
			seen[p] = true
		}
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	// Longest first, then lexical. Traefik orders by specificity and nginx by
	// declaration; emitting longest-first makes both agree without relying on
	// either one's tie-breaking.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// render turns a path list into Ingress path entries, indented to sit inside
// spec.rules[0].http.paths.
func render(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "          - path: %s\n", p)
		b.WriteString("            pathType: Prefix\n")
		b.WriteString("            backend:\n")
		b.WriteString("              service:\n")
		b.WriteString("                name: bff-http\n")
		b.WriteString("                port:\n")
		b.WriteString("                  number: 8085\n")
	}
	return b.String()
}

// splice replaces the content between the markers. Returns an error rather
// than appending when a marker is missing: silently writing the block
// somewhere plausible is how a generator produces a manifest nobody reviewed.
func splice(doc, block string) (string, error) {
	begin := strings.Index(doc, beginMarker)
	end := strings.Index(doc, endMarker)
	if begin < 0 || end < 0 || end < begin {
		return "", fmt.Errorf("markers not found (or out of order) -- expected %q then %q", beginMarker, endMarker)
	}
	head := doc[:begin+len(beginMarker)+1]
	return head + block + doc[end:], nil
}

func main() {
	write := flag.String("write", "", "splice the block into this file between its markers")
	flag.Parse()

	block := render(collect())
	if *write == "" {
		fmt.Print(block)
		return
	}

	raw, err := os.ReadFile(*write)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	out, err := splice(string(raw), block)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s: %v\n", *write, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*write, []byte(out), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run the generator tests**

```bash
go test ./cmd/frontdoorpaths/ -v
```

Expected: PASS, all three.

If `TestCollectIncludesTheThirdPartyPaths` fails on `/unsubscribe`, that means it is not in `SelfAuthenticatedPaths()` under the default configuration — read `component/server/unauthenticated_surface.go` and follow what it actually declares rather than adding the path to the generator by hand. The generator must never carry a path the declarations do not.

- [ ] **Step 5: Generate the block and wire the Makefile**

```bash
go run ./cmd/frontdoorpaths --write deploy/k8s/overlays/local/api-front-door.yaml
```

Add to `Makefile` beside `arch-model` (and to the `.PHONY` line at 540):

```make
## Regenerate the bff HTTP path entries in the api front door from
## component/server's own path declarations.
frontdoor-paths:
	$(GO) run ./cmd/frontdoorpaths --write deploy/k8s/overlays/local/api-front-door.yaml

## CI gate: fail when the front door's generated path block is stale. The
## drift this catches is a new public HTTP path that nothing routes -- which
## does not 404, it hands HTTP/1.1 to an h2c backend. Pair with
## `make frontdoor-paths` locally to fix.
frontdoor-paths-check:
	$(GO) test -count=1 -run TestFrontDoorPathsAreNotStale ./deploy/k8s/overlays/local/
```

- [ ] **Step 6: Write the staleness gate**

```go
// deploy/k8s/overlays/local/frontdoor_paths_staleness_test.go
package local

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The generated block in api-front-door.yaml must equal what the generator
// produces right now. Mirrors TestArchitectureModelIsNotStale: the artifact is
// checked in so a plain `kubectl apply -k` works, and this asserts the
// checked-in copy is current.
func TestFrontDoorPathsAreNotStale(t *testing.T) {
	out, err := exec.Command("go", "run", "../../../../cmd/frontdoorpaths").CombinedOutput()
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile("api-front-door.yaml")
	if err != nil {
		t.Fatalf("reading the front door: %v", err)
	}
	doc := string(raw)

	const begin = "# BEGIN generated bff HTTP paths"
	const end = "# END generated bff HTTP paths"
	b, e := strings.Index(doc, begin), strings.Index(doc, end)
	if b < 0 || e < 0 {
		t.Fatal("api-front-door.yaml has lost its generated-block markers")
	}
	got := doc[strings.Index(doc[b:], "\n")+b+1 : e]

	if strings.TrimRight(got, " \n") != strings.TrimRight(string(out), " \n") {
		t.Errorf("the generated path block is stale -- run `make frontdoor-paths`.\n"+
			"checked in:\n%s\ngenerator:\n%s", got, out)
	}
}
```

- [ ] **Step 7: Run the gate**

```bash
make frontdoor-paths-check
```

Expected: PASS. Then prove it actually gates — delete one line from the generated block, re-run, confirm it FAILs, and restore it.

- [ ] **Step 8: Document the mechanism**

Add a row to the endpoint-protocol table in `CLAUDE.md` noting that the bff's HTTP exceptions are routed by a generated Ingress block, and add the two targets to the "Common Tasks" table.

- [ ] **Step 9: Commit**

```bash
git add cmd/frontdoorpaths/ deploy/k8s/overlays/local/api-front-door.yaml \
        deploy/k8s/overlays/local/frontdoor_paths_staleness_test.go \
        Makefile CLAUDE.md
git commit -m "feat: derive the bff's HTTP path set instead of authoring it

/inbound/{source} and GET+POST /unsubscribe are documented public HTTP
exceptions that third parties dial -- a Shopify webhook, a mail client
executing RFC 8058 -- and no overlay in this repository routed either one.
A missing rule does not 404: it hands an HTTP/1.1 request to an h2c backend
and fails with a protocol error naming nothing.

The declarations already existed and were already verified against real
registration by TestContractRoutesMatchesRegistration. This makes the front
door read from them. make frontdoor-paths regenerates; frontdoor-paths-check
gates, mirroring arch-model / arch-model-check."
```

---

### Task 4: The MCP host rule

**Files:**
- Create: `deploy/k8s/overlays/local/mcp-front-door.yaml`
- Modify: `deploy/k8s/overlays/local/kustomization.yaml` (resources list)
- Test: `deploy/k8s/overlays/local/render_frontdoor_test.go` (Task 1's gate)

**Interfaces:**
- Consumes: `frontDoorHosts` from Task 1 — `mcp.memql.localhost` is already listed there.
- Produces: nothing later tasks consume.

- [ ] **Step 1: Confirm the gate still fails for this host**

```bash
go test ./deploy/k8s/overlays/local/ -run TestFrontDoorServesExactlyTheFiveHosts -v
```

Expected: FAIL naming `mcp.memql.localhost` among the missing hosts.

- [ ] **Step 2: Write the Ingress**

```yaml
# The MCP protocol head. Its own host rather than a path under api.<domain>
# for the reason that shapes this whole directory: an ingress controller's
# backend protocol is a per-SERVICE setting, and the mcp node is a different
# Service on a different port (8090) speaking plain HTTP, not h2c.
#
# Not proxied through the edge either -- MCP clients configure a URL, they are
# not browsers, and an extra hop on a tool-calling path buys nothing.
#
# FIRST REAL EXERCISE OF :8090. deploy/k8s/base/mcp.yaml has carried the port
# and a comment saying a public ingress targets it, but no overlay in this
# repository defined one. Protocol details (streamable HTTP vs SSE, timeouts)
# may surface here that nothing has tested.
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: mcp-front-door
  namespace: memql
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: websecure
spec:
  tls:
    - hosts: ["mcp.memql.localhost"]
      secretName: memql-front-door-tls
  rules:
    - host: mcp.memql.localhost
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: mcp
                port:
                  number: 8090
```

Add `- mcp-front-door.yaml` to the `resources:` list in `kustomization.yaml`.

- [ ] **Step 3: Run the gate**

```bash
go test ./deploy/k8s/overlays/local/ -run TestFrontDoorServesExactlyTheFiveHosts -v
```

Expected: still FAIL, now naming only `*.memql.localhost` and `memql.localhost` — those arrive in Plan 2, Task 9. Record that in the commit so a reader does not think the gate is broken.

- [ ] **Step 4: Add the hostname to the hosts block**

In `scripts/install/hosts-entries.sh`, add `mcp.` to both `DEFAULT_HOSTNAMES` and `hostnames_for_domain`.

- [ ] **Step 5: Verify against a cluster**

```bash
make dev NODE=mcp
curl -sS -o /dev/null -w '%{http_code}\n' https://mcp.memql.localhost/
```

Expected: any HTTP status from the mcp node — a 404 or 405 proves the route works. A TLS error or a connection refusal does not.

- [ ] **Step 6: Commit**

```bash
git add deploy/k8s/overlays/local/mcp-front-door.yaml \
        deploy/k8s/overlays/local/kustomization.yaml \
        scripts/install/hosts-entries.sh
git commit -m "feat: give the MCP protocol head its own front-door host

Its own host rather than a path under api.<domain>: backend protocol is a
per-Service setting and mcp is a different Service on 8090 speaking plain
HTTP. Not proxied through the edge either -- MCP clients configure a URL,
are not browsers, and gain nothing from an extra hop.

First real exercise of :8090. base/mcp.yaml has carried the port and a
comment claiming a public ingress targets it; no overlay defined one.

TestFrontDoorServesExactlyTheFiveHosts still fails on the two edge hosts
(*.<domain>, apex) by design -- those land in the edge plan."
```

---

### Task 5: The front-door reference page

**Files:**
- Create: `docs/public/operate/front-door.md`
- Modify: `GLOSSARY.md`

- [ ] **Step 1: Write the page**

Front-matter per `docs/DOCS_STANDARD.md` (`title` / `audience: public` / `status: stable` / `area: operate` / `sinceVersion` / `owner: znas`). Content:

- The five hosts, what is behind each, and which protocol each speaks.
- Why the count is five and must not grow: sites are data (see the site-hosting page from Plan 2), so a new site adds a row, not a rule.
- The one generated rule, why it is generated, and the two make targets.
- The media plane, and that it is permanently separate because WebRTC media is UDP.
- The per-Service backend-protocol constraint, stated once, since it explains `bff` vs `bff-http`, the MCP host, and the generated block all at once.
- Local versus cloud: what varies (ingress controller, cert source, DNS source) and what does not (the hosts, the paths, the dial path), linking `environment-parity.md`.

- [ ] **Step 2: Add the index entry**

One line in `GLOSSARY.md` under the operate section.

- [ ] **Step 3: Verify the doc gates**

```bash
go test ./docs/... 2>/dev/null || true
grep -rn "cockpit\." docs/public/operate/front-door.md
```

Expected: no `cockpit.` references, and any docs front-matter test passes.

- [ ] **Step 4: Commit**

```bash
git add docs/public/operate/front-door.md GLOSSARY.md
git commit -m "docs: add the front-door reference

Five hosts, what is behind each, why the count must not grow, and the one
generated rule."
```

---

## Merge order

Tasks 1 and 2 must merge as **one PR** — the rename is atomic (spec §8). Tasks 3, 4 and 5 may each be their own PR afterwards.

Plan 2 (`2026-08-13-edge-and-site-hosting.md`) depends on Task 2 having landed: it consumes `api.` as the derived host and adds the two remaining front-door hosts.

# Portal -> VS Code Handoff Implementation Plan (PR 2 of 3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A person reading a concept in the portal clicks "Open definition in VS Code" and lands on that construct in the extension -- the file in their workspace, the local checkout, a read-only document served from the cluster, or the detail page -- with the extension never adding a cluster, signing in silently, running anything, or writing settings from a link.

**Architecture:** The portal composes a `vscode://znasllc.memql/open` deep link from the cluster's domain (served in `runtime-config.json`) and the concept's canonical id. The extension registers a `UriHandler` whose pure resolver (no `vscode` import) parses, matches a registered cluster, resolves the construct from the catalog, and decides the landing; a thin adapter in `extension.ts` performs it. A remote construct with no local file opens as a read-only `memql-cluster:` document served by a `TextDocumentContentProvider` over a new sdk/ts pack-browser client.

**Tech Stack:** TypeScript (sdk/ts on `node:test`; editors/vscode on `node --test` + the xvfb host lane; clients/portal on vitest + testing-library), Go (`component/edge`).

**Spec:** `docs/superpowers/specs/2026-08-21-portal-vscode-handoff-and-locality-design.md` (sections 4 and 2.8-2.9). Epic memql#4242; this plan closes #4247, #4249, #4248, #4250, #4251, #4252 in ONE pull request (branch `feat/handoff-portal-vscode`).

## Global Constraints

- **No emojis** anywhere in code, docs, copy, or script output (CLAUDE.md). Use `[ ]` / `[x]` and `SUCCESS:` / `ERROR:` / `WARNING:` / `INFO:`.
- **Stage files by explicit path.** Never `git add -A` or `git add .`.
- **One PR closes all six issues.** Commit per task; the PR body carries `Closes #4247`, `Closes #4249`, `Closes #4248`, `Closes #4250`, `Closes #4251`, `Closes #4252`.
- **Pre-release: no backwards-compat shims.** `runtime-config.json` is additive-only by contract (a field is added, never a required one removed) -- that is the one place "additive" is itself the rule.
- **The link contract is exactly** `vscode://znasllc.memql/open?v=1&cluster=<domain>&kind=<kind>&name=<registry key>` -- `v` is `1`, the four keys are `v`, `cluster`, `kind`, `name`; `name` for a concept is its canonical id; values are `encodeURIComponent`-encoded; the Insiders variant swaps only the scheme to `vscode-insiders`.
- **A link may select a registered cluster, connect it through the existing flows, and open a document. A link may never add a cluster, sign in silently, run anything, or write settings beyond the existing read-only marking.** `originPath` comes from the cluster's catalog, never from the link.
- **No source rendering in the portal**, not even read-only.
- **`clients/portal/src/pages/ConceptPage.tsx`, `src/components/**`, `src/concepts/**`, `src/viewkit/**` may not contain a concept-id-shaped literal (`v<digits>:<word>:<word>`) anywhere, including comments** (`portal_render_path_test.go` at the repo root). Reference the id only through the variable `concept.id`; write examples as `<version>:<domain>:<entity>`.
- **Portal icons come only from `clients/portal/src/ui/icons.ts`**; pages and components never import `lucide-react` directly.
- **Extension modules under `src/constructs/`, `src/handoff/`, `src/state/` stay free of `vscode` imports** (`cmd/memql-lsp/vscodeimportrule_test.go`); `vscode` is used only in adapters (`extension.ts`, `src/webview/*`, `src/views/*`, and the new `src/constructs/clusterDocuments.ts` adapter half named below).
- **Information policy (memql#4194):** panels, toasts and tooltips carry a short classified verdict; raw material goes to the `MemQL Connection` output channel through the existing redactor. A cluster name and a construct key may be logged; a token or a raw error body may not.
- **The LSP client's `documentSelector` is restricted to `{ language: 'memql', scheme: 'file' }`** after Task 4; cluster documents receive no LSP diagnostics.
- Test commands: `cd sdk/ts && npm test`; `cd editors/vscode && npm test` (unit), `make vscode-test-host` (needs `DISPLAY` or xvfb); `make portal-typecheck && make portal-test && make portal-build`; `go test ./component/edge/... && go test .` (repo root guards).

---

### Task 1: sdk/ts pack-browser client (`PackClient`) -- closes #4247

**Files:**
- Create: `sdk/ts/src/pack/pack.ts`
- Create: `sdk/ts/src/pack/index.ts`
- Create: `sdk/ts/test/pack.test.ts`
- Modify: `sdk/ts/src/client/wire.ts` (request payloads near `ListConstructsPayload` ~line 426; result payloads near `ListConstructsResultPayload` ~line 1199; the `ClientPayload` union ~line 520; the `ServerPayload` union ~line 1391; `readServerPayload` ~line 1443 -- both its return-type union and its `if` chain)
- Modify: `sdk/ts/src/index.ts` (add `export * as pack from "./pack/index.js";`)
- Modify: `sdk/ts/package.json` (`exports["./pack"]`, mirroring the `"./constructs"` entry exactly)

**Interfaces:**
- Consumes: `Dispatcher.sendAndWait`, `newShortId`, `readServerPayload` (existing).
- Produces (Task 4 consumes these names verbatim):

```ts
export interface PackDomain { name: string; origin: string; fileCount: number; }
export interface PackFile { path: string; size: number; }
export interface PackFileSource { domain: string; path: string; source: string; origin: string; found: boolean; }
export interface PackCallOptions { signal?: AbortSignal; }
export class PackClient {
  constructor(dispatcher: Dispatcher);
  listDomains(opts?: PackCallOptions): Promise<PackDomain[]>;
  listFiles(domain: string, opts?: PackCallOptions): Promise<PackFile[]>;
  readFile(domain: string, path: string, opts?: PackCallOptions): Promise<PackFileSource>;
}
```

The wire keys are protojson lowerCamelCase of the proto oneof fields: requests `listPackDomains` / `listPackFiles` / `readPackFile`; replies `listPackDomainsResult` / `listPackFilesResult` / `readPackFileResult`; `PackDomain` -> `{name, origin, fileCount}`, `PackFile` -> `{path, size}` (int64: arrives as `string | number`), `ReadPackFileResult` -> `{requestId, domain, path, source, origin, found}`. The Go SDK mirror is `sdk/go/pack/pack.go` (`Domain` / `File` / `FileSource`, `ListDomains` / `ListFiles` / `ReadFile`); keep the TS names parallel. Note the existing, unrelated `SetPackEnabledPayload.packDomain` (module registry) -- name nothing `Pack*Enabled`.

- [ ] **Step 1: Write the failing tests**

Create `sdk/ts/test/pack.test.ts`:

```ts
import test from "node:test";
import assert from "node:assert/strict";

import { PackClient } from "../src/pack/pack.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// Local stand-in, as every client test in this directory keeps its own.
class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
  private nextId = 0;

  send(msg: ClientMessage): string {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return id;
  }

  async sendAndWait(msg: ClientMessage): Promise<ServerMessage> {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return new Promise<ServerMessage>((resolve) => {
      this.pendingReplies.set(id, resolve);
    });
  }

  registerStream(_requestId: string, _handler: (msg: ServerMessage) => void): () => void {
    return () => {};
  }

  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  lastSent(): ClientMessage {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.lastSent: nothing sent yet");
    return last.msg;
  }
}

function newClient(): { mock: MockDispatcher; client: PackClient } {
  const mock = new MockDispatcher();
  return { mock, client: new PackClient(mock as unknown as Dispatcher) };
}

test("listDomains sends listPackDomains with a requestId and maps the reply", async () => {
  const { mock, client } = newClient();
  const pending = client.listDomains();
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string }>;
  assert.ok(sent.listPackDomains, "envelope must carry a listPackDomains payload");
  assert.ok((sent.listPackDomains.requestId ?? "").length > 0);
  mock.reply({
    listPackDomainsResult: {
      requestId: sent.listPackDomains.requestId,
      domains: [{ name: "cognition", origin: "embedded", fileCount: 7 }, { name: "acme" }],
    },
  });
  assert.deepEqual(await pending, [
    { name: "cognition", origin: "embedded", fileCount: 7 },
    { name: "acme", origin: "", fileCount: 0 },
  ]);
});

test("listFiles carries the domain and coerces an int64 size from string or number", async () => {
  const { mock, client } = newClient();
  const pending = client.listFiles("cognition");
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string; domain?: string }>;
  assert.equal(sent.listPackFiles?.domain, "cognition");
  mock.reply({
    listPackFilesResult: {
      requestId: sent.listPackFiles?.requestId,
      domain: "cognition",
      files: [{ path: "queries.memql", size: "1204" }, { path: "prompts/x.tmpl", size: 33 }, { path: "shapes.memql" }],
    },
  });
  assert.deepEqual(await pending, [
    { path: "queries.memql", size: 1204 },
    { path: "prompts/x.tmpl", size: 33 },
    { path: "shapes.memql", size: 0 },
  ]);
});

test("readFile carries domain and path and returns the source with its origin", async () => {
  const { mock, client } = newClient();
  const pending = client.readFile("cognition", "queries.memql");
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string; domain?: string; path?: string }>;
  assert.equal(sent.readPackFile?.domain, "cognition");
  assert.equal(sent.readPackFile?.path, "queries.memql");
  mock.reply({
    readPackFileResult: {
      requestId: sent.readPackFile?.requestId,
      domain: "cognition",
      path: "queries.memql",
      source: "query space spaces {}\n",
      origin: "embedded",
      found: true,
    },
  });
  assert.deepEqual(await pending, {
    domain: "cognition",
    path: "queries.memql",
    source: "query space spaces {}\n",
    origin: "embedded",
    found: true,
  });
});

// A missing file is a normal answer, not an error: the engine replies
// found=false with no wire error, exactly as sdk/go/pack documents.
test("readFile resolves found=false rather than throwing for a missing file", async () => {
  const { mock, client } = newClient();
  const pending = client.readFile("cognition", "nope.memql");
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string }>;
  mock.reply({ readPackFileResult: { requestId: sent.readPackFile?.requestId, domain: "cognition", path: "nope.memql" } });
  assert.deepEqual(await pending, { domain: "cognition", path: "nope.memql", source: "", origin: "", found: false });
});

test("a queryError reply throws with the engine's message, naming the call", async () => {
  const { mock, client } = newClient();
  const pending = client.listFiles("cognition");
  mock.reply({ queryError: { requestId: "r", error: { message: "not permitted" } } });
  await assert.rejects(pending, /listFiles: not permitted/);
});

test("an unexpected reply envelope throws rather than resolving empty", async () => {
  const { mock, client } = newClient();
  const pending = client.listDomains();
  mock.reply({ listConstructsResult: { requestId: "r", constructs: [] } });
  await assert.rejects(pending, /listDomains: unexpected reply envelope/);
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd sdk/ts && npm test 2>&1 | tail -20`
Expected: compile error -- `../src/pack/pack.js` does not exist.

- [ ] **Step 3: Add the wire types**

In `sdk/ts/src/client/wire.ts`, beside `ListConstructsPayload`:

```ts
// Pack browser (memql#2127 / B1): read-only enumeration of the embedded,
// plugin-registered and MEMQL_DSL_PATH .memql trees. Three single-reply
// exchanges -- the Go routing ledger (sdk/go/client/dispatcher_stream_routing_test.go)
// already classifies the three results as single-reply, so nothing here joins
// streamRequestId.
export interface ListPackDomainsPayload {
  requestId: string;
}

export interface ListPackFilesPayload {
  requestId: string;
  domain: string;
}

export interface ReadPackFilePayload {
  requestId: string;
  domain: string;
  path: string;
}
```

Beside `ListConstructsResultPayload`:

```ts
export interface PackDomainWire {
  name?: string;
  origin?: string;
  fileCount?: number;
}

export interface PackFileWire {
  path?: string;
  // int64 on the wire: protojson may encode it as a string.
  size?: string | number;
}

export interface ListPackDomainsResultPayload {
  requestId?: string;
  domains?: PackDomainWire[];
}

export interface ListPackFilesResultPayload {
  requestId?: string;
  domain?: string;
  files?: PackFileWire[];
}

export interface ReadPackFileResultPayload {
  requestId?: string;
  domain?: string;
  path?: string;
  source?: string;
  origin?: string;
  found?: boolean;
}
```

Add to the `ClientPayload` union: `| { listPackDomains: ListPackDomainsPayload } | { listPackFiles: ListPackFilesPayload } | { readPackFile: ReadPackFilePayload }`.
Add to the `ServerPayload` union: `| { listPackDomainsResult: ListPackDomainsResultPayload } | { listPackFilesResult: ListPackFilesResultPayload } | { readPackFileResult: ReadPackFileResultPayload }`.
In `readServerPayload`: add the three `{ kind: "..."; value: ... }` members to its return-type union and three `if (m.<key>) return { kind: "<key>", value: m.<key> as <Type> };` blocks, placed beside the `listConstructsResult` block.

- [ ] **Step 4: Write the client**

Create `sdk/ts/src/pack/pack.ts`:

```ts
// The pack browser: what .memql files a cluster carries, and their source.
//
// Three single-reply calls over the dispatcher, in the shape of ConstructsClient.
// The Go SDK's sdk/go/pack is the mirror; the names are kept parallel on purpose.
//
// A MISSING FILE IS NOT AN ERROR. The engine answers ReadPackFile with
// found=false and no wire error, so readFile resolves with found=false; only
// a queryError or an unrecognised envelope throws. A caller rendering a file
// that is not there should say so, not show an exception.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type { PackDomainWire, PackFileWire } from "../client/wire.js";

export interface PackDomain {
  name: string;
  /** "embedded" | "pack:<domain>" */
  origin: string;
  fileCount: number;
}

export interface PackFile {
  /** Relative to the domain root, e.g. "queries.memql" or "prompts/x.tmpl". */
  path: string;
  size: number;
}

export interface PackFileSource {
  domain: string;
  path: string;
  source: string;
  /** "embedded" | "pack:<domain>" | "disk:<path>" */
  origin: string;
  /** false when the file does not exist -- a normal answer, not an error. */
  found: boolean;
}

export interface PackCallOptions {
  signal?: AbortSignal;
}

export class PackClient {
  constructor(private readonly dispatcher: Dispatcher) {
    if (!dispatcher) throw new Error("PackClient: dispatcher is required");
  }

  async listDomains(opts: PackCallOptions = {}): Promise<PackDomain[]> {
    const reply = await this.dispatcher.sendAndWait(
      { listPackDomains: { requestId: newShortId() } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(`listDomains: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind !== "listPackDomainsResult") {
      throw new Error("listDomains: unexpected reply envelope");
    }
    return (payload.value.domains ?? []).map(domainFromWire);
  }

  async listFiles(domain: string, opts: PackCallOptions = {}): Promise<PackFile[]> {
    const reply = await this.dispatcher.sendAndWait(
      { listPackFiles: { requestId: newShortId(), domain } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(`listFiles: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind !== "listPackFilesResult") {
      throw new Error("listFiles: unexpected reply envelope");
    }
    return (payload.value.files ?? []).map(fileFromWire);
  }

  async readFile(domain: string, path: string, opts: PackCallOptions = {}): Promise<PackFileSource> {
    const reply = await this.dispatcher.sendAndWait(
      { readPackFile: { requestId: newShortId(), domain, path } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(`readFile: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind !== "readPackFileResult") {
      throw new Error("readFile: unexpected reply envelope");
    }
    const v = payload.value;
    return {
      domain: v.domain ?? domain,
      path: v.path ?? path,
      source: v.source ?? "",
      origin: v.origin ?? "",
      found: v.found === true,
    };
  }
}

// Every field is defaulted: protojson omits zero values, so a raw read hands
// the consumer `undefined` for exactly the common case.
function domainFromWire(d: PackDomainWire): PackDomain {
  return { name: d.name ?? "", origin: d.origin ?? "", fileCount: d.fileCount ?? 0 };
}

function fileFromWire(f: PackFileWire): PackFile {
  const raw = f.size;
  const size = typeof raw === "string" ? Number(raw) : (raw ?? 0);
  return { path: f.path ?? "", size: Number.isFinite(size) ? size : 0 };
}
```

Create `sdk/ts/src/pack/index.ts`:

```ts
export {
  PackClient,
  type PackCallOptions,
  type PackDomain,
  type PackFile,
  type PackFileSource,
} from "./pack.js";
```

Add `export * as pack from "./pack/index.js";` to `sdk/ts/src/index.ts`, and an `exports["./pack"]` entry in `sdk/ts/package.json` shaped exactly like `"./constructs"`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd sdk/ts && npm run typecheck && npm test 2>&1 | tail -12`
Expected: all tests pass including `test/exports.test.ts` (the new subpath is declared) and the six new pack tests.

- [ ] **Step 6: Commit**

```bash
git add sdk/ts/src/pack/pack.ts sdk/ts/src/pack/index.ts sdk/ts/test/pack.test.ts sdk/ts/src/client/wire.ts sdk/ts/src/index.ts sdk/ts/package.json
git commit -m "sdk/ts: pack-browser client (ListPackDomains / ListPackFiles / ReadPackFile)" -m "Refs memql#4247"
```

---

### Task 2: `runtime-config.json` gains `domain` -- closes #4249

**Files:**
- Modify: `component/edge/runtimeconfig.go` (struct `RuntimeConfig`; `runtimeConfigForSite`; new helper `domainFromEnv`)
- Modify: `component/edge/runtimeconfig_test.go` (extend every `RuntimeConfig{...}` literal; add the two new cases)
- Modify: `clients/portal/src/cluster/config.ts` (`PortalRuntimeConfig.domain`, `UNKNOWN_RUNTIME_CONFIG`, `normalizeRuntimeConfig`)
- Modify: `clients/portal/test/runtimeConfig.test.ts`
- Modify: every portal test fixture that builds a full `PortalRuntimeConfig` literal (find them with `grep -rln 'identityUrl: ""' clients/portal/test`; each gains `domain: ""` or a real value)

**Interfaces:**
- Produces: JSON field `domain` (string; `""` when the node has no `MEMQL_DOMAIN`; never omitted), and `PortalRuntimeConfig.domain: string` (normalized to `""` when absent). Task 3 reads `config.domain`.

- [ ] **Step 1: Write the failing Go tests**

Append to `component/edge/runtimeconfig_test.go`:

```go
func TestRuntimeConfigForSite_CarriesTheDomain(t *testing.T) {
	env := fakeEnv(map[string]string{
		"MEMQL_DOMAIN":            "  acme.example.com ",
		"MEMQL_IDENTITY_BASE_URL": "https://identity.acme.example.com",
	})
	got := runtimeConfigForSite(&Site{ID: "s1", Hostname: "shop.acme.example.com"}, env, true)
	if got.Domain != "acme.example.com" {
		t.Errorf("Domain = %q, want the trimmed MEMQL_DOMAIN", got.Domain)
	}
}

// An unset MEMQL_DOMAIN is served as an empty string, never omitted: a reader
// can then tell "this node predates the field" (key absent) from "this node
// has no domain" (key present, empty).
func TestServeRuntimeConfig_DomainKeyIsAlwaysPresent(t *testing.T) {
	doc := runtimeConfigForSite(&Site{ID: "s1", Hostname: "x"}, fakeEnv(map[string]string{}), false)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	v, present := m["domain"]
	if !present {
		t.Fatalf("domain key missing from %s", raw)
	}
	if v != "" {
		t.Errorf("domain = %v, want empty string", v)
	}
}
```

(Add `"encoding/json"` to the test's imports if it is not already there.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./component/edge/ -run 'TestRuntimeConfigForSite_CarriesTheDomain|TestServeRuntimeConfig_DomainKeyIsAlwaysPresent' 2>&1 | tail -5`
Expected: compile error `got.Domain undefined`.

- [ ] **Step 3: Implement**

In `component/edge/runtimeconfig.go`: add to `RuntimeConfig` (after `AuthEnabled`):

```go
	// Domain is the cluster's configured MEMQL_DOMAIN -- the value every
	// role host (api., identity., mcp.) is derived from (memql#3767). Served
	// so a client can compose an address that names THIS cluster (the
	// portal's "open in VS Code" link carries it as the cluster key) without
	// reverse-engineering it from identityUrl. Empty, never omitted, when
	// the node has no domain configured.
	Domain string `json:"domain"`
```

Add the helper beside `identityURLFromEnv`:

```go
// domainFromEnv reads MEMQL_DOMAIN through the injected env, trimmed. It
// deliberately does not import component/envregistry: that package derives
// OTHER variables from the domain and never exposes the domain itself, and
// this file's existing identityURLFromEnv already reads the derived values
// the same way.
func domainFromEnv(env func(string) string) string {
	return strings.TrimSpace(env("MEMQL_DOMAIN"))
}
```

Set `Domain: domainFromEnv(env),` in `runtimeConfigForSite`'s returned literal. Add `"strings"` to the imports if absent. Extend every `RuntimeConfig{...}` literal in the existing tests with a `Domain` value matching their `env` (empty when the fixture sets no `MEMQL_DOMAIN`).

- [ ] **Step 4: Run the Go tests**

Run: `go test ./component/edge/... 2>&1 | tail -3`
Expected: `ok`.

- [ ] **Step 5: Write the failing portal test**

Append to `clients/portal/test/runtimeConfig.test.ts` (inside the existing `describe`, following its style):

```ts
  it("reads the domain, and defaults it to empty when an older node omits it", () => {
    expect(
      normalizeRuntimeConfig({
        identityUrl: "https://identity.acme.example.com",
        identityApiBaseUrl: "",
        oauthClientId: "portal",
        authEnabled: true,
        domain: "acme.example.com",
      }).domain,
    ).toBe("acme.example.com");
    expect(
      normalizeRuntimeConfig({
        identityUrl: "https://identity.acme.example.com",
        identityApiBaseUrl: "",
        oauthClientId: "portal",
        authEnabled: true,
      }).domain,
    ).toBe("");
  });
```

- [ ] **Step 6: Implement the portal side**

In `clients/portal/src/cluster/config.ts`: add to `PortalRuntimeConfig`:

```ts
  // The cluster's configured domain (MEMQL_DOMAIN), served by the node since
  // memql#4249. Empty when an older node omits it or none is configured;
  // src/cluster/editorLink.ts derives a fallback from identityUrl.
  domain: string;
```

Add `domain: ""` to `UNKNOWN_RUNTIME_CONFIG`, and in `normalizeRuntimeConfig` read it defensively in the same style as the other string fields (trimmed; `""` when absent or not a string). Then run `make portal-typecheck`; every fixture it reports as missing `domain` gets `domain: ""` (the grep in the Files list finds them).

- [ ] **Step 7: Run the portal checks**

Run: `make portal-typecheck && make portal-test 2>&1 | tail -6`
Expected: typecheck clean; all tests pass.

- [ ] **Step 8: Commit**

```bash
git add component/edge/runtimeconfig.go component/edge/runtimeconfig_test.go clients/portal/src/cluster/config.ts clients/portal/test/runtimeConfig.test.ts
git add <each fixture file the grep found>
git commit -m "edge: runtime-config carries the cluster domain; portal reads it" -m "Refs memql#4249"
```

---

### Task 3: Portal "Open definition in VS Code" -- closes #4250

**Files:**
- Create: `clients/portal/src/cluster/editorLink.ts` (pure: domain resolution, URI composition, scheme preference)
- Create: `clients/portal/src/components/OpenInVsCode.tsx`
- Create: `clients/portal/test/editorLink.test.ts`
- Create: `clients/portal/test/openInVsCode.test.tsx`
- Modify: `clients/portal/src/ui/Button.tsx` (add `ButtonLink`, an anchor sharing the same `TONE` / `SIZE` maps)
- Modify: `clients/portal/src/ui/index.ts` (export `ButtonLink`)
- Modify: `clients/portal/src/ui/icons.ts` (re-export `ExternalLink` from `lucide-react`)
- Modify: `clients/portal/src/pages/ConceptPage.tsx` (header, after the `<Chip>`s)

**Interfaces:**
- Consumes: `useAuth().config.domain` / `.identityUrl` (Task 2), `concept.id`.
- Produces:

```ts
// editorLink.ts
export type EditorScheme = "vscode" | "vscode-insiders";
export const EDITOR_SCHEME_STORAGE_KEY = "memql-portal-editor-scheme";
export function clusterDomainFor(config: { domain: string; identityUrl: string }): string;
export function editorOpenUri(input: { scheme: EditorScheme; domain: string; kind: string; name: string }): string;
export function readStoredEditorScheme(): EditorScheme;
export function storeEditorScheme(scheme: EditorScheme): void;
export const EXTENSION_INSTALL_URL =
  "https://github.com/znasllc-io/memql/blob/main/editors/vscode/README.md#install--update-the-extension-locally";
```

`clusterDomainFor` returns `config.domain` when non-empty; otherwise the host of `identityUrl` with a leading `identity.` label removed (exact by the front-door rule, memql#3767: every role host is one label under the domain); otherwise `""`. A host that does not start with `identity.` yields `""` -- guessing is worse than hiding the control.

- [ ] **Step 1: Write the failing pure tests**

Create `clients/portal/test/editorLink.test.ts`:

```ts
import { afterEach, describe, expect, it } from "vitest";

import {
  EDITOR_SCHEME_STORAGE_KEY,
  clusterDomainFor,
  editorOpenUri,
  readStoredEditorScheme,
  storeEditorScheme,
} from "../src/cluster/editorLink";

describe("clusterDomainFor", () => {
  it("prefers the served domain", () => {
    expect(clusterDomainFor({ domain: "acme.example.com", identityUrl: "https://identity.other.test" })).toBe(
      "acme.example.com",
    );
  });
  it("derives the domain from identityUrl when the node omits it", () => {
    expect(clusterDomainFor({ domain: "", identityUrl: "https://identity.acme.example.com" })).toBe("acme.example.com");
    expect(clusterDomainFor({ domain: "", identityUrl: "https://identity.memql.localhost:443/" })).toBe("memql.localhost");
  });
  it("refuses to guess from a host that is not the identity role host", () => {
    expect(clusterDomainFor({ domain: "", identityUrl: "https://login.acme.example.com" })).toBe("");
    expect(clusterDomainFor({ domain: "", identityUrl: "" })).toBe("");
    expect(clusterDomainFor({ domain: "", identityUrl: "not a url" })).toBe("");
  });
});

describe("editorOpenUri", () => {
  it("composes the v=1 contract with every value encoded once", () => {
    expect(
      editorOpenUri({ scheme: "vscode", domain: "acme.example.com", kind: "concept", name: "v1:cognition:space" }),
    ).toBe("vscode://znasllc.memql/open?v=1&cluster=acme.example.com&kind=concept&name=v1%3Acognition%3Aspace");
  });
  it("swaps only the scheme for Insiders", () => {
    expect(editorOpenUri({ scheme: "vscode-insiders", domain: "d.test", kind: "query", name: "a b" })).toBe(
      "vscode-insiders://znasllc.memql/open?v=1&cluster=d.test&kind=query&name=a%20b",
    );
  });
});

describe("the remembered scheme", () => {
  afterEach(() => {
    globalThis.localStorage?.removeItem(EDITOR_SCHEME_STORAGE_KEY);
  });
  it("defaults to vscode and round-trips Insiders", () => {
    expect(readStoredEditorScheme()).toBe("vscode");
    storeEditorScheme("vscode-insiders");
    expect(readStoredEditorScheme()).toBe("vscode-insiders");
    storeEditorScheme("vscode");
    expect(readStoredEditorScheme()).toBe("vscode");
  });
  it("ignores a stored value it does not recognise", () => {
    globalThis.localStorage?.setItem(EDITOR_SCHEME_STORAGE_KEY, "emacs");
    expect(readStoredEditorScheme()).toBe("vscode");
  });
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd clients/portal && npx vitest run test/editorLink.test.ts 2>&1 | tail -8`
Expected: FAIL -- module `../src/cluster/editorLink` not found.

- [ ] **Step 3: Write `editorLink.ts`**

```ts
// The portal -> VS Code handoff link (memql#4250; design section 4.1-4.2).
//
// The contract is ONE shape, versioned:
//   <scheme>://znasllc.memql/open?v=1&cluster=<domain>&kind=<kind>&name=<registry key>
// The extension's resolver refuses any other `v`, so a portal that needs a
// different shape bumps the version rather than adding a key the old handler
// would silently ignore.
//
// THE CLUSTER KEY IS THE DOMAIN, because it is the one value the extension's
// add/edit flow stores (ClusterConfig.domain); endpoint and issuer compose
// from it. The node serves it in runtime-config.json; an older node does not,
// and for that case the identity host is exact, not a guess: every role host
// is a single label under the domain (memql#3767), so identity.<domain> minus
// its first label IS the domain. Any other host shape yields "" -- the caller
// hides the control rather than sending someone to the wrong cluster.

export type EditorScheme = "vscode" | "vscode-insiders";

export const EDITOR_SCHEME_STORAGE_KEY = "memql-portal-editor-scheme";

export const EXTENSION_INSTALL_URL =
  "https://github.com/znasllc-io/memql/blob/main/editors/vscode/README.md#install--update-the-extension-locally";

export function clusterDomainFor(config: { domain: string; identityUrl: string }): string {
  const served = config.domain.trim();
  if (served !== "") return served;
  let host = "";
  try {
    host = new URL(config.identityUrl).hostname;
  } catch {
    return "";
  }
  const prefix = "identity.";
  if (!host.startsWith(prefix) || host.length <= prefix.length) return "";
  return host.slice(prefix.length);
}

export function editorOpenUri(input: { scheme: EditorScheme; domain: string; kind: string; name: string }): string {
  const query = [
    ["v", "1"],
    ["cluster", input.domain],
    ["kind", input.kind],
    ["name", input.name],
  ]
    .map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
    .join("&");
  return `${input.scheme}://znasllc.memql/open?${query}`;
}

function isEditorScheme(value: string | null): value is EditorScheme {
  return value === "vscode" || value === "vscode-insiders";
}

// Same try/catch shape as app/theme.ts: localStorage can be blocked, and a
// remembered editor choice is not worth failing the page over.
export function readStoredEditorScheme(): EditorScheme {
  try {
    const raw = globalThis.localStorage?.getItem(EDITOR_SCHEME_STORAGE_KEY) ?? null;
    return isEditorScheme(raw) ? raw : "vscode";
  } catch {
    return "vscode";
  }
}

export function storeEditorScheme(scheme: EditorScheme): void {
  try {
    globalThis.localStorage?.setItem(EDITOR_SCHEME_STORAGE_KEY, scheme);
  } catch {
    // Not worth failing over.
  }
}
```

- [ ] **Step 4: Run the pure tests**

Run: `cd clients/portal && npx vitest run test/editorLink.test.ts 2>&1 | tail -6`
Expected: all pass.

- [ ] **Step 5: Write the failing component test**

Create `clients/portal/test/openInVsCode.test.tsx`:

```tsx
import { afterEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { OpenInVsCode } from "../src/components/OpenInVsCode";
import { EDITOR_SCHEME_STORAGE_KEY } from "../src/cluster/editorLink";

afterEach(() => {
  globalThis.localStorage?.removeItem(EDITOR_SCHEME_STORAGE_KEY);
});

describe("OpenInVsCode", () => {
  it("is a link carrying the v=1 contract, with the install pointer beside it", () => {
    render(<OpenInVsCode domain="acme.example.com" kind="concept" name="v1:cognition:space" />);
    const link = screen.getByRole("link", { name: "Open definition in VS Code" });
    expect(link.getAttribute("href")).toBe(
      "vscode://znasllc.memql/open?v=1&cluster=acme.example.com&kind=concept&name=v1%3Acognition%3Aspace",
    );
    const help = screen.getByRole("link", { name: "how to install" });
    expect(help.getAttribute("href")).toContain("editors/vscode/README.md#install--update-the-extension-locally");
    expect(help.getAttribute("target")).toBe("_blank");
    expect(help.getAttribute("rel")).toContain("noopener");
  });

  it("switches to Insiders and remembers it", () => {
    const { unmount } = render(<OpenInVsCode domain="d.test" kind="concept" name="x" />);
    fireEvent.click(screen.getByRole("button", { name: "Use VS Code Insiders" }));
    expect(screen.getByRole("link", { name: "Open definition in VS Code Insiders" }).getAttribute("href")).toMatch(
      /^vscode-insiders:\/\//,
    );
    unmount();
    render(<OpenInVsCode domain="d.test" kind="concept" name="x" />);
    expect(screen.getByRole("link", { name: "Open definition in VS Code Insiders" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Use VS Code" })).toBeTruthy();
  });

  it("renders nothing when the cluster's domain is unknown", () => {
    const { container } = render(<OpenInVsCode domain="" kind="concept" name="x" />);
    expect(container.textContent).toBe("");
  });
});
```

- [ ] **Step 6: Run to verify it fails**

Run: `cd clients/portal && npx vitest run test/openInVsCode.test.tsx 2>&1 | tail -6`
Expected: FAIL -- component module not found.

- [ ] **Step 7: Add `ButtonLink` and the icon**

In `clients/portal/src/ui/Button.tsx`, after `Button`, sharing the SAME `TONE` / `SIZE` maps (do not copy the class strings):

```tsx
// A link that looks like a Button. For navigations that are not clicks with
// side effects -- a deep link hands the browser a URL, and an anchor lets the
// browser own the "open this application?" gesture.
export function ButtonLink({
  tone = "quiet",
  size = "sm",
  href,
  title,
  target,
  rel,
  children,
}: {
  tone?: ButtonTone;
  size?: ButtonSize;
  href: string;
  title?: string;
  target?: string;
  rel?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <a href={href} title={title} target={target} rel={rel} className={classesFor(tone, size)}>
      {children}
    </a>
  );
}
```

Extract the class composition `Button` already does into a shared `function classesFor(tone: ButtonTone, size: ButtonSize): string` used by both, so the two cannot drift. Export `ButtonLink` from `clients/portal/src/ui/index.ts`. In `clients/portal/src/ui/icons.ts` add `ExternalLink` to the `lucide-react` re-export list.

- [ ] **Step 8: Write the component**

Create `clients/portal/src/components/OpenInVsCode.tsx`:

```tsx
import { useState, type ReactNode } from "react";

import {
  EXTENSION_INSTALL_URL,
  editorOpenUri,
  readStoredEditorScheme,
  storeEditorScheme,
  type EditorScheme,
} from "../cluster/editorLink";
import { ButtonLink } from "../ui";
import { ExternalLink } from "../ui/icons";

// The handoff control (design section 4.2). The portal renders no source --
// this is the one door to it, and it opens in the editor. Nothing here can
// tell whether the extension is installed, so the install pointer is always
// beside the link and the copy says what the link needs.
//
// The id is referenced only through the `name` prop: this directory is on
// the concept-agnostic render path and may not name a concept.
export function OpenInVsCode({ domain, kind, name }: { domain: string; kind: string; name: string }): ReactNode {
  const [scheme, setScheme] = useState<EditorScheme>(() => readStoredEditorScheme());
  if (domain === "") return null;

  const insiders = scheme === "vscode-insiders";
  const editorName = insiders ? "VS Code Insiders" : "VS Code";
  const other: EditorScheme = insiders ? "vscode" : "vscode-insiders";
  const otherName = insiders ? "VS Code" : "VS Code Insiders";

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <ButtonLink href={editorOpenUri({ scheme, domain, kind, name })} title={`Opens the definition in ${editorName}`}>
        <ExternalLink size={13} aria-hidden="true" />
        <span>Open definition in {editorName}</span>
      </ButtonLink>
      <span className="text-xs text-muted">
        Needs the MemQL extension for VS Code.{" "}
        <a href={EXTENSION_INSTALL_URL} target="_blank" rel="noopener noreferrer" className="text-accent hover:underline">
          how to install
        </a>
        {" - "}
        <button
          type="button"
          className="text-accent hover:underline"
          onClick={() => {
            storeEditorScheme(other);
            setScheme(other);
          }}
        >
          Use {otherName}
        </button>
      </span>
    </div>
  );
}
```

(The `ButtonLink` renders an anchor; if `Button`'s children layout uses `inline-flex items-center gap-1.5`, `classesFor` carries it for both.)

- [ ] **Step 9: Mount it on the concept page**

In `clients/portal/src/pages/ConceptPage.tsx`: import `useAuth` from `../auth/AuthProvider`, `clusterDomainFor` from `../cluster/editorLink`, and `OpenInVsCode` from `../components/OpenInVsCode`. Inside `ConceptPage`, after `const { status } = useCluster();`, add `const { config } = useAuth();`. In the `<header>` block, after the `<Chip>` elements, add:

```tsx
        <span className="basis-full" />
        <OpenInVsCode domain={clusterDomainFor(config)} kind="concept" name={concept.id} />
```

Do not write any concept-id-shaped literal in this file, in code or comments.

- [ ] **Step 10: Extend the browser test**

Append to `clients/portal/test/conceptBrowser.test.tsx`, inside the describe block that covers the concept page (keep the `AUTH_DISABLED_CLUSTER` fixture and give it `domain: "memql.test"`):

```tsx
  it("offers to open the concept's definition in VS Code, addressed to this cluster", async () => {
    renderBrowser(harness(), `/concepts/${NODE}`);
    const link = await screen.findByRole("link", { name: "Open definition in VS Code" });
    expect(link.getAttribute("href")).toBe(
      `vscode://znasllc.memql/open?v=1&cluster=memql.test&kind=concept&name=${encodeURIComponent(NODE)}`,
    );
  });
```

(`NODE` is the concept id fixture that file already uses; adapt the name if it differs.)

- [ ] **Step 11: Run every portal check and the root guards**

Run: `make portal-typecheck && make portal-test && make portal-build 2>&1 | tail -8 && go test . -run 'Portal' 2>&1 | tail -3`
Expected: typecheck clean, all tests pass, build clean, root portal guards `ok`.

- [ ] **Step 12: Commit**

```bash
git add clients/portal/src/cluster/editorLink.ts clients/portal/src/components/OpenInVsCode.tsx clients/portal/test/editorLink.test.ts clients/portal/test/openInVsCode.test.tsx clients/portal/src/ui/Button.tsx clients/portal/src/ui/index.ts clients/portal/src/ui/icons.ts clients/portal/src/pages/ConceptPage.tsx clients/portal/test/conceptBrowser.test.tsx
git commit -m "portal: open a concept's definition in VS Code from the concept page" -m "Refs memql#4250"
```

---

### Task 4: Cluster documents -- read-only source served from the cluster -- closes #4248

**Files:**
- Create: `editors/vscode/src/constructs/clusterDocument.ts` (pure: scheme, uri compose/parse, pack locator, notices; NO `vscode` import)
- Create: `editors/vscode/src/constructs/clusterDocuments.ts` (adapter: the `TextDocumentContentProvider`, the header CodeLens, `openClusterDocument`; imports `vscode` -- add it to `vscodeImportAllowList`)
- Create: `editors/vscode/test/clusterDocument.test.ts`
- Modify: `cmd/memql-lsp/vscodeimportrule_test.go` (`vscodeImportAllowList` gains `"constructs/clusterDocuments.ts"`)
- Modify: `editors/vscode/src/webview/constructPanel.ts` (deps; `viewSourceFromCluster` message; the dead-end message gains an action)
- Modify: `editors/vscode/src/webview/constructScreens.ts` (`ConstructPageInput.offerClusterSource`; the button)
- Modify: `editors/vscode/src/constructs/readonlyDecorations.ts` (badge + hover for the scheme)
- Modify: `editors/vscode/src/extension.ts` (register provider + lens + command `memql.constructs.showDetails`; narrow the LSP `documentSelector`; pass deps to `ConstructPanel.open`)
- Modify: `editors/vscode/package.json` (`memql.constructs.showDetails` command, palette-hidden)
- Modify: `editors/vscode/test-host/index.ts` (one host case)
- Test: the test file that already covers `renderConstructPage` (find it with `grep -l renderConstructPage editors/vscode/test/*.ts`; create `test/constructScreens.test.ts` if none)

**Interfaces:**
- Consumes: `PackClient.readFile(domain, path)` (Task 1), `signatureLine(source, kind, name)` (`src/constructs/signature.ts`), `CatalogConstruct`, `ConnectionManager.state` / `.dispatcher`, `toCatalogConstruct`.
- Produces (Task 5 consumes verbatim):

```ts
// clusterDocument.ts
export const CLUSTER_DOCUMENT_SCHEME = "memql-cluster";
export interface ClusterDocumentRef { cluster: string; originPath: string; kind: string; name: string; }
export function clusterDocumentUri(ref: ClusterDocumentRef): string;   // memql-cluster://<encodeURIComponent(cluster)>/<originPath>?kind=..&name=..
export function parseClusterDocumentUri(uri: { authority: string; path: string; query: string }): ClusterDocumentRef | undefined;
export function packLocator(originPath: string): { domain: string; path: string } | undefined;
export function notConnectedNotice(cluster: string): string;
export function notFoundNotice(cluster: string, originPath: string): string;
// clusterDocuments.ts
export class ClusterDocumentProvider implements vscode.TextDocumentContentProvider { constructor(deps: { connections: ConnectionManager }); readonly onDidChange: vscode.Event<vscode.Uri>; provideTextDocumentContent(uri: vscode.Uri): Promise<string>; invalidate(): void; dispose(): void; }
export class ClusterDocumentLens implements vscode.CodeLensProvider { provideCodeLenses(document: vscode.TextDocument): vscode.CodeLens[]; }
export async function openClusterDocument(ref: ClusterDocumentRef): Promise<vscode.TextEditor>;   // opens + reveals at the signature
```

`originPath` is relative to the DSL tree root (`component/memql/construct_catalog.go`: `"cognition/queries.memql"`); `packLocator` takes the first segment as the pack domain and the rest as the file path, dropping a leading `dsl/` if a catalog ever reports one.

- [ ] **Step 1: Write the failing pure tests**

Create `editors/vscode/test/clusterDocument.test.ts`:

```ts
import test from "node:test";
import assert from "node:assert/strict";

import {
  CLUSTER_DOCUMENT_SCHEME,
  clusterDocumentUri,
  notConnectedNotice,
  packLocator,
  parseClusterDocumentUri,
} from "../src/constructs/clusterDocument.js";

test("a cluster document uri round-trips its cluster, path and construct key", () => {
  const uri = clusterDocumentUri({
    cluster: "staging",
    originPath: "cognition/queries.memql",
    kind: "query",
    name: "spaceParticipants",
  });
  assert.equal(uri, `${CLUSTER_DOCUMENT_SCHEME}://staging/cognition/queries.memql?kind=query&name=spaceParticipants`);
  assert.deepEqual(
    parseClusterDocumentUri({ authority: "staging", path: "/cognition/queries.memql", query: "kind=query&name=spaceParticipants" }),
    { cluster: "staging", originPath: "cognition/queries.memql", kind: "query", name: "spaceParticipants" },
  );
});

test("a cluster name with a space survives the authority", () => {
  const uri = clusterDocumentUri({ cluster: "my lab", originPath: "a/b.memql", kind: "concept", name: "v1:a:b" });
  assert.ok(uri.startsWith(`${CLUSTER_DOCUMENT_SCHEME}://my%20lab/`));
  assert.equal(parseClusterDocumentUri({ authority: "my%20lab", path: "/a/b.memql", query: "kind=concept&name=v1%3Aa%3Ab" })?.name, "v1:a:b");
});

test("a malformed uri parses to undefined rather than a half-filled ref", () => {
  assert.equal(parseClusterDocumentUri({ authority: "", path: "/a.memql", query: "kind=query&name=x" }), undefined);
  assert.equal(parseClusterDocumentUri({ authority: "c", path: "/", query: "kind=query&name=x" }), undefined);
  assert.equal(parseClusterDocumentUri({ authority: "c", path: "/a.memql", query: "name=x" }), undefined);
});

test("the pack locator splits the domain off the origin path", () => {
  assert.deepEqual(packLocator("cognition/queries.memql"), { domain: "cognition", path: "queries.memql" });
  assert.deepEqual(packLocator("cognition/prompts/reply.tmpl"), { domain: "cognition", path: "prompts/reply.tmpl" });
  assert.deepEqual(packLocator("dsl/cognition/queries.memql"), { domain: "cognition", path: "queries.memql" });
  assert.equal(packLocator("queries.memql"), undefined);
  assert.equal(packLocator(""), undefined);
});

test("the not-connected notice names the cluster and the way back", () => {
  const notice = notConnectedNotice("staging");
  assert.match(notice, /staging/);
  assert.match(notice, /reconnect/i);
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd editors/vscode && npm test 2>&1 | grep -E "clusterDocument|error TS" | head`
Expected: compile error -- the module does not exist.

- [ ] **Step 3: Write the pure module**

Create `editors/vscode/src/constructs/clusterDocument.ts`:

```ts
// A construct's file, served from the cluster that loaded it (design 4.5).
//
// THE FILE IS NOT ON THIS MACHINE, and this is the honest rendering of that:
// a read-only document whose bytes come from the cluster's own pack browser
// (ReadPackFile), opened at the construct's signature. It is distinct from
// `memql-catalog:` (catalogTarget.ts), which is a non-resolvable sentinel the
// run path uses and which nothing can open.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go);
// clusterDocuments.ts is the adapter.

export const CLUSTER_DOCUMENT_SCHEME = "memql-cluster";

export interface ClusterDocumentRef {
  /** The registry name of the cluster the bytes come from. */
  cluster: string;
  /** Relative to the DSL tree root, as the catalog reports it. */
  originPath: string;
  kind: string;
  name: string;
}

export function clusterDocumentUri(ref: ClusterDocumentRef): string {
  const query = `kind=${encodeURIComponent(ref.kind)}&name=${encodeURIComponent(ref.name)}`;
  return `${CLUSTER_DOCUMENT_SCHEME}://${encodeURIComponent(ref.cluster)}/${ref.originPath}?${query}`;
}

export function parseClusterDocumentUri(uri: {
  authority: string;
  path: string;
  query: string;
}): ClusterDocumentRef | undefined {
  const cluster = safeDecode(uri.authority);
  const originPath = uri.path.replace(/^\/+/, "");
  if (cluster === "" || originPath === "") return undefined;
  const params = new URLSearchParams(uri.query);
  const kind = params.get("kind") ?? "";
  const name = params.get("name") ?? "";
  if (kind === "" || name === "") return undefined;
  return { cluster, originPath, kind, name };
}

/** The pack-browser coordinates of an origin path: the first segment is the domain. */
export function packLocator(originPath: string): { domain: string; path: string } | undefined {
  let p = originPath.replace(/\\/g, "/").replace(/^\.\//, "");
  if (p.startsWith("dsl/")) p = p.slice("dsl/".length);
  const slash = p.indexOf("/");
  if (slash <= 0 || slash === p.length - 1) return undefined;
  return { domain: p.slice(0, slash), path: p.slice(slash + 1) };
}

export function notConnectedNotice(cluster: string): string {
  return (
    `// Not connected to ${cluster}.\n` +
    `// This document is served from the cluster; reconnect to ${cluster} and reopen it.\n`
  );
}

export function notFoundNotice(cluster: string, originPath: string): string {
  return `// ${cluster} does not serve ${originPath}.\n// The catalog named this path, but the pack browser has no such file.\n`;
}

function safeDecode(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}
```

- [ ] **Step 4: Run the pure tests**

Run: `cd editors/vscode && npm test 2>&1 | tail -6`
Expected: pass, count up by 5.

- [ ] **Step 5: Write the adapter**

Create `editors/vscode/src/constructs/clusterDocuments.ts`:

```ts
// The vscode half of cluster documents: a content provider over ReadPackFile,
// one header lens, and the open-and-reveal helper. Every decision lives in
// clusterDocument.ts; this converts.
//
// THE CACHE DIES WITH THE CONNECTION. A document fetched from one cluster must
// not be presented as current after a switch or a drop, so invalidate() clears
// it on every connection-state change and fires onDidChange for every open
// document of this scheme -- VS Code re-asks, and the provider answers with the
// not-connected notice until the stream is back.

import * as vscode from "vscode";
import { PackClient } from "@znasllc-io/memql-sdk-core/pack";

import type { ConnectionManager } from "../connection/manager.js";
import { signatureLine } from "./signature.js";
import {
  CLUSTER_DOCUMENT_SCHEME,
  clusterDocumentUri,
  notConnectedNotice,
  notFoundNotice,
  packLocator,
  parseClusterDocumentUri,
  type ClusterDocumentRef,
} from "./clusterDocument.js";

export class ClusterDocumentProvider implements vscode.TextDocumentContentProvider {
  private readonly changed = new vscode.EventEmitter<vscode.Uri>();
  readonly onDidChange = this.changed.event;
  private readonly cache = new Map<string, string>();
  private readonly unsubscribe: () => void;

  constructor(private readonly deps: { connections: ConnectionManager }) {
    this.unsubscribe = deps.connections.onDidChangeState(() => this.invalidate());
  }

  async provideTextDocumentContent(uri: vscode.Uri): Promise<string> {
    const key = uri.toString();
    const cached = this.cache.get(key);
    if (cached !== undefined) return cached;
    const ref = parseClusterDocumentUri({ authority: uri.authority, path: uri.path, query: uri.query });
    if (ref === undefined) return "// Not a cluster document.\n";
    const state = this.deps.connections.state;
    const dispatcher = this.deps.connections.dispatcher;
    if (dispatcher === undefined || state.status !== "connected" || state.clusterName !== ref.cluster) {
      return notConnectedNotice(ref.cluster);
    }
    const locator = packLocator(ref.originPath);
    if (locator === undefined) return notFoundNotice(ref.cluster, ref.originPath);
    const file = await new PackClient(dispatcher).readFile(locator.domain, locator.path);
    const text = file.found ? file.source : notFoundNotice(ref.cluster, ref.originPath);
    if (file.found) this.cache.set(key, text);
    return text;
  }

  invalidate(): void {
    const open = vscode.workspace.textDocuments.filter((d) => d.uri.scheme === CLUSTER_DOCUMENT_SCHEME);
    this.cache.clear();
    for (const d of open) this.changed.fire(d.uri);
  }

  dispose(): void {
    this.unsubscribe();
    this.changed.dispose();
  }
}

/** One lens at line 0: where the bytes came from, and the way to the detail page. */
export class ClusterDocumentLens implements vscode.CodeLensProvider {
  provideCodeLenses(document: vscode.TextDocument): vscode.CodeLens[] {
    const ref = parseClusterDocumentUri({ authority: document.uri.authority, path: document.uri.path, query: document.uri.query });
    if (ref === undefined) return [];
    return [
      new vscode.CodeLens(new vscode.Range(0, 0, 0, 0), {
        title: `From ${ref.cluster} -- read-only -- Open construct details`,
        command: "memql.constructs.showDetails",
        arguments: [{ kind: ref.kind, name: ref.name }],
      }),
    ];
  }
}

export async function openClusterDocument(ref: ClusterDocumentRef): Promise<vscode.TextEditor> {
  const uri = vscode.Uri.parse(clusterDocumentUri(ref));
  const document = await vscode.workspace.openTextDocument(uri);
  const editor = await vscode.window.showTextDocument(document, { viewColumn: vscode.ViewColumn.One, preview: true });
  const line = signatureLine(document.getText(), ref.kind, ref.name);
  if (line >= 0) {
    const at = new vscode.Position(line, 0);
    editor.selection = new vscode.Selection(at, at);
    editor.revealRange(new vscode.Range(at, at), vscode.TextEditorRevealType.InCenter);
  }
  return editor;
}
```

Add `"constructs/clusterDocuments.ts"` to `vscodeImportAllowList` in `cmd/memql-lsp/vscodeimportrule_test.go` (beside the other `constructs/` entries).

- [ ] **Step 6: Wire it in `extension.ts`**

In `startLanguageClient`, change the selector to `documentSelector: [{ language: 'memql', scheme: 'file' }]` with a one-line comment: cluster documents (`memql-cluster:`) must not receive import-resolution diagnostics for files that are not on disk.

In `registerRuntimeSurface`, after the Constructs tree block:

```ts
const clusterDocuments = new ClusterDocumentProvider({ connections });
const constructPanelDeps = (): ConstructPanelDeps => ({
  viewSourceFromCluster: async (construct) => {
    const state = connections?.state;
    if (state?.status !== 'connected') {
      void window.showInformationMessage('MemQL: connect to a cluster to read its source.');
      return;
    }
    await openClusterDocument({ cluster: state.clusterName, originPath: construct.originPath, kind: construct.kind, name: construct.name });
  },
});
context.subscriptions.push(
  clusterDocuments,
  workspace.registerTextDocumentContentProvider(CLUSTER_DOCUMENT_SCHEME, clusterDocuments),
  languages.registerCodeLensProvider({ scheme: CLUSTER_DOCUMENT_SCHEME, language: 'memql' }, new ClusterDocumentLens()),
  commands.registerCommand('memql.constructs.showDetails', async (key?: { kind?: string; name?: string }) => {
    const dispatcher = connections?.dispatcher;
    if (dispatcher === undefined || key?.kind === undefined || key?.name === undefined) return;
    const result = await new ConstructsClient(dispatcher).listConstructs();
    const found = result.constructs.find((c) => c.kind === key.kind && c.name === key.name);
    if (found === undefined) {
      void window.showInformationMessage(`MemQL: the cluster has no ${key.kind} ${key.name} loaded.`);
      return;
    }
    ConstructPanel.open(context, toCatalogConstruct(found), constructPanelDeps());
  })
);
```

The existing `memql.constructs.open` handler passes `constructPanelDeps()` too. Register `memql.constructs.showDetails` in `package.json` `contributes.commands` (title "MemQL: Show Construct Details") with a `commandPalette` entry `"when": "false"`.

- [ ] **Step 7: The detail page action**

In `constructScreens.ts`: `ConstructPageInput` gains `offerClusterSource: boolean`; in `actionsHtml`, when `input.construct.originPath !== "" && !input.fileInWorkspace && input.offerClusterSource`, push `<button class="secondary" type="button" data-act="viewSourceFromCluster">View source from cluster</button>`. In `constructPanel.ts`: `static open(context, construct, deps: ConstructPanelDeps)` with `export interface ConstructPanelDeps { viewSourceFromCluster: (construct: CatalogConstruct) => Promise<void>; }`; `onMessage` handles `"viewSourceFromCluster"`; `openFile()`'s not-in-workspace branch keeps its sentence but is no longer a dead end because the page now draws the cluster-source button; `render()` passes `offerClusterSource: this.construct.originPath !== "" && this.fileUri === undefined`. Add to the constructScreens test: the button is rendered for a not-in-workspace file, absent for an in-workspace one, and absent for a promoted construct.

- [ ] **Step 8: The badge**

In `readonlyDecorations.ts` `provideFileDecoration`, before the existing path logic:

```ts
if (uri.scheme === CLUSTER_DOCUMENT_SCHEME) {
  return {
    badge: "RO",
    tooltip: `Served from ${decodeURIComponent(uri.authority)} -- read-only. The file is not on this machine; this is the source the cluster loaded.`,
    propagate: false,
  };
}
```

- [ ] **Step 9: Host test**

In `editors/vscode/test-host/index.ts`, add a smoke case:

```ts
smoke("a cluster document opens read-only with no language-server diagnostics", async () => {
  const ext = extension();
  await ext.activate();
  const uri = vscode.Uri.parse("memql-cluster://nowhere/cognition/queries.memql?kind=query&name=x");
  const doc = await vscode.workspace.openTextDocument(uri);
  assert.match(doc.getText(), /Not connected to nowhere/);
  await new Promise((r) => setTimeout(r, 1500));
  assert.equal(vscode.languages.getDiagnostics(uri).length, 0, "the LSP must not diagnose a cluster document");
});
```

If the host does not associate the `memql` language with a non-`file` uri by extension, call `vscode.languages.setTextDocumentLanguage(document, "memql")` inside `openClusterDocument` after opening, and say so in the report.

- [ ] **Step 10: Run the unit suite, the Go rule, and the host lane**

Run: `cd editors/vscode && npm test 2>&1 | tail -6 && cd ../.. && go test ./cmd/memql-lsp/ -run 'VSCode|Vscode' 2>&1 | tail -3 && make vscode-test-host 2>&1 | tail -8`
Expected: unit pass; the import-rule tests `ok`; host lane passes (live cases skip).

- [ ] **Step 11: Commit**

```bash
git add editors/vscode/src/constructs/clusterDocument.ts editors/vscode/src/constructs/clusterDocuments.ts editors/vscode/test/clusterDocument.test.ts cmd/memql-lsp/vscodeimportrule_test.go editors/vscode/src/webview/constructPanel.ts editors/vscode/src/webview/constructScreens.ts editors/vscode/src/constructs/readonlyDecorations.ts editors/vscode/src/extension.ts editors/vscode/package.json editors/vscode/test-host/index.ts
git add <the constructScreens test file>
git commit -m "vscode: cluster documents -- a construct's source served read-only from the cluster" -m "Refs memql#4248"
```

---

### Task 5: The URI handler -- `vscode://znasllc.memql/open` -- closes #4251

**Files:**
- Create: `editors/vscode/src/handoff/openRequest.ts` (pure parse + validate)
- Create: `editors/vscode/src/handoff/resolve.ts` (pure: cluster match, landing decision, workspace candidates)
- Create: `editors/vscode/src/handoff/pending.ts` (pure: store/take with TTL over a `Memento`-shaped interface)
- Create: `editors/vscode/test/handoffOpenRequest.test.ts`, `editors/vscode/test/handoffResolve.test.ts`, `editors/vscode/test/handoffPending.test.ts`
- Modify: `editors/vscode/package.json` (`activationEvents` gains `"onUri"`)
- Modify: `editors/vscode/src/extension.ts` (register the handler in `activate`; the adapter; the pending replay at the end of `registerRuntimeSurface`; `activate` returns `{ handleOpenUri }` for the host lane)
- Modify: `editors/vscode/src/webview/constructPanel.ts` (export `openFileAtSignature(uri, kind, name)` so the handoff and the page share one open path)
- Modify: `editors/vscode/src/install/receipt.ts` (`recordedStackDir` -- see Interfaces)
- Modify: `editors/vscode/test-host/index.ts` (two host cases)
- Modify: `editors/vscode/README.md` (new section "Open from the portal", after "Constructs")

**Interfaces:**
- Consumes: `readClustersFileSafe`, the `memql.clusters.select` command, `composeEndpointFromDomain`, `normalizeDomain`, `ConstructsClient.listConstructs`, `toCatalogConstruct`, `ConstructPanel.open`, `openClusterDocument` (Task 4), `promptForCluster`, `addCluster`, `readReceipt`, `defaultReceiptPath`.
- `recordedStackDir(receipt)`: PR 1 (#4246) adds this reader to `receipt.ts`. If it is not on `main` when this task runs, add it here EXACTLY as below, so the rebase is a no-op:

```ts
/** The directory `install.cloneStack` put the checkout in, or "" when the receipt records none. */
export function recordedStackDir(receipt: Receipt | null): string {
  if (!receipt) return "";
  const entry = entryFor(receipt, "stackCheckout");
  const dest = entry?.result?.dest ?? entry?.params?.dest;
  return typeof dest === "string" ? dest.trim() : "";
}
```

- Produces:

```ts
// openRequest.ts
export const OPEN_REQUEST_VERSION = "1";
export interface OpenRequest { version: "1"; domain: string; kind: string; name: string; }
export type OpenRequestError = { error: string };
export function parseOpenRequest(uri: { path: string; query: string }): OpenRequest | OpenRequestError;
// resolve.ts
export type ClusterMatch = { kind: "none" } | { kind: "one"; cluster: ClusterConfig; alsoMatched: string[] };
export function matchCluster(clusters: readonly ClusterConfig[], domain: string, selected: string): ClusterMatch;
export type Landing =
  | { kind: "notLoaded" }
  | { kind: "detailPage" }
  | { kind: "workspaceFile"; folder: string; relativePath: string }
  | { kind: "openCheckout"; checkout: string; mode: "thisWindow" | "ask" }
  | { kind: "clusterDocument" };
export function landingFor(input: { construct?: { origin: string; originPath: string }; existingIn?: { folder: string; relativePath: string }; clusterLocal: boolean; checkout: string; workspaceFolderCount: number }): Landing;
export function workspaceCandidates(originPath: string): string[];   // ["dsl/<p>", "<p>"]
// pending.ts
export const PENDING_HANDOFF_KEY = "memql.handoff.pending";
export const PENDING_HANDOFF_TTL_MS = 120_000;
export interface PendingHandoff { request: OpenRequest; storedAt: number; }
export function storePending(memento: { update(key: string, value: unknown): Thenable<void> }, request: OpenRequest, now: number): Thenable<void>;
export function takePending(memento: { get<T>(key: string): T | undefined; update(key: string, value: unknown): Thenable<void> }, now: number): Promise<OpenRequest | undefined>;
// extension.ts
export interface MemqlExtensionApi { handleOpenUri(uri: Uri): Promise<HandoffOutcome>; }
export interface HandoffOutcome { outcome: "refused" | "untrusted" | "noCluster" | "notLoaded" | "opened"; detail: string; }
```

Validation rules in `parseOpenRequest`: the path must be exactly `/open`; `v` must equal `"1"`; `cluster`, `kind`, `name` each present exactly once and non-empty after trimming; `kind` matches `/^[a-z][A-Za-z0-9]{0,31}$/`; `name` is at most 512 characters and contains no control character (U+0000-U+001F, U+007F), no `/` or `\`, and no `..`; the whole query is at most 4096 characters; `cluster` goes through `normalizeDomain`, is lower-cased, and must match `/^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/`. Every failure returns `{ error }` naming the field.

`matchCluster`: an entry matches when `normalizeDomain(c.domain ?? "").toLowerCase() === domain` or `c.endpoint.trim().toLowerCase() === composeEndpointFromDomain(domain)`. Zero -> `none`. One or more -> `one`, preferring the entry whose `name === selected`, else the first in file order; the others' names go in `alsoMatched`.

`landingFor`: no construct -> `notLoaded`; `originPath === ""` -> `detailPage`; `existingIn` present -> `workspaceFile`; `clusterLocal && checkout !== ""` -> `openCheckout` with `mode: workspaceFolderCount === 0 ? "thisWindow" : "ask"`; otherwise `clusterDocument`.

- [ ] **Step 1: Write the failing tests**

Create `editors/vscode/test/handoffOpenRequest.test.ts`:

```ts
import test from "node:test";
import assert from "node:assert/strict";

import { parseOpenRequest } from "../src/handoff/openRequest.js";

const ok = (query: string) => parseOpenRequest({ path: "/open", query });

test("the v=1 contract parses", () => {
  assert.deepEqual(ok("v=1&cluster=acme.example.com&kind=concept&name=v1%3Acognition%3Aspace"), {
    version: "1",
    domain: "acme.example.com",
    kind: "concept",
    name: "v1:cognition:space",
  });
});

test("the domain is normalised and case-folded", () => {
  const r = ok("v=1&cluster=.Acme.Example.COM.&kind=query&name=x");
  assert.equal("domain" in r ? r.domain : r.error, "acme.example.com");
});

test("every malformed input is refused by name", () => {
  const cases: Array<[string, RegExp]> = [
    ["v=2&cluster=a.test&kind=query&name=x", /v=2/],
    ["cluster=a.test&kind=query&name=x", /\bv\b/],
    ["v=1&kind=query&name=x", /cluster/],
    ["v=1&cluster=a.test&cluster=b.test&kind=query&name=x", /cluster/],
    ["v=1&cluster=a.test&name=x", /kind/],
    ["v=1&cluster=a.test&kind=Query&name=x", /kind/],
    ["v=1&cluster=a.test&kind=query", /name/],
    ["v=1&cluster=a.test&kind=query&name=..%2Fetc%2Fpasswd", /name/],
    ["v=1&cluster=a.test&kind=query&name=a%00b", /name/],
    ["v=1&cluster=not%20a%20host&kind=query&name=x", /cluster/],
    [`v=1&cluster=a.test&kind=query&name=${"x".repeat(600)}`, /name/],
    [`v=1&cluster=a.test&kind=query&name=x&pad=${"p".repeat(5000)}`, /query/],
  ];
  for (const [query, field] of cases) {
    const r = ok(query);
    assert.ok("error" in r, query);
    assert.match(r.error, field, query);
  }
  const wrongPath = parseOpenRequest({ path: "/run", query: "v=1&cluster=a.test&kind=query&name=x" });
  assert.ok("error" in wrongPath && /\/run/.test(wrongPath.error));
});
```

Create `editors/vscode/test/handoffResolve.test.ts`:

```ts
import test from "node:test";
import assert from "node:assert/strict";

import { landingFor, matchCluster, workspaceCandidates } from "../src/handoff/resolve.js";
import type { ClusterConfig } from "../src/clusters/model.js";

const cluster = (over: Partial<ClusterConfig>): ClusterConfig => ({ name: "x", endpoint: "", ...over });

test("a cluster matches by domain or by the endpoint the domain composes", () => {
  const byDomain = cluster({ name: "lab", domain: "Lab.Example.com" });
  const byEndpoint = cluster({ name: "edge", endpoint: "api.edge.example.com:443" });
  const other = cluster({ name: "other", domain: "other.test" });
  assert.equal(matchCluster([other, byDomain], "lab.example.com", "").kind, "one");
  assert.equal(matchCluster([other, byEndpoint], "edge.example.com", "").kind, "one");
  assert.equal(matchCluster([other], "lab.example.com", "").kind, "none");
});

test("several matches prefer the selected cluster and name the rest", () => {
  const a = cluster({ name: "a", domain: "d.test" });
  const b = cluster({ name: "b", domain: "d.test" });
  const m = matchCluster([a, b], "d.test", "b");
  assert.equal(m.kind, "one");
  if (m.kind === "one") {
    assert.equal(m.cluster.name, "b");
    assert.deepEqual(m.alsoMatched, ["a"]);
  }
});

test("the landing follows the design table", () => {
  const c = { origin: "core", originPath: "cognition/queries.memql" };
  assert.deepEqual(landingFor({ clusterLocal: false, checkout: "", workspaceFolderCount: 1 }), { kind: "notLoaded" });
  assert.deepEqual(
    landingFor({ construct: { origin: "promoted", originPath: "" }, clusterLocal: false, checkout: "", workspaceFolderCount: 1 }),
    { kind: "detailPage" },
  );
  assert.deepEqual(
    landingFor({ construct: c, existingIn: { folder: "/w", relativePath: "dsl/cognition/queries.memql" }, clusterLocal: true, checkout: "/w", workspaceFolderCount: 1 }),
    { kind: "workspaceFile", folder: "/w", relativePath: "dsl/cognition/queries.memql" },
  );
  assert.deepEqual(landingFor({ construct: c, clusterLocal: true, checkout: "/home/me/.memql/src", workspaceFolderCount: 0 }), {
    kind: "openCheckout",
    checkout: "/home/me/.memql/src",
    mode: "thisWindow",
  });
  assert.deepEqual(landingFor({ construct: c, clusterLocal: true, checkout: "/home/me/.memql/src", workspaceFolderCount: 1 }), {
    kind: "openCheckout",
    checkout: "/home/me/.memql/src",
    mode: "ask",
  });
  assert.deepEqual(landingFor({ construct: c, clusterLocal: false, checkout: "", workspaceFolderCount: 1 }), { kind: "clusterDocument" });
  // A local cluster with no recorded checkout still has the cluster to read from.
  assert.deepEqual(landingFor({ construct: c, clusterLocal: true, checkout: "", workspaceFolderCount: 1 }), { kind: "clusterDocument" });
});

test("workspace candidates try the checkout layout first", () => {
  assert.deepEqual(workspaceCandidates("cognition/queries.memql"), ["dsl/cognition/queries.memql", "cognition/queries.memql"]);
  assert.deepEqual(workspaceCandidates("dsl/cognition/queries.memql"), ["dsl/cognition/queries.memql", "cognition/queries.memql"]);
});
```

Create `editors/vscode/test/handoffPending.test.ts`:

```ts
import test from "node:test";
import assert from "node:assert/strict";

import { PENDING_HANDOFF_KEY, PENDING_HANDOFF_TTL_MS, storePending, takePending } from "../src/handoff/pending.js";

function memento() {
  const store = new Map<string, unknown>();
  return {
    get<T>(key: string): T | undefined {
      return store.get(key) as T | undefined;
    },
    update(key: string, value: unknown): Thenable<void> {
      if (value === undefined) store.delete(key);
      else store.set(key, value);
      return Promise.resolve();
    },
    has: (key: string) => store.has(key),
  };
}

const request = { version: "1" as const, domain: "d.test", kind: "query", name: "q" };

test("a pending handoff is taken exactly once", async () => {
  const m = memento();
  await storePending(m, request, 1000);
  assert.deepEqual(await takePending(m, 2000), request);
  assert.equal(await takePending(m, 2001), undefined);
  assert.equal(m.has(PENDING_HANDOFF_KEY), false);
});

test("an expired handoff is dropped, not replayed", async () => {
  const m = memento();
  await storePending(m, request, 1000);
  assert.equal(await takePending(m, 1000 + PENDING_HANDOFF_TTL_MS + 1), undefined);
  assert.equal(m.has(PENDING_HANDOFF_KEY), false);
});

test("garbage in the memento is ignored", async () => {
  const m = memento();
  await m.update(PENDING_HANDOFF_KEY, { nope: true });
  assert.equal(await takePending(m, 5), undefined);
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd editors/vscode && npm test 2>&1 | grep -E "handoff|error TS" | head`
Expected: compile errors -- the modules do not exist.

- [ ] **Step 3: Write the three pure modules**

`editors/vscode/src/handoff/openRequest.ts`:

```ts
// What the portal sends, and what this extension accepts (design 4.1, 4.3).
//
// ONE SHAPE, VERSIONED. The handler refuses any `v` but "1" rather than
// guessing at keys it does not know: a portal that needs a different shape
// bumps the version. Every refusal names the field, because the person
// reading the toast is debugging a link, not this code.
//
// HOSTILE INPUT IS THE NORMAL CASE. Any web page can fire a vscode:// link at
// this handler, so nothing here is trusted: lengths are capped, the path must
// be exactly /open, duplicate keys are refused, and `name` may not carry a
// path separator or a control character. `originPath` never comes from the
// link at all -- the catalog supplies it after the cluster is matched.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { normalizeDomain } from "../connection/endpoint.js";

export const OPEN_REQUEST_VERSION = "1";
const MAX_QUERY = 4096;
const MAX_NAME = 512;
const KIND = /^[a-z][A-Za-z0-9]{0,31}$/;
const HOST = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/;
const CONTROL = /[\u0000-\u001f\u007f]/;

export interface OpenRequest {
  version: "1";
  domain: string;
  kind: string;
  name: string;
}

export type OpenRequestError = { error: string };

export function parseOpenRequest(uri: { path: string; query: string }): OpenRequest | OpenRequestError {
  if (uri.path !== "/open") return { error: `unsupported path ${uri.path}; this extension opens constructs at /open` };
  if (uri.query.length > MAX_QUERY) return { error: "the query is longer than this handler accepts" };
  const params = new URLSearchParams(uri.query);
  const one = (key: string): string | OpenRequestError => {
    const all = params.getAll(key);
    if (all.length === 0) return { error: `missing ${key}` };
    if (all.length > 1) return { error: `${key} appears more than once` };
    const v = all[0]!.trim();
    return v === "" ? { error: `missing ${key}` } : v;
  };
  const v = one("v");
  if (typeof v !== "string") return v;
  if (v !== OPEN_REQUEST_VERSION) return { error: `unsupported link version v=${v}; this extension accepts v=1` };
  const cluster = one("cluster");
  if (typeof cluster !== "string") return cluster;
  const domain = normalizeDomain(cluster).toLowerCase();
  if (!HOST.test(domain)) return { error: `cluster ${JSON.stringify(cluster)} is not a domain` };
  const kind = one("kind");
  if (typeof kind !== "string") return kind;
  if (!KIND.test(kind)) return { error: `kind ${JSON.stringify(kind)} is not a construct kind` };
  const name = one("name");
  if (typeof name !== "string") return name;
  if (name.length > MAX_NAME || CONTROL.test(name) || /[\\/]/.test(name) || name.includes("..")) {
    return { error: "name is not a construct name" };
  }
  return { version: "1", domain, kind, name };
}
```

`editors/vscode/src/handoff/resolve.ts`:

```ts
// Which registered cluster a link names, and where a construct lands (design 4.3-4.4).
//
// A link carries a DOMAIN, and the registry is keyed by NAME -- so matching is
// by the domain the add/edit flow stored, or by the endpoint that domain
// composes for an entry registered before domains were recorded. Several
// entries may name one domain (a developer with two tokens to one cluster);
// the selected one wins and the rest are named, not hidden.
//
// The landing is a pure decision over facts the adapter gathers: which
// workspace folder (if any) holds the file, whether the cluster is local, and
// where its checkout is. It never touches the filesystem itself.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { ClusterConfig } from "../clusters/model.js";
import { composeEndpointFromDomain, normalizeDomain } from "../connection/endpoint.js";

export type ClusterMatch =
  | { kind: "none" }
  | { kind: "one"; cluster: ClusterConfig; alsoMatched: string[] };

export function matchCluster(clusters: readonly ClusterConfig[], domain: string, selected: string): ClusterMatch {
  const wanted = normalizeDomain(domain).toLowerCase();
  const endpoint = composeEndpointFromDomain(wanted);
  const matches = clusters.filter(
    (c) => normalizeDomain(c.domain ?? "").toLowerCase() === wanted || c.endpoint.trim().toLowerCase() === endpoint,
  );
  if (matches.length === 0) return { kind: "none" };
  const chosen = matches.find((c) => c.name === selected) ?? matches[0]!;
  return { kind: "one", cluster: chosen, alsoMatched: matches.filter((c) => c !== chosen).map((c) => c.name) };
}

export type Landing =
  | { kind: "notLoaded" }
  | { kind: "detailPage" }
  | { kind: "workspaceFile"; folder: string; relativePath: string }
  | { kind: "openCheckout"; checkout: string; mode: "thisWindow" | "ask" }
  | { kind: "clusterDocument" };

export function landingFor(input: {
  construct?: { origin: string; originPath: string };
  existingIn?: { folder: string; relativePath: string };
  clusterLocal: boolean;
  checkout: string;
  workspaceFolderCount: number;
}): Landing {
  if (input.construct === undefined) return { kind: "notLoaded" };
  if (input.construct.originPath === "") return { kind: "detailPage" };
  if (input.existingIn !== undefined) {
    return { kind: "workspaceFile", folder: input.existingIn.folder, relativePath: input.existingIn.relativePath };
  }
  if (input.clusterLocal && input.checkout !== "") {
    return { kind: "openCheckout", checkout: input.checkout, mode: input.workspaceFolderCount === 0 ? "thisWindow" : "ask" };
  }
  return { kind: "clusterDocument" };
}

/** Where a catalog path may sit inside a workspace folder: a checkout keeps the tree under dsl/. */
export function workspaceCandidates(originPath: string): string[] {
  const bare = originPath.replace(/\\/g, "/").replace(/^\.\//, "").replace(/^dsl\//, "");
  return [`dsl/${bare}`, bare];
}
```

`editors/vscode/src/handoff/pending.ts`:

```ts
// A handoff that has to survive a window reload (design 4.4).
//
// Opening the checkout folder in this window restarts the extension host, so
// the request is parked in globalState with a short TTL and taken exactly once
// on the next activation. The TTL is what keeps a stale request from opening a
// file an hour later in a window nobody asked it to.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { OpenRequest } from "./openRequest.js";

export const PENDING_HANDOFF_KEY = "memql.handoff.pending";
export const PENDING_HANDOFF_TTL_MS = 120_000;

export interface PendingHandoff {
  request: OpenRequest;
  storedAt: number;
}

interface MementoLike {
  get<T>(key: string): T | undefined;
  update(key: string, value: unknown): Thenable<void>;
}

export function storePending(memento: Pick<MementoLike, "update">, request: OpenRequest, now: number): Thenable<void> {
  const pending: PendingHandoff = { request, storedAt: now };
  return memento.update(PENDING_HANDOFF_KEY, pending);
}

export async function takePending(memento: MementoLike, now: number): Promise<OpenRequest | undefined> {
  const raw = memento.get<unknown>(PENDING_HANDOFF_KEY);
  await memento.update(PENDING_HANDOFF_KEY, undefined);
  if (!isPending(raw)) return undefined;
  if (now - raw.storedAt > PENDING_HANDOFF_TTL_MS || now < raw.storedAt) return undefined;
  return raw.request;
}

function isPending(value: unknown): value is PendingHandoff {
  if (value === null || typeof value !== "object") return false;
  const v = value as { request?: unknown; storedAt?: unknown };
  if (typeof v.storedAt !== "number") return false;
  const r = v.request as { version?: unknown; domain?: unknown; kind?: unknown; name?: unknown } | null | undefined;
  return (
    r !== undefined &&
    r !== null &&
    r.version === "1" &&
    typeof r.domain === "string" &&
    typeof r.kind === "string" &&
    typeof r.name === "string"
  );
}
```

- [ ] **Step 4: Run the pure tests**

Run: `cd editors/vscode && npm test 2>&1 | tail -6`
Expected: pass.

- [ ] **Step 5: The adapter in `extension.ts`**

1. `package.json`: `"activationEvents": ["onLanguage:memql", "onView:memqlClusters", "onUri"]`.
2. In `activate`, after the output channels: `context.subscriptions.push(window.registerUriHandler({ handleUri: (uri) => { void handleOpenUri(uri); } }));` and make `activate` return `{ handleOpenUri }` typed `MemqlExtensionApi`.
3. `async function handleOpenUri(uri: Uri): Promise<HandoffOutcome>`:
   - `parseOpenRequest({ path: uri.path, query: uri.query })`; on error: `window.showErrorMessage(`MemQL: this link cannot be opened -- ${error}.`)`, `noteDiagnostic(connectionOutput, 'Handoff refused', error)`, return `{ outcome: 'refused', detail: error }`.
   - if `connections === undefined`: `window.showWarningMessage('MemQL: trust this workspace to open constructs from the portal.')`, return `untrusted`.
   - `readClustersFileSafe(clustersPath)`; `matchCluster(file.clusters, request.domain, file.selectedCluster)`; on `none`: `const choice = await window.showInformationMessage(`MemQL: no registered cluster for ${request.domain}.`, 'Add cluster...')`; if chosen, `const edited = await promptForCluster({ name: request.domain.split('.')[0] ?? request.domain, domain: request.domain, endpoint: composeEndpointFromDomain(request.domain) })` and `if (edited) await writeCluster(clustersTree, () => addCluster(clustersPath, edited))` -- nothing is written unless the person completes the prompts; return `noCluster`.
   - if `alsoMatched.length > 0`: `window.showInformationMessage(`MemQL: ${request.domain} is registered as ${cluster.name}; also as ${alsoMatched.join(', ')}.`)` (non-modal).
   - if the connection is not `connected` to `cluster.name`: `await commands.executeCommand('memql.clusters.select', { cluster, selected: false })` (persists the selection and connects, with the existing sign-in prompts), then wait up to 30 s for `connections.state.status === 'connected' && state.clusterName === cluster.name` (resolve on `onDidChangeState`; a state of `error` resolves early); on timeout or error return `noCluster` with the state's message in a toast.
   - `const catalog = (await new ConstructsClient(dispatcher).listConstructs()).constructs`; `found = catalog.find((c) => c.kind === request.kind && c.name === request.name)`.
   - Facts: `existingIn` = the first `{ folder, relativePath }` for which `workspace.fs.stat(Uri.joinPath(folder.uri, candidate))` succeeds, over `workspace.workspaceFolders` x `workspaceCandidates(found.originPath)`; `clusterLocal = cluster.local === true`; `checkout = recordedStackDir(await readReceipt(defaultReceiptPath()))`; `workspaceFolderCount = workspace.workspaceFolders?.length ?? 0`.
   - `landingFor(...)` and perform: `notLoaded` -> `window.showInformationMessage(`MemQL: ${cluster.name} has no ${request.kind} ${request.name} loaded.`)`; `detailPage` -> `ConstructPanel.open(context, toCatalogConstruct(found), constructPanelDeps())`; `workspaceFile` -> `openFileAtSignature(Uri.joinPath(folderUri, relativePath), found.kind, found.name)` (the helper exported from `constructPanel.ts`, which `openFile()` now calls too); `clusterDocument` -> `openClusterDocument({ cluster: cluster.name, originPath: found.originPath, kind: found.kind, name: found.name })`; `openCheckout` -> `await storePending(context.globalState, request, Date.now())`, then `mode === 'thisWindow'` ? `commands.executeCommand('vscode.openFolder', Uri.file(checkout), { forceNewWindow: false })` : `const pick = await window.showInformationMessage(`Open the local checkout (${checkout}) to edit this construct?`, { modal: true }, 'Open in new window', 'Add to this workspace')` -> new window: `commands.executeCommand('vscode.openFolder', Uri.file(checkout), { forceNewWindow: true })`; add: `workspace.updateWorkspaceFolders(workspace.workspaceFolders?.length ?? 0, 0, { uri: Uri.file(checkout) })`; cancelled: `await takePending(context.globalState, Date.now())` to discard.
   - Every outcome logs one line: `noteDiagnostic(connectionOutput, 'Handoff from portal', `${cluster.name} ${request.kind} ${request.name} -> ${landing.kind}`)`, and returns `{ outcome: 'opened' | 'notLoaded', detail: landing.kind }`.
4. At the end of `registerRuntimeSurface`: `void takePending(context.globalState, Date.now()).then((req) => { if (req !== undefined) void handleOpenUri(Uri.parse(`vscode://znasllc.memql/open?v=1&cluster=${encodeURIComponent(req.domain)}&kind=${encodeURIComponent(req.kind)}&name=${encodeURIComponent(req.name)}`)); });`

- [ ] **Step 6: Host tests**

In `test-host/index.ts` (the runner points `HOME` at a temp dir, so `clusters.yaml` is the temp one):

```ts
smoke("a portal link for an unregistered cluster is refused without side effects", async () => {
  const ext = extension();
  const api = (await ext.activate()) as { handleOpenUri(uri: vscode.Uri): Promise<{ outcome: string }> };
  const file = path.join(os.homedir(), ".memql", "clusters.yaml");
  const before = await fs.readFile(file, "utf8").catch(() => "");
  const result = await api.handleOpenUri(vscode.Uri.parse("vscode://znasllc.memql/open?v=1&cluster=nowhere.test&kind=query&name=x"));
  assert.equal(result.outcome, "noCluster");
  const after = await fs.readFile(file, "utf8").catch(() => "");
  assert.equal(after, before, "a link must not write clusters.yaml");
});

smoke("a malformed portal link is refused by name", async () => {
  const ext = extension();
  const api = (await ext.activate()) as { handleOpenUri(uri: vscode.Uri): Promise<{ outcome: string; detail: string }> };
  const result = await api.handleOpenUri(vscode.Uri.parse("vscode://znasllc.memql/open?v=9&cluster=a.test&kind=query&name=x"));
  assert.equal(result.outcome, "refused");
  assert.match(result.detail, /v=9/);
});
```

The information message in the first case must not block: use the non-modal `showInformationMessage` (it returns when dismissed or ignored); in the host lane nothing clicks "Add cluster...", so the promise resolves undefined and the handler returns `noCluster`.

- [ ] **Step 7: README**

Add a section `## Open from the portal` after `## Constructs` in `editors/vscode/README.md`:

```markdown
## Open from the portal

The portal's concept page has **Open definition in VS Code**. It is a link of
one shape:

    vscode://znasllc.memql/open?v=1&cluster=<domain>&kind=<kind>&name=<registry key>

and this extension handles it in four steps: match `cluster` against the
domains in `clusters.yaml`, select and connect that cluster (the same sign-in
you would get from the tree), find the construct in its catalog, and open it
where it is -- the file in your workspace, revealed at its signature; the local
cluster's checkout, if you have none of it open; a read-only document served
from the cluster, when the file is not on this machine; or the construct's
detail page, when it was promoted and has no file.

**A link may select a registered cluster, connect it, and open a document. It
may never add a cluster, sign in silently, run anything, or write settings.**
An unregistered domain gets an offer to add it through the ordinary prompts;
a malformed link is refused and the refusal names the field. VS Code's own
"allow this extension to open the URI" prompt is the consent gate; there is no
second one.
```

- [ ] **Step 8: Run everything**

Run: `cd editors/vscode && npm test 2>&1 | tail -6 && cd ../.. && make vscode-test-host 2>&1 | tail -8 && go test ./cmd/memql-lsp/ -run 'VSCode|Vscode|Extension' 2>&1 | tail -3`
Expected: all green.

- [ ] **Step 9: Commit**

```bash
git add editors/vscode/src/handoff/openRequest.ts editors/vscode/src/handoff/resolve.ts editors/vscode/src/handoff/pending.ts editors/vscode/test/handoffOpenRequest.test.ts editors/vscode/test/handoffResolve.test.ts editors/vscode/test/handoffPending.test.ts editors/vscode/package.json editors/vscode/src/extension.ts editors/vscode/src/webview/constructPanel.ts editors/vscode/src/install/receipt.ts editors/vscode/test-host/index.ts editors/vscode/README.md
git commit -m "vscode: handle vscode://znasllc.memql/open -- the portal's handoff lands on the construct" -m "Refs memql#4251"
```

---

### Task 6: "Browse rows in portal" from a concept's detail page -- closes #4252

**Files:**
- Modify: `editors/vscode/src/clusters/portalUrl.ts` (add `encodePortalSegment`, `portalConceptUrl`)
- Test: the file that covers `portalTarget` (`grep -l portalTarget editors/vscode/test/*.ts`); create `editors/vscode/test/portalConceptUrl.test.ts` if none
- Modify: `editors/vscode/src/webview/constructScreens.ts` (the button, concepts only)
- Modify: `editors/vscode/src/webview/constructPanel.ts` (`ConstructPanelDeps.browseRowsInPortal`; message `browseRows`)
- Modify: `editors/vscode/src/extension.ts` (factor `portalUrlForCluster(cluster)` out of the `memql.clusters.openPortal` body; supply the dep)

**Interfaces:**
- Produces: `export function encodePortalSegment(value: string): string` (mirrors the portal's `encodeSegment`: `encodeURIComponent(value).replace(/%3A/g, ":")`) and `export function portalConceptUrl(root: string, conceptId: string): string` (`${root-with-trailing-slash}concepts/${encodePortalSegment(conceptId)}`).

- [ ] **Step 1: Write the failing tests**

```ts
import test from "node:test";
import assert from "node:assert/strict";

import { encodePortalSegment, portalConceptUrl } from "../src/clusters/portalUrl.js";

// Pinned to the portal's own fixtures (clients/portal/test/conceptUrls.test.tsx):
// colons stay literal so an id reads as an id in the address bar.
test("a concept id keeps its colons and encodes everything else", () => {
  assert.equal(encodePortalSegment("v1:cognition:space"), "v1:cognition:space");
  assert.equal(encodePortalSegment("v1:a b:c/d"), "v1:a%20b:c%2Fd");
});

test("the concept url hangs off the portal root", () => {
  assert.equal(portalConceptUrl("https://portal.acme.test/", "v1:cognition:space"), "https://portal.acme.test/concepts/v1:cognition:space");
  assert.equal(portalConceptUrl("https://portal.acme.test", "v1:cognition:space"), "https://portal.acme.test/concepts/v1:cognition:space");
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd editors/vscode && npm test 2>&1 | grep -E "portal|error TS" | head`
Expected: compile error -- the exports do not exist.

- [ ] **Step 3: Implement**

In `portalUrl.ts`:

```ts
/** The portal's path-segment encoding, mirrored: colons literal, the rest percent-encoded. */
export function encodePortalSegment(value: string): string {
  return encodeURIComponent(value).replace(/%3A/g, ":");
}

/** The portal's concept-rows page for one concept, under the root `portalTarget` found. */
export function portalConceptUrl(root: string, conceptId: string): string {
  const base = root.endsWith("/") ? root : `${root}/`;
  return `${base}concepts/${encodePortalSegment(conceptId)}`;
}
```

In `constructScreens.ts` `actionsHtml`: when `input.construct.kind === "concept"`, push `<button class="secondary" type="button" data-act="browseRows">Browse rows in portal</button>`. In `constructPanel.ts`: `ConstructPanelDeps` gains `browseRowsInPortal: (construct: CatalogConstruct) => Promise<void>`; `onMessage` routes `"browseRows"`. In `extension.ts`: extract the URL computation from `memql.clusters.openPortal` into `async function portalUrlForCluster(cluster: ClusterConfig): Promise<string>` (site row when connected, composed otherwise; `""` when neither) and use it in both places; the dep resolves the connected cluster's `ClusterConfig` from `readClustersFileSafe(clustersPath)` by `connections.state.clusterName`, then `env.openExternal(Uri.parse(portalConceptUrl(root, construct.name)))`, or the existing "no portal address" error when the root is empty.

Add to the constructScreens test: the button appears for `kind: "concept"` and for no other kind.

- [ ] **Step 4: Run the suite**

Run: `cd editors/vscode && npm test 2>&1 | tail -6`
Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add editors/vscode/src/clusters/portalUrl.ts editors/vscode/src/webview/constructScreens.ts editors/vscode/src/webview/constructPanel.ts editors/vscode/src/extension.ts
git add <the portalUrl test file> <the constructScreens test file>
git commit -m "vscode: browse a concept's rows in the portal from its detail page" -m "Refs memql#4252"
```

---

## Self-review (done while writing)

- Spec coverage: 4.1 (Task 3), 4.2 (Tasks 2-3), 4.3-4.4 (Task 5), 4.5 (Task 4), 4.6 (Task 5 rules + host tests), 4.7 (Task 6), 2.8 (Task 5 resolves by `kind` + `name`), 2.9 (Task 2). Section 8's README "Open from the portal" lands in Task 5; the consolidation is PR 3.
- Type consistency: `PackClient.readFile` (Task 1) is what Task 4 calls; `ClusterDocumentRef` / `openClusterDocument` (Task 4) are what Task 5 calls; `ConstructPanelDeps` is defined in Task 4 and extended in Task 6; `openFileAtSignature` is exported in Task 5 and used by the page and the handoff; `recordedStackDir` is spelled exactly as PR 1 spells it.
- Nothing here crosses a node boundary.

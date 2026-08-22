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
  // Explicit tuple type: a plain `string[][]` literal loses per-pair arity
  // under noUncheckedIndexedAccess, so destructuring `[k, v]` would widen `v`
  // to `string | undefined`.
  const pairs: Array<[string, string]> = [
    ["v", "1"],
    ["cluster", input.domain],
    ["kind", input.kind],
    ["name", input.name],
  ];
  const query = pairs.map(([k, v]) => `${k}=${encodeURIComponent(v)}`).join("&");
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

# MemQL browser editor

The default OS file handoff opens `https://vscode.<domain>/editor/`. This is
Code-OSS in the browser, assembled with the pinned MIT-licensed
`@codingame/monaco-vscode-api` packages. Both MemQL extensions are bundled from
this checkout. It has no shared server process, terminal, or server filesystem.
Desktop VS Code and Cursor remain choices in Files settings.

The host calls Productivity Tools' activation check before opening a file.
Productivity depends on the core extension and consumes its versioned connection
API. A missing, disabled, incompatible, failed or stalled extension leaves an
explanation, Retry, and Continue without MemQL tools. Basic mode edits local
files without opening the requested MemQL resource; the banner says connected
files and versioned saves are unavailable. `?mode=basic` starts without the two
extensions, including on a fresh profile.

The `resource` URL parameter contains only a `memql-file:` reference belonging
to this installation. No file content or credential travels in a handoff URL.
The core extension signs in through the existing device grant and owns cluster
selection and SecretStorage. Browser credentials are persisted in origin-local
IndexedDB, encrypted with a non-extractable WebCrypto key; webviews have a
separate origin. This is browser storage, not an operating-system keychain.
Clearing browser site data removes the editor's sign-in.

Saving is the existing Productivity file provider: it checks the loaded revision
and creates a new MemQL version. A concurrent edit refuses the save until the
user compares and resolves the conflict. A local file save remains local.

## Build and verify

```
node editors/browser/build.mjs
npm --prefix editors/browser run typecheck
npm --prefix editors/browser test
```

The build installs workspace dependencies, compiles both browser extension entry
points, copies their assets and licenses, and builds the static editor. The same
command runs in the edge image's SPA stage and the required extension CI lane.
Use `--skip-deps` only when dependencies are already installed.

Local deployment uses `make dev`. The editor is nested in the existing VS Code
site bundle, so it follows the same image, ArgoCD, TLS and ingress path as OS.
No Microsoft Marketplace download or per-browser installation is needed.

## Browser isolation

The site seed declares `extensionRuntimePath: "/editor/"`. Other sites and the
landing page retain their existing policies. The runtime subtree admits
WebAssembly and workers. Only its stable `assets/extension-host.html` document
allows the evaluation required to load CommonJS browser extensions.

Custom editor webviews use `<site>--assets--<opaque-id>.<domain>` sibling origins,
under the installation's existing wildcard ingress and certificate. The edge
resolves each through the original site's declaration on every replica. They
serve only GET/HEAD of the public runtime assets; identity routes, API proxies,
preview grants, runtime configuration and the application document are refused.
They receive neither app credentials nor SecretStorage. Upstream webview CSP and
the extension's content CSP govern each sandboxed document.

The editor owns its unsaved-document lifecycle. The generic site auto-refresh
script is not injected into it or its frames. Reload to take an editor update;
VS Code's normal unsaved-change handling remains in charge.

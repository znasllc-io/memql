# MemQL for VS Code

Language support for MemQL (`.memql`) files, powered by the offline
`memql-lsp` language server (which embeds the same MemQL Sense brain the
Cockpit uses). Works fully offline against local files -- no cluster, no auth.

## Features

- Syntax highlighting (TextMate grammar, generated from the DSL spec) refined
  by semantic tokens from the server.
- Live diagnostics (errors and warnings) as you type, including cross-reference
  resolution: unknown import modules/ids and signature concepts that resolve to
  nothing.
- Context-aware completion (constructs, concepts, functions, annotations, ...),
  including segment-aware `use`-line completion (namespaces -> kinds -> ids) and
  kind-filtered in-body invocation completion.
- Hover documentation and signature help.

## Runtime panel

An activity-bar panel connects the extension to a running cluster: pick a
cluster from `~/.memql/clusters.yaml` (the same file the memQL Cockpit
uses), browse every registered concept grouped by domain, and inspect rows
-- paged, with live detail -- without leaving the editor. It requires a
trusted workspace, since it reads credentials and opens a network
connection. See [VS Code Runtime Panel](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel.md).

## Install / update locally

One command builds the extension (building a fresh `memql-lsp`) and
(re)installs it into VS Code:

```bash
make vscode-install          # from the repo root
```

Then reload the editor to pick up the new server: run **Developer: Reload
Window** (Cmd/Ctrl+Shift+P), or restart VS Code. Because the language
intelligence lives in the bundled `memql-lsp` binary, this is the loop to run
every time you change the server or the extension -- `--force` overwrites the
installed build, so re-running it just updates in place.

Options (via `scripts/vscode/install.sh`, or `EDITOR_CMD=` on the make target):

```bash
make vscode-install EDITOR_CMD=cursor        # install into Cursor / code-insiders / codium
bash scripts/vscode/install.sh --no-build    # reinstall the last-built .vsix (skip the rebuild)
bash scripts/vscode/install.sh --help
```

If the editor CLI is missing, run "Shell Command: Install 'code' command in
PATH" from the VS Code command palette.

## Requirements

The `memql-lsp` binary. The extension resolves it in this order:

1. The `memql.lsp.serverPath` setting, if set.
2. A bundled platform binary at `bin/<platform>-<arch>/memql-lsp` (added at
   packaging time -- `make vscode-install` / `make vscode-package` bundle it).
3. `memql-lsp` on your `PATH`.

Build it from the memql repo with `go build -o bin/memql-lsp ./cmd/memql-lsp`.

## Development

```bash
make vscode-deps                 # from the repo root -- see below
cd editors/vscode && npm ci && npm run compile
```

`make vscode-deps` is not optional on a clean checkout. The extension consumes
`sdk/ts` and `sdk/ts-viewkit` as `file:` dependencies, and their `main` /
`types` point into `dist/` -- which does not exist until those packages are
built. Skipping it leaves the symlinks resolving to nothing and `tsc -p ./`
fails.

Press `F5` to launch an Extension Development Host, set `memql.lsp.serverPath`
to your built binary, and open a folder of `.memql` files.

### Testing

```bash
make vscode-test        # unit lane -- bare node --test, seconds, no Electron
make vscode-test-host   # host smoke lane -- downloads and drives a real VS Code
```

The two lanes answer different questions. `vscode-test` covers the modules that
do not import `vscode`; it is fast and dependency-light and must stay that way.
`vscode-test-host` (`editors/vscode/test-host/`) launches a real Extension
Development Host to assert what a unit test structurally cannot reach -- that
activation survives the host's runtime, that every command the manifest
contributes is actually registered, that the activity-bar contributions were
accepted, that a file watcher fires for a path outside the workspace, and that
each webview opens. It needs a display, and falls back to `xvfb-run` when
`DISPLAY` is unset. CI runs it against both the declared `engines.vscode` floor
and current stable, because that floor is where this bug class actually fires.

Neither lane dials a cluster. Everything downstream of a connection is verified
by hand against the [manual verification
checklist](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel-verification.md).

## Settings

- `memql.lsp.serverPath` -- absolute path to the `memql-lsp` binary.
- `memql.lsp.trace.server` -- `off` | `messages` | `verbose`.

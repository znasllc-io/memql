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
cd editors/vscode
npm install
npm run compile
```

Press `F5` to launch an Extension Development Host, set `memql.lsp.serverPath`
to your built binary, and open a folder of `.memql` files.

## Settings

- `memql.lsp.serverPath` -- absolute path to the `memql-lsp` binary.
- `memql.lsp.trace.server` -- `off` | `messages` | `verbose`.

# MemQL for VS Code

Language support for MemQL (`.memql`) files, powered by the offline
`memql-lsp` language server (which embeds the same MemQL Sense brain the
Cockpit uses). Works fully offline against local files -- no cluster, no auth.

## Features

- Syntax highlighting (TextMate grammar, generated from the DSL spec) refined
  by semantic tokens from the server.
- Live diagnostics (errors and warnings) as you type.
- Context-aware completion (constructs, concepts, functions, annotations, ...).
- Hover documentation and signature help.

## Requirements

The `memql-lsp` binary. The extension resolves it in this order:

1. The `memql.lsp.serverPath` setting, if set.
2. A bundled platform binary at `bin/<platform>-<arch>/memql-lsp` (added at
   packaging time -- see WP7).
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

---
title: MemQL in VS Code (offline language server)
audience: public
status: stable
area: language
sinceVersion: 0.13.0
owner: znas
---

# MemQL in VS Code

The MemQL VS Code extension gives `.memql` authors first-class editing --
syntax highlighting, live diagnostics, context-aware completion, hover, and
signature help -- powered by the **same MemQL Sense brain** the Cockpit
[Editor](../cockpit/editor.md) uses. It works **fully offline against local
files**: no running cluster, no auth. Open a folder of `.memql` files and
iterate on the syntax itself.

Sense stays the single language-intelligence component serving both the Cockpit
and VS Code (see [Sense & the DSL Spec](./sense.md)). This extension adds a new
*delivery mechanism* -- an offline language server -- on top of the existing
Sense package; it forks no brain and changes no wire contract.

## Architecture

```
  VS Code  (editors/vscode, TypeScript)
    - vscode-languageclient  -- spawns -->  memql-lsp (stdio)
    - memql.tmLanguage.json  (baseline offline highlighting, generated)
    - language-configuration.json (comments / brackets / brace
      expansion + indentation via onEnterRules + indentationRules --
      no longer reliant on the built-in bracket heuristic, and firing
      even when the `{` carries trailing text; VS Code gates
      onEnterRules at editor.autoIndent "advanced"+ and
      indentationRules at "full", the default)
                   |  LSP over stdio (JSON-RPC)
                   v
  cmd/memql-lsp  (Go binary)
    - LSP front end (tliron/glsp)
    - workspace loader: BuildOfflineSense(os.DirFS(root)) -- DB-free
    - per request: buffer text -> sense.{Tokenize,Diagnose,Complete,Hover,SignatureHelp}
                   |  in-process Go calls (no gRPC, no network)
                   v
  component/memql/sense   (the UNCHANGED brain)
    projects vocabulary from component/language/dslspec + .../annotations
```

- **`cmd/memql-lsp`** speaks LSP over stdio via `tliron/glsp`. At `initialize`
  it builds an offline Sense service from the workspace with
  `memql.BuildOfflineSense` -- the DB-free construction the boot-time
  validation tier already uses (no database, no network). It advertises
  incremental text sync, push diagnostics, semantic tokens, completion, hover,
  and signature help.
- **Positions.** Sense is 1-based (line and column-in-runes); LSP is 0-based
  (line and character-in-UTF-16-code-units). The server converts on every
  request (`cmd/memql-lsp/internal/position`); the two diverge only on
  supplementary-plane runes (surrogate pairs).
- **Registry refresh.** On save / watched-file change the server rebuilds
  Sense (debounced) and atomically swaps it, so a concept or shape added in one
  file becomes visible to completion/hover in the others.
- **The extension** (`editors/vscode`) is a thin `vscode-languageclient`. It
  resolves the server binary in order: the `memql.lsp.serverPath` **user**
  setting, then a bundled `bin/<platform>-<arch>/memql-lsp`, then `memql-lsp`
  on `PATH`. `serverPath` is machine/user-scoped only, so an untrusted
  workspace cannot redirect it.

## Baseline grammar (generated)

`editors/vscode/syntaxes/memql.tmLanguage.json` colors a file the instant it
opens -- before the server attaches, and in diffs / on GitHub. It is
**generated from `dslspec`** (the same source of truth Sense projects from),
never hand-written, so it cannot drift from the language:

```bash
make vscode-grammar   # memql-lsp gen-grammar editors/vscode/syntaxes/memql.tmLanguage.json
```

A drift test fails the build if the checked-in grammar falls behind `dslspec`
or the `GrammarVersion`. Regenerate on every `GrammarVersion` bump. Semantic
tokens from the server then refine the baseline coloring.

## Setup and development

```bash
# Build the server binary.
make memql-lsp                       # -> bin/memql-lsp

# Develop the extension.
cd editors/vscode
npm install
npm run compile
# Press F5 (Extension Development Host), set memql.lsp.serverPath to the built
# binary, and open a folder of .memql files.
```

## Packaging

```bash
make vscode-package   # build the darwin-arm64 binary, compile the client, vsce package -> .vsix
```

The offline LSP embeds the engine, so the binary is bundled per platform;
darwin-arm64 (standardized dev hardware) is built first. The `vscode-extension`
CI lane guards the grammar against drift and runs this packaging flow. Release
targets the VS Code Marketplace and OpenVSX; version the extension in lockstep
with `GrammarVersion`.

Nothing bundled is tracked in git: `editors/vscode/bin/` and `*.vsix` are
ignored (editors/vscode/.gitignore), so the platform binary is cross-built
fresh at package time. A stale extension on a developer machine is therefore
a locally-installed VSIX, not a stale artifact in the repository -- reinstall
from a fresh `make vscode-package` to pick up server changes.

### 0.2.0 -- the editor-intelligence epic (#2600)

Packaged against `GrammarVersion` `2026.07-null-coalescing-operator`, this
release carries the epic end to end:

- `@` offers only the enclosing construct's legal annotations, and body
  completion is scoped to that construct's blocks and verbs (#2626, #2627).
- Member completion after a dot: `actor.`, `event.`, `args.`, `payload.`
  (#2624), driven by the canonical actor envelope (#2623).
- Block-specific completion inside `args` / `filter` / `insert` / `update`
  / `shape`, teaching the post-grammar-epoch short forms (#2628).
- Real snippet completions -- body blocks and construct skeletons (#2629).
- Edit-time errors mirroring the engine's load rules: `actor-undeclared`
  (#2622) and `actor-unknown-property` (#2625).

### 0.3.0 -- the cross-reference resolution epic (#2728)

This release teaches Sense to RESOLVE cross-references, not just complete them,
backed by a workspace symbol graph built from the `dslimports` tree (#2729):

- Import diagnostics: `use fylo.concept.{ oder }` now flags the wrong kind
  segment and the undeclared id (#2730), and a `mutate/query/shape/seed
  <Concept>` bound to a concept that exists nowhere is an error (#2731) --
  both conservative, so a legitimate external/global reference never squiggles.
- Segment-aware `use`-line completion: typing `fylo.` offers the module kinds,
  and the brace list offers that module's importable ids, instead of dumping
  the whole symbol table (#2732).
- Kind-filtered invocation completion: in a behavioral body, `query <name>`
  offers only queries, `logic <name>` only logic, etc. (#2733).
- A syntax&lt;-&gt;Sense parity gate keeps these tables in step with the grammar
  (#2734).

**Updating:** the intelligence lives in the bundled `memql-lsp` binary, so a new
release ships only when you rebuild it. Run `make vscode-package`, reinstall the
`.vsix` (`code --install-extension editors/vscode/memql-0.3.0.vsix --force`), and
reload the window -- a stale editor is a locally-installed VSIX, never a repo
artifact.

## Snippet completions

The extension advertises `snippetSupport` (vscode-languageclient
default), and the server emits real snippets (#2629): body-block
snippets that open a block with the cursor inside, and construct
skeletons with tabstops at the names you fill. Items that are not
snippets declare `InsertTextFormat=PlainText` explicitly, so nothing
inserts tabstop syntax literally.

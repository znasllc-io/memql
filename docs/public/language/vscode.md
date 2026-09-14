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
signature help -- powered by the **same MemQL Sense brain** the engine serves
over gRPC. It works **fully offline against local
files**: no running cluster, no auth. Open a folder of `.memql` files and
iterate on the syntax itself.

> The extension also ships a runtime panel -- cluster selection and a
> generic concept browser against a live cluster. See
> [VS Code Runtime Panel](vscode-runtime-panel.md).

> Connected to a cluster, the extension additionally reports each construct's
> **training state** -- whether the cluster knows it, knows an older version of
> it, or has never heard of it -- and offers dry-run, try-in-session, promote
> and demote. That surface, and the seeded-versus-trained model underneath it,
> is [Training Constructs Into a Running Cluster](training.md).

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
      indentationRules at "full", the default; generated)
    - package.json "memql" block (the edition and grammar it was
      built from, compared with the cluster's at connect)
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
  signature help, go-to-definition, and code actions: the
  [rewrite to edition 2026](sense.md#the-rewrite-quick-fix), as a quick fix
  and as `source.fixAll`.
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
  workspace cannot redirect it -- and a workspace-scoped value is refused
  *out loud*, with a warning naming the setting, rather than silently ignored.
  When no binary resolves at all, only the language features are lost: the
  runtime surface (Clusters / Concepts / Runs) is registered independently and
  keeps working.

## Baseline grammar and language configuration (generated)

`editors/vscode/syntaxes/memql.tmLanguage.json` colors a file the instant it
opens -- before the server attaches, and in diffs / on GitHub. It is
**generated from `dslspec`** (the same source of truth Sense projects from),
never hand-written, so it cannot drift from the language. Semantic tokens from
the server then refine the baseline coloring.

`editors/vscode/language-configuration.json` -- comment toggling, bracket
matching, auto-closing and surrounding pairs, and the Enter and re-indent
rules -- is generated beside it (memql#5362). The comment tokens, the bracket
pairs and the string quote come from `dslspec`'s punctuation table
(`component/language/dslspec/punctuation.go`, pinned against the lexer); the
shape of the Enter and indentation patterns is structured data in
`cmd/memql-lsp/internal/grammar/languageconfig.go`, and a pair added to the
table reaches every pattern on the next regeneration. One command writes both
files:

```bash
make vscode-grammar   # memql-lsp gen-grammar ... and memql-lsp gen-language-config ...
```

Two staleness tests fail the build when either committed file differs from
what the generator produces -- because the tables moved, or because the file
was edited by hand, which the next regeneration would undo. Regenerate on every
`GrammarVersion` bump.

## Which MemQL the extension speaks, and which the cluster does

The extension ships its own language server, so its completion and
diagnostics are the grammar it was packaged with, whatever grammar the cluster
it connects to was built with. Three pieces keep the two from drifting apart
silently (D25 of the language-freeze program, memql#5362):

- **The extension declares what it was built from.** `editors/vscode/package.json`
  carries a top-level `"memql": {"edition": "2026", "grammarVersion": "..."}`,
  and `parser.EditorRelease` (`component/language/parser/edition.go`) names the
  first extension release that carries the current `GrammarVersion`.
  `cmd/memql-lsp/editorparity_test.go` fails when the pin differs from
  `parser.GrammarVersion` or `parser.Edition`, when `parser.EditorRelease` is
  newer than the extension's `version`, or when `CHANGELOG.md` has no
  `## <EditorRelease>` section naming the grammar and the edition. Its messages
  name each edit, and one about a moved grammar or edition opens with
  `Has <EditorRelease> been published?`, because the answer decides the fix.
- **The cluster states its language on connect.** `ServerHello` carries
  `edition`, `grammar_version` and `editor_release`, and the extension compares
  them with its own pin on every connect. A cluster on a newer grammar raises a
  warning naming the release to install, with **Open in Extensions**; a cluster
  on an older grammar raises a notice that completion may offer forms it
  refuses until the cluster is updated; two grammars that cannot be ordered
  raise a notice naming both and pointing at the release built for the
  cluster's grammar. Grammar versions are labels, never ordered: the only
  orders the extension states are one edition against another and
  `editor_release` against its own version. A cluster that predates the fields
  reports nothing, and nothing is shown. Each cluster is mentioned once per
  grammar per session, and the details are in the **MemQL Connection** output
  channel.
- **Any other client asks the same question.** The `DslSpec` export over the
  stream (spec version `1.1.0`) carries `edition` and `grammarVersion`.

**When the grammar moves**, the change that moves it carries the extension's
side too, and whether `parser.EditorRelease` has been published decides how:

- **Not published** -- that release carries the new grammar: set
  `memql.grammarVersion`, name the new grammar in its `## <EditorRelease>`
  section of `CHANGELOG.md`, and run `make vscode-grammar`.
- **Published** -- a published release cannot change, so the grammar needs a
  new one: set `memql.grammarVersion`, raise the extension's `version` and
  `parser.EditorRelease` to the new release, give it its own changelog section
  naming the grammar and the edition, and run `make vscode-grammar`.

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

## Install / update locally

The language intelligence lives in the bundled `memql-lsp` binary, so refreshing
your editor after a server or extension change is a build + reinstall + reload
loop. One command does the first two:

```bash
make vscode-install                  # build a fresh .vsix and (re)install it into VS Code
make vscode-install EDITOR_CMD=cursor # ... into Cursor / code-insiders / codium
```

Then run **Developer: Reload Window** so the new server process spawns.
`scripts/vscode/install.sh` wraps `package.sh` (the build) + `code
--install-extension --force` (the reinstall, which overwrites the currently
installed build); `--no-build` reinstalls the last-built `.vsix`, `--help` lists
the flags. Re-run it whenever you change the server or extension -- there is no
marketplace auto-update for a locally-built extension, so a stale editor is
always a locally-installed VSIX, never a repo artifact.

## Packaging

```bash
make vscode-package   # build the darwin-arm64 binary, compile the client, vsce package -> .vsix
```

The offline LSP embeds the engine, so the binary is bundled per platform;
darwin-arm64 (standardized dev hardware) is built first. The `vscode-extension`
CI lane runs every drift guard under `cmd/memql-lsp` -- the grammar against
`dslspec`, `language-configuration.json`, and `package.json`'s `engines.vscode`
/ `engines.node` floors -- then runs this packaging flow. It gates on the
`vscode` bucket, and because `go-checks` gates on the `go` bucket instead, an
`editors/vscode`-only change (the shape every dependabot bump to the extension
takes) skips `go-checks` entirely and this lane is the only place those guards
fire (memql#2792). Release targets the VS Code Marketplace and OpenVSX. The
extension's version tracks the GRAMMAR, not the engine release: a grammar
change ships in an extension release that carries it, and
`cmd/memql-lsp/editorparity_test.go` refuses the change otherwise (see above).

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

### 0.4.0 -- editor parity (#5362)

The first release that declares the language it was built from: edition 2026,
and the `GrammarVersion` its section of `editors/vscode/CHANGELOG.md` names
(the parity test keeps that line, the `memql` pin and the parser in step).

- At connect, the cluster's edition and grammar are compared with the
  extension's own, and a cluster on a newer grammar names the release to
  install (see above).
- The language configuration is generated from `dslspec`, like the grammar,
  and gated the same way.
- Writing edition 2026 (epic memql#5363): completion and hover by expression
  position, every retired spelling reported as the parse error it is, and the
  [rewrite to edition 2026](sense.md#the-rewrite-quick-fix) as a quick fix and
  on save. The 0.4.0 section of `editors/vscode/CHANGELOG.md` lists them.

**Updating:** the intelligence lives in the bundled `memql-lsp` binary, so a new
release ships only when you rebuild it. Run `make vscode-package`, reinstall the
`.vsix` (`code --install-extension editors/vscode/memql-0.4.0.vsix --force`), and
reload the window -- a stale editor is a locally-installed VSIX, never a repo
artifact.

## Snippet completions

The extension advertises `snippetSupport` (vscode-languageclient
default), and the server emits real snippets (#2629): body-block
snippets that open a block with the cursor inside, and construct
skeletons with tabstops at the names you fill. Items that are not
snippets declare `InsertTextFormat=PlainText` explicitly, so nothing
inserts tabstop syntax literally.

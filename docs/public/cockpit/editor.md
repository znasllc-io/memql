---
title: Cockpit Editor
audience: public
status: stable
area: cockpit
sinceVersion: 0.9.0
owner: znas
---

# Cockpit Editor

The **Editor** is a Cockpit tab (`F3`) with two surfaces. By default
it **browses** a connected node's DSL **packs** read-only: a domain
tree on the left, the `.memql` / `.tmpl` files inside the selected
domain in the middle, and the selected file's
[Sense](../language/sense.md)-colored source on the right. It is the
fastest way to read the exact DSL a running node carries -- core
embedded constructs and any plugin-registered packs alike -- with
syntax highlighting and hover.

`Ctrl+B` toggles into **authoring mode**, where you create and edit a
local `.memql` **bundle** with the same live IntelliSense and then
[**Validate**](#validate-the-gate-1-sandbox) and
[**Inject**](#inject-session-define) it against the running engine. See
[Authoring a bundle](#authoring-a-bundle) below.

> Browsing packs is **read-only** -- embedded and plugin-registered
> pack files cannot be edited in place. Authoring edits a separate
> local bundle, never a pack; injecting a bundle is **session-scoped**
> and never mutates the durable schema.

The Cockpit ships from its own repo,
`github.com/znasllc-io/memql-cockpit`; this page documents the engine
contract behind the Editor and the panel's as-built behaviour.

---

## pack vs bundle

Two terms that look similar and are not the same:

- A **pack** is a compile-time `.memql` subtree embedded in, or
  `RegisterTree`'d into, the engine. Packs are part of the binary (or a
  Go module compiled into it). The Editor browses packs, and they are
  **read-only** there -- a pack subtree is owned by its registering
  module and the embedded tree is immutable.
- A **bundle** is a runtime-authored set of constructs validated and
  injected through the authoring API. Bundles are not packs; they are
  not part of the binary. In the Cockpit a bundle is a local directory
  of `.memql` files under `~/.memql/bundles/<name>/`, authored in
  [authoring mode](#authoring-a-bundle); editing what you see in the
  read-only browser would mean authoring a bundle instead. Validating
  or injecting a bundle never edits a pack and never touches the
  binary's embedded tree.

---

## Browsing packs

The Editor's three panes mirror the read-only pack-browser engine API
(`dsl/pack_browse.go`), reached through the `sdk/go/pack` SDK:

| Pane | Shows | Engine / SDK call |
|---|---|---|
| **DOMAINS** | Every browsable DSL namespace on the node, with an origin label and file count | `ListDomains` |
| **FILES** | The `.memql` / `.tmpl` files under the selected domain, by relative path | `ListFiles` |
| **SOURCE** | The selected file's source, Sense-colored | `ReadFile` |

Each domain carries an **origin label** so you can tell core from
contributed DSL:

- `embedded` -- a core baked-in domain (this binary's `//go:embed`
  tree).
- `pack:<name>` -- a domain contributed at `init()` time via
  `RegisterTree` by an external Go module (a pack).
- `disk:<path>` -- a single file served from an on-disk override under
  `MEMQL_DSL_PATH` instead of the embedded copy. (This label appears on
  a read result, since the overlay is resolved per file.)

The browser is read-only by construction: there is no write path in the
pack API. Domain names and relative paths are validated and cleaned so
a read cannot escape its domain root.

Every wire call goes through the SDK (`pack.Client` / `sense.Client`) --
no raw DSL, no protobuf -- per the Cockpit's SDK-only rule. The pack
browser binds to the active cluster's dispatcher, so switching the
selected cluster transparently retargets the Editor at that node's
packs. The Editor is gated on a connected, selected cluster; with no
cluster connected the panes show a placeholder.

---

## The Sense-colored viewer and hover

The SOURCE pane colors the file with [MemQL Sense](../language/sense.md)
tokens (via the Sense SDK's `Tokenize`) -- keywords, identifiers,
strings, annotations, and concept ids each get their own style.
Coloring is best-effort: if no Sense client is available for the active
cluster, or a tokenize call fails, the source still renders, just
uncolored.

When the SOURCE pane is focused it carries a read-only **hover cursor**.
Move the cursor to a symbol and request **Sense hover** to see its info
-- function docs, a concept schema, an annotation's documentation -- in
a small overlay. Hover runs `Sense.Hover` against the exact source text
shown.

### Key bindings

These are the as-built bindings from the Editor panel (`cli/dsledit/`):

| Key | Action |
|---|---|
| `Tab` | Cycle focus: DOMAINS → FILES → SOURCE → DOMAINS |
| `Enter` | DOMAINS: load the selected domain's files. FILES: load the selected file's source |
| `↑` / `↓` | DOMAINS / FILES: move the selection. SOURCE: move the hover cursor line |
| `←` / `→` | SOURCE: move the hover cursor column |
| `H` | SOURCE: request Sense hover at the cursor (shows the overlay) |
| `Esc` | SOURCE: close the hover overlay |
| `PgUp` / `PgDn` / `Home` / `End` | SOURCE: scroll the viewport |

---

## Authoring a bundle

`Ctrl+B` from the read-only browser switches the Editor into
**authoring mode** (and `Ctrl+B` again switches back). Authoring mode
lets you write a local `.memql` **bundle** with the same Sense
IntelliSense the browser uses for coloring, then validate and inject it
against the connected node. Nothing you author touches a pack or the
binary's embedded tree -- a bundle lives on your machine until you
explicitly inject it, and an inject is session-scoped.

Authoring mode is a connected-cluster surface for Validate / Inject
(those drive the engine over gRPC), but the local bundle editing -- the
panes, the on-disk files -- works whether or not a cluster is selected.

### The three panes

Authoring mode mirrors the browser's three-pane shape, but the panes
are local-bundle-oriented:

| Pane | Shows |
|---|---|
| **BUNDLES** | Your local bundle directories under `~/.memql/bundles/`. The open bundle is marked with `*`. |
| **FILES** | The `.memql` / `.tmpl` files in the selected bundle. The open file is marked with `*`. |
| **EDITOR** | The editable buffer for the open file -- Sense-colored, with inline diagnostics, completion, and hover. A `*` next to the file name in the pane title means unsaved changes. |

A bundle is just a directory; a file is created with a `.memql`
extension when you don't supply one (so typing `queries` yields
`queries.memql`). Bundle and file names must be a single safe path
segment -- no separators, no traversal, no leading dot.

### Live IntelliSense

The EDITOR pane drives the same [MemQL Sense](../language/sense.md)
service as the read-only viewer, through the Sense SDK
(`sdk/go/sense`):

- **Coloring + diagnostics** refresh automatically as you type
  (debounced), so syntax highlighting and inline error / warning
  underlines track the buffer. An initial pass runs when you open a
  file, so coloring appears before you type.
- **Completion** (`Ctrl+Space`) requests context-aware completions at
  the cursor and opens a popup anchored at the cursor cell. The popup
  **live-filters** to the identifier prefix under the cursor as you
  keep typing, and closes when the word ends. `↑` / `↓` move the
  selection; `Enter` or `Tab` accepts (replacing the typed prefix);
  `Esc` dismisses.
- **Hover** (`Ctrl+K`) requests Sense hover at the cursor -- function
  docs, a concept schema, an annotation's documentation -- in an
  overlay anchored at the cursor; `Esc` closes it.

Because Sense is driven from the same `dslspec` source of truth that
backs the load-time grammar, the completions and diagnostics you see
while authoring are exactly what the engine will accept -- see
[MemQL Sense & the DSL Spec](../language/sense.md).

### Validate (the Gate-1 sandbox)

`Ctrl+G` **validates** the open bundle. The Cockpit concatenates every
`.memql` / `.tmpl` file in the bundle into one source payload (a bundle
is validated as a whole, since constructs can reference each other
across files), saves any unsaved buffer first, and calls `ValidateBundle`
over the authoring SDK (`sdk/go/authoring`).

Validate runs the engine's **Gate-1 isolated compile-and-bind
sandbox**: it slices the source into `(kind, name, source)` constructs
and compiles + binds each in ISOLATION against a read-only clone of the
live concept registry (bundle-declared concepts are overlaid onto the
clone first, so a construct that references a concept the same bundle
defines resolves). It **never mutates engine state and registers
nothing** -- safe to run against a running node. The result lands in a
per-construct overlay (`Esc` closes it): each construct shows `[ok]`,
`[!!]` with the compile/bind error, or `[--] skipped` for a kind the
sandbox does not yet compile (a skip does not fail the bundle). A
bundle with no recognizable constructs comes back as not-OK with a
single explaining line.

### Inject (session-define)

`Ctrl+R` **injects** the open bundle: it validates through the same
Gate-1 sandbox and, on success, registers the bundle's constructs into
**your stream's owner-scoped authored registry** via
`SessionDefineBundle`. Function-family constructs (`query` / `mutation`
/ `logic`, plus `spec` / `trait`) become **callable by name for the
lifetime of this session**.

Inject is deliberately narrow:

- **Session-scoped / ephemeral.** The authored registry lives for the
  stream and is **dropped when the session ends**. Injecting changes
  nothing durable -- restart the Cockpit (or disconnect) and the
  injected constructs are gone.
- **Never shadows core.** Resolution is core-first: an injected
  construct whose name a sealed core construct already owns is dropped,
  so authoring can only *add* owner-private capability, never redefine
  platform behaviour.
- **Owner-scoped.** Constructs are keyed to your `userId`; one
  session can never resolve another's injected constructs.

A bundle that fails validation registers nothing and the overlay shows
the rejection plus the failing diagnostics.

Both Validate and Inject require the **owner or developer** role (the
engine's `auth.CanAuthor` gate) -- validation reveals the live concept
surface, and a session-define changes what is callable on the stream.
Either action works from any pane focus (the `Ctrl` combos never
collide with editor typing).

### Durable promotion is not yet available

Inject is the only path the Cockpit wires today, and it is
session-scoped by design. Promoting a validated bundle to a
**durable**, cluster-wide construct -- owner-gated, persisted, and
surviving restarts -- is a separate **Phase-2** action tracked in
**memql-cockpit#232**. It is still being built; there is no
durable-promote control in the Editor yet. Until it lands, treat inject
as a way to try a construct out within your own session, not to ship
one.

### The engine contract behind the actions

Both actions go through the Go authoring SDK, which wraps the engine's
authoring machinery (`component/memql/authoring_session.go` +
`authoring_sandbox.go`). The same contract is callable directly:

```go
import "github.com/znasllc-io/memql/sdk/go/authoring"

client := authoring.NewClient(dispatcher)

// Validate only -- Gate-1 sandbox, never mutates engine state.
vr, err := client.ValidateBundle(ctx, bundleSource)
// vr.OK, vr.Diagnostics[].{Name, Kind, OK, Skipped, Error}

// Validate + session-inject (callable by name for the stream's life).
sr, err := client.SessionDefineBundle(ctx, bundleSource)
// sr.OK, sr.Defined[].{Kind, Name}, sr.Diagnostics, sr.Error
```

A wire-level failure (dispatcher closed, context cancelled, permission
denied) surfaces as a Go error; a bundle that simply fails to compile
comes back as `OK=false` with diagnostics, not as an error.

### Authoring key bindings

These are the as-built bindings for authoring mode
(`cli/dsledit/author.go`):

| Key | Action |
|---|---|
| `Ctrl+B` | Toggle back to the read-only pack browser |
| `Tab` | Cycle focus: BUNDLES → FILES → EDITOR → BUNDLES |
| `N` | BUNDLES: new bundle. FILES: new file (prompts for a name) |
| `Enter` | BUNDLES: open the bundle's files. FILES: open the file in the editor |
| `Ctrl+S` | EDITOR: save the buffer to disk |
| `Ctrl+Space` | EDITOR: Sense completion at the cursor |
| `Ctrl+K` | EDITOR: Sense hover at the cursor |
| `Ctrl+G` | Validate the bundle (Gate-1 sandbox; no engine mutation) |
| `Ctrl+R` | Inject the bundle (session-define; session-scoped, never shadows core) |
| `Esc` | Dismiss the Validate / Inject overlay, a completion / hover popup, or a name prompt |

---

## Where this lives

| Piece | Location |
|---|---|
| Pack-browse engine API | `dsl/pack_browse.go` |
| Pack-browse SDK | `sdk/go/pack/` |
| Authoring engine contract | `component/memql/authoring_session.go`, `authoring_sandbox.go` |
| Authoring gRPC handlers | `component/grpc/authoring_handlers.go` |
| Authoring SDK | `sdk/go/authoring/` |
| Editor panel + authoring mode (Cockpit, separate repo) | `cli/dsledit/` (`view.go` browser, `author.go` authoring + Validate/Inject, `bundle.go` local workspace IO) in `memql-cockpit` |

## See also

- [MemQL Sense & the DSL Spec](../language/sense.md) -- the language
  intelligence that colors the SOURCE pane and drives authoring-mode
  completion, hover, and diagnostics.
- [Building a Pack](../build/building-a-pack.md) -- how a compile-time
  pack is authored and registered (the durable, embedded counterpart
  to a runtime bundle).
- [MemQL Language](../language/memql.md) -- the DSL reference.
</content>

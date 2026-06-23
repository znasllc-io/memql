---
title: Cockpit Editor
audience: public
status: stable
area: cockpit
sinceVersion: 0.9.0
owner: znas
---

# Cockpit Editor

The **Editor** is a Cockpit tab that browses a connected node's DSL
**packs** read-only: a domain tree on the left, the `.memql` / `.tmpl`
files inside the selected domain in the middle, and the selected file's
[Sense](../language/sense.md)-colored source on the right. It is the
fastest way to read the exact DSL a running node carries -- core
embedded constructs and any plugin-registered packs alike -- with
syntax highlighting and hover.

> The Editor is **read-only**. Browsing and viewing embedded / pack
> files is shipped today. In-Editor authoring (writing a runtime
> bundle, validating it, and injecting it) is a separate, not-yet-
> shipped surface -- see [Authoring](#authoring-coming-in-230--231)
> below.

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
- A **bundle** is a runtime-authored set of constructs
  (`v1:authoring:bundle`) validated and injected through the authoring
  API. Bundles are not packs; they are not part of the binary. Editing
  what you see in the Editor would mean authoring a bundle -- the
  [Authoring](#authoring-coming-in-230--231) path, which is not yet
  wired into the Cockpit.

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

## Authoring (coming in #230 / #231)

Editing a pack file in place is **not** how runtime authoring works,
and the in-Editor authoring UI is **not yet available in the Cockpit**.
Authoring a bundle (writing constructs, validating them, injecting
them) is tracked as:

- **C2 (memql-cockpit#230)** -- the in-Editor bundle authoring mode.
- **C3 (memql-cockpit#231)** -- wiring the Validate / Inject actions to
  the engine.

Until those land, the Editor is browse-and-read only. The underlying
**engine contract** those actions will call is already shipped and
callable today via the Go SDK (`sdk/go/authoring`), so this section
documents that contract -- not a Cockpit UI.

### The validate / inject engine contract

Two operations over the engine's gRPC stream, both reusing the
authoring machinery in `component/memql/authoring_session.go` +
`authoring_sandbox.go`:

- **ValidateBundle** runs the Gate-1 isolated compile-and-bind sandbox
  over a `.memql` bundle and returns per-construct diagnostics plus an
  overall `OK`. It slices the source into `(kind, name, source)`
  constructs and compiles + binds each in ISOLATION against a read-only
  clone of the live concept registry. It NEVER mutates engine state and
  registers nothing -- safe to call against a running engine. A bundle
  with no recognizable constructs comes back as `OK=false` with a
  single explaining diagnostic, so the caller always gets a typed
  answer rather than a bare error.

- **SessionDefineBundle** (the SDK name for the engine's
  `AuthorSessionBundle`) validates, then registers the bundle's
  constructs into the caller's **owner-scoped, stream-scoped** authored
  registry, NON-DURABLY. Function-family constructs (`query` /
  `mutation` / `logic`) become callable by name for the lifetime of the
  stream, never shadowing core, and are dropped when the stream
  (session) ends. A bundle that fails validation registers nothing and
  returns the diagnostics with `OK=false`.

Owner-gated **durable** activation / promotion of a bundle is out of
scope for this surface (tracked separately, #232); the contract above
is validate plus non-durable session inject only.

From the SDK:

```go
import "github.com/znasllc-io/memql/sdk/go/authoring"

client := authoring.NewClient(dispatcher)

// Validate only -- never mutates engine state.
vr, err := client.ValidateBundle(ctx, bundleSource)
// vr.OK, vr.Diagnostics[].{Name, Kind, OK, Skipped, Error}

// Validate + session-inject (callable by name for the stream's life).
sr, err := client.SessionDefineBundle(ctx, bundleSource)
// sr.OK, sr.Defined[].{Kind, Name}, sr.Diagnostics, sr.Error
```

A wire-level failure (dispatcher closed, context cancelled, permission
denied) surfaces as a Go error; a bundle that simply fails to compile
comes back as `OK=false` with diagnostics, not as an error.

---

## Where this lives

| Piece | Location |
|---|---|
| Pack-browse engine API | `dsl/pack_browse.go` |
| Pack-browse SDK | `sdk/go/pack/` |
| Authoring engine contract | `component/memql/authoring_session.go`, `authoring_sandbox.go` |
| Authoring SDK | `sdk/go/authoring/` |
| Editor panel (Cockpit, separate repo) | `cli/dsledit/` in `memql-cockpit` |

## See also

- [MemQL Sense & the DSL Spec](../language/sense.md) -- the language
  intelligence that colors the SOURCE pane.
- [Building a Pack](../build/building-a-pack.md) -- how a compile-time
  pack is authored and registered.
- [MemQL Language](../language/memql.md) -- the DSL reference.
</content>

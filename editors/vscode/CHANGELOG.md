# Changelog

The Marketplace renders this file on the extension's page, so it is written for
someone deciding whether to install rather than for someone reading the repo.

## 0.5.0

**The MemQL it speaks**
- This release speaks MemQL edition 2026, grammar
  `2026.09-dsl-v1-loop-protection-930e046b`: its highlighting, completion and
  diagnostics are that grammar's. A cluster on this grammar names this release
  to an older editor that connects to it.

**Writing automations**
- Automations can declare `@loop` and `@mode`; completion and diagnostics know
  both. Completion offers each annotation and its keys, and hovering a key
  explains it.
- `@loop(maxDepth=4, until=row => row.status == "done")` marks an automation
  that triggers itself on purpose, such as one that moves a row forward a step
  at a time. `until` names the state that ends the cycle, and `maxDepth` bounds
  how many times the automation may run in one chain. The editor underlines an
  `until` that is not a lambda over the row and a `maxDepth` that is not a
  whole number.
- `@mode` says what happens when an automation fires while it is already
  running: `single` refuses the new fire, `queued` makes it wait its turn,
  `restart` cancels the run in flight, and `parallel` runs both. `max` bounds
  how many may wait or run at once, and the editor underlines a `max` below 1.
- Completing an annotation key that takes no value, such as a `@mode` or
  `@rowAuthz`'s `clusterOwner`, now inserts the key alone. It used to add an
  `=` that the engine refuses.

## 0.4.0

**The MemQL it speaks, and the cluster's**
- This release speaks MemQL edition 2026, grammar
  `2026.09-dsl-v1-expressions-52d9aeb1`: its
  highlighting, completion and diagnostics are that grammar's.
- When you connect to a cluster, the extension compares the cluster's MemQL
  with its own. A cluster on a newer grammar raises a notice naming the release
  of this extension to install, with a button that opens it in the Extensions
  view. A cluster on an older grammar raises a notice that completion may offer
  forms the cluster refuses until the cluster is updated. Each cluster is
  mentioned once per grammar in a session, and the details are in the MemQL
  Connection output channel.
- A cluster whose engine predates the comparison does not report its MemQL, and
  the extension says nothing about it.

**Writing MemQL**
- **The language line.** Each domain declares the language it is written in, in
  a `memql.toml` beside its `.memql` files. A domain without one is flagged on
  its files with a quick fix that writes it. Until then the workspace does not
  load: hover is off, and completion offers keywords, annotations and snippets
  but no loaded concepts, fields or functions. A notification says why.
- **The parse-time refusals.** Annotations are checked as you type against one
  registry of where each may be written and with what arguments. A misplaced,
  mis-argued or retired annotation is an error that names the fix.
- Closing a file now clears its diagnostics, because the language server
  analyzes open files only.

**Writing edition 2026**
- Filters, specs, traits and trigger filters are written as a lambda over a
  named parameter: `filter row => row.status == "open" && isActiveRecord(row)`.
  In a filter, a refine clause or a trigger filter, completion offers the
  `row =>` header and nothing else until it is written. After `row.`, it offers
  the fields of the concept the query, spec or trigger is bound to, each with
  its declared type, then the fields every row carries, such as `id` and
  `createdAt`.
- Completion offers only what the cursor's position accepts. A filter or a
  condition is not offered query or mutation calls, the middle of an
  expression is not offered statement keywords, and a list field of the row in
  a filter is offered only the methods the database can run.
- Hovering a function, an operator, or a spec or trait applied in an
  expression shows its signature, whether it runs in the database or in the
  engine at that position, and the spellings it replaced: `x.count()` replaces
  `len(x)` and `count(x)`.
- The spellings edition 2026 retires, such as `cond(...)`, `when(...)` or a
  filter without its `row =>`, no longer load: the engine refuses a file that
  uses one. The editor underlines each as an error, and the message names the
  replacement and the command that rewrites a whole tree,
  `memqlmigrate --rewrite=expressions`. Hovering one shows your own construct
  rewritten: on `filter status == args.owner`, the hover shows
  `filter row => row.status == args.owner`.
- An underlined spelling offers a quick fix, **Rewrite to edition 2026**, which
  rewrites that construct the way `memqlmigrate` would and changes only the
  lines it has to. **Rewrite the file to edition 2026** does every construct at
  once, and runs on save if `source.fixAll` is in your
  `editor.codeActionsOnSave`. A construct the rewrite cannot convert, such as a
  relationship traversal, keeps its underline and offers no fix.

## 0.3.1

First published release. The extension has been built and installed from source
throughout its development; this is the first version available from a registry.

**MemQL language support**
- Syntax highlighting, diagnostics, completion, hover and signature help for
  `.memql` files, from a language server bundled with the extension. It needs
  no cluster and no network.

**Local clusters**
- Create, repair, update and uninstall a local MemQL cluster from the
  Deployments panel, with a checklist before each run that says what will
  happen and a record of what did.
- Build node images from your own checkout and roll the cluster onto them.

**Connected clusters**
- Browse the constructs a cluster has loaded, run them, and read the results.

### Platforms

Published for `linux-x64`, `linux-arm64`, `darwin-arm64` and `darwin-x64`.

Language support works on all four. Creating a **local** cluster additionally
needs `linux/amd64` or `darwin/arm64` -- on the other two the installer says so
before it changes anything, and connecting to an existing cluster is unaffected.

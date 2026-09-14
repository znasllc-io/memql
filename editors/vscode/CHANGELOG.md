# Changelog

The Marketplace renders this file on the extension's page, so it is written for
someone deciding whether to install rather than for someone reading the repo.

## 0.4.0

**The MemQL it speaks, and the cluster's**
- This release speaks MemQL edition 2026, grammar
  `2026.09-dsl-v1-expressions-b80c0409`: its
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

**Writing edition 2026**
- Filters, specs, traits and trigger filters are written as a lambda over a
  named parameter: `filter row => row.status == "open" && isActiveRecord(row)`.
  After `row.`, completion offers the fields of the concept the query, spec or
  trigger is bound to, each with its declared type, then the fields every row
  carries, such as `id` and `createdAt`.
- Completion offers only what the cursor's position accepts. A filter or a
  condition is not offered query or mutation calls, the middle of an
  expression is not offered statement keywords, and a list field of the row in
  a filter is offered only the methods the database can run.
- Hovering a function, an operator, or a spec or trait applied in an
  expression shows its signature, whether it runs in the database or in the
  engine at that position, and the spellings it replaced: `x.count()` replaces
  `len(x)` and `count(x)`.
- Retired spellings are underlined, and the message names the replacement and
  the command that rewrites a whole tree, `memqlmigrate --rewrite=expressions`.
  Hovering one shows your own construct rewritten: on
  `filter status == args.owner`, the hover shows
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

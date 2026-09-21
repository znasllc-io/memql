# Changelog

## 0.6.0

MemQL edition 2026 is frozen, and this release is the editor that speaks it.

- **The language is version one.** Edition 2026 / language 1.0 is frozen:
  grammar `2026.09-before-write-error-accessor-77cda60c`, unchanged from 0.5.1.
  What the freeze means for you is that a form this extension highlights and
  completes is a form clusters will go on accepting -- a public form now leaves
  only through a deprecation window, warning with its replacement for at least
  two minor releases before it stops loading.
- **The cluster can now hand a model its own grammar.** Clusters on this
  release serve a generated BNF and a vocabulary of every construct,
  annotation, builtin and function over `memqlGrammar()` and
  `memqlVocabulary()`. Both are generated from the same tables this extension's
  highlighting and completion are generated from, so what a cluster tells a
  model it accepts and what the editor offers you cannot drift apart.
- **And you can now read both without leaving the editor.** **MemQL: Show
  Language Reference** opens the connected cluster's grammar and vocabulary in
  one tab: a single search filters both, and a copy action puts either whole
  artifact on the clipboard for handing to a model. It names the edition it is
  showing, whether that edition is frozen, and the grammar version -- and with
  no cluster connected it says so and shows the edition and grammar version
  this extension itself was built against, which is the language its own
  completion and diagnostics speak. It is the one MemQL command that works in a
  folder you have not trusted, because it reads no credential and opens no
  connection.
- **No change to the language features.** Highlighting, completion,
  diagnostics, hover and signature help are as they were in 0.5.1; the language
  server, TextMate grammar and language configuration regenerate byte-identical
  against the frozen edition.

## 0.5.1

- Support edition 2026 before-write triggers and `row.<field> = <expression>` statements. Retire the no-argument `error()` accessor while retaining `error("message")`. Grammar: `2026.09-before-write-error-accessor-77cda60c`.


The Marketplace renders this file on the extension's page, so it is written for
someone deciding whether to install rather than for someone reading the repo.

## 0.5.0

**The MemQL it speaks**
- This release speaks MemQL edition 2026, grammar
  `2026.09-dsl-v1-loop-protection-cf139af9`: its highlighting, completion and
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
  `2026.09-retire-error-accessor-78765bad`: its
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
- The retired `error()` accessor is refused with a replacement hint.
  `error("message")` remains the catalog function and needs no builtin import.
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

**Writing a logic or an automation**
- A logic's and an automation's body is a list of statements, run in the order
  they are written: `x := query activeUsers(status: "active")`, a bare call,
  `if` / `else if` / `else`, `for x in xs if <cond> { ... }`, `switch`,
  `parallel { branch a { ... } }`, `publish "<topic>" { ... }` in an automation,
  and `return`. A call carries its kind -- `query`, `mutation`, `logic`,
  `builtin`, `automation` or `action` -- and names every argument, and
  `retry(n)`, `on error continue` and `on surface(...)` close one. A statement's
  value is read by its name: `rows.first().email`.
- Completion in a body offers the statements its position accepts, the kinds a
  call names, and the names bound above the cursor.
- The forms statements replace no longer load: a `step` block, a logic's
  `body { }`, the one-line `automation x @trigger(...) => logic y`,
  `steps.<id>.result`, `forEach`, `publishEvent(...)`, `@schedule(...)` and
  `partition=` on a trigger. The editor underlines each as an error naming the
  replacement and the command that rewrites a whole tree,
  `memqlmigrate --rewrite=bodies`.
- `step("x")`, `input()`, `item()` and `index()` no longer load either: a
  statement is read by its name, an argument as `args.<name>`, and a loop's
  element by the name the loop gives it.
- A mutation is declared with the word a call spells:
  `mutation <Concept> <name> { ... }`. `mutate` no longer loads.

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

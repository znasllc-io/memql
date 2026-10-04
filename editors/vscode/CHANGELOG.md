# Changelog

## 0.6.2

A new interface for everything around your `.memql` files: clusters, signing
in, installing and uninstalling a local cluster, deployments and the authoring
pages. Nothing it could do before is gone. The language is the same as 0.6.1.

- **Every page is rebuilt to say less.** The cluster page, the Add a cluster
  page, the deployments pages and the construct, concept, run and language
  reference pages share one layout: a title, the facts, and a bar at the bottom
  with the state in words and only the actions that work from it. Commands,
  menus and notifications are in plain words, with titles such as **MemQL:
  Install Local Cluster...**, **MemQL: Open MemQL OS** and **MemQL: Show
  Cluster Details**, and every notification is one short sentence with its fix
  as a button.
- **Reconnecting to a cluster works again.** Two things stopped it. The editor
  signed in with the client another MemQL tool had saved for the same cluster,
  which the cluster refused for the editor's sign-in: the browser showed "Bad
  Request" and the editor waited ten minutes. And it refused a cluster address
  written as `https://`, which is how MemQL Cockpit saves the local cluster, so
  after signing in it still could not connect to a cluster that was answering.
  The editor now always signs in and refreshes as itself, and connects to an
  `https://` address.
- **Sign in is one click.** From the cluster's row, its page or the
  notification, with no dialog in front of the browser. When you are already
  signed in to MemQL OS in that browser, it finishes without typing. A cluster
  that refuses the editor says so in seconds, the browser page says "You're
  signed in" only once the sign-in has really finished, and after 30 seconds
  **Use a code instead** is offered without giving up on the browser.
- **The connection comes back on its own.** The cluster you use reconnects when
  a window opens and after the connection drops. If it is still away after
  about two minutes, a notification says so, with **Reconnect**.
- **Long operations share one progress screen.** Install, repair, uninstall,
  update, rebuild and deploy show a bar that only moves forward, the step
  running now, the step count and the time so far, with the live log one click
  away. A failed step says what went wrong and gives its fix as a command to
  **Run in terminal**. **Cancel** stops after the current step, and **Resume**
  carries on.
- **Choices are switches.** Every on/off choice on a page is a switch, in place
  of a checkbox. On the uninstall page the shared tools start off, and deleting
  a cluster's data takes its own red switch and a typed phrase before the
  button appears.
- **A cluster made with `make up` can be uninstalled.** Uninstall was offered
  for a local cluster with no install record and then refused it. It now lists
  that cluster as kept, and removes it only when you turn on **Delete the
  cluster's data** and type `delete memql data`. Dismissing the password prompt
  now cancels an install or uninstall instead of starting it.
- **Deployments and authoring have pages of their own.** The Deployments page
  shows the cluster's version, its history and the next step: Update, Change
  version, Rebuild from checkout and Pull and rebuild for a local cluster;
  Deploy, Promote, Abort and Roll back for a remote one. A construct's lens
  names its state in words (Not on cluster, Live, Staged) and opens the actions
  that state allows.
- **Guided install is gone.** It showed each step's command for you to run, and
  in practice ran the same steps again as Retry does. A step that needs your
  hand now gives its command with **Run in terminal**.
- **MemQL opens in VS Code for the Web.** In a browser it connects to clusters
  you already have, with the sign-in approved by a code; installing a cluster
  and the language server stay on the desktop. On every host the editor keeps
  its sign-in in VS Code's secret storage rather than in the cluster list it
  shares with MemQL Cockpit, and MemQL Productivity Tools uses this connection
  instead of a sign-in of its own.

## 0.6.1

- **Tools say which agent may call them, and which person.** Edition 2026
  gains `@requiresAgentRole("assistant", ...)` on a tool -- the acting agent's
  role -- and `@requiresRank("<role>")` becomes legal on a tool, where it
  judges the person the call is for. Completion and hover offer both. Grammar
  `2026.09-dsl-v1-followups-9f344ecf`; the language is still edition 2026.
- **`@allowedRoles(...)` is deprecated.** It still loads, and the editor now
  warns on every use naming the two annotations it splits into; the quick fix
  writes `@requiresAgentRole(...)` for a list of agent roles and
  `@requiresRank(...)` for a list of person roles that forms a floor, and
  offers nothing for a list it cannot carry across exactly.
- **The editor now shows what the load refuses.** A field the concept does not
  declare, a context spec applied to the row, a read that needs `.?`, an
  expression over its cost budget: until now these appeared only when the tree
  loaded (memqllint, boot). The language server now runs the engine's load over
  the file you have open -- the text in the buffer, placed where the file sits
  in the tree, so a name two domains share means what the load says it means --
  and underlines the refused expression with the load's own sentence and its
  rule id (`lower_unknown_field`) as the code. It runs after the syntax
  diagnostics have appeared, never before them, and one fault draws one
  squiggle. A problem found by a run or a training action now carries the same
  code.
- **A workspace that relates to a storefront pack builds.** A product domain
  importing a pack's concept (`use wholesale.concepts.{ application }`) used to
  fail the language server's build, which left every file of the workspace
  without hover or registry-backed completion. The server now links the packs a
  cluster links.
- **A query clause written twice is an error.** A second `filter` or `sort`
  line in a query used to replace the first without a word, so a query could
  lose its ownership filter and still look right. The editor now underlines the
  second copy (`query_clause_duplicate`) and says to merge the two.
- **The Deployments panel's buttons send what they name.** Rollout promote /
  abort could never run: it sent `promote` with no rollout, which the SDK
  refused before sending, and it offered no abort. Each rollout in flight is
  now a row under Rollouts with its own Promote and Abort, with Abort
  confirmed against the rollout's name. Preparing the next version, which
  always took a patch, is one choice per bump naming the version it prepares
  (`Prepare 1.5.0`, minor). Roll back, which targeted the newest succeeded
  deployment and so usually the one running, returns to the release before
  the one the cluster runs, and says which before you press.

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

---
title: Your first MemQL program
audience: public
status: stable
area: language
sinceVersion: 0.20.0
owner: znas
---

# Your first MemQL program

Build a personal reading list with four constructs: a concept, mutation, query,
and tool. You can validate the whole file offline, then run it on a cluster
where your account has authoring permission. No model call is needed.

## The complete file

Save this as `reading.memql` in its own workspace folder, or open the checked-in
[example](../../../examples/reading-list/reading.memql). Use a separate folder
from the engine's core DSL to keep your tutorial definitions distinct.

<!-- corpus: 2026/examples/first-program/reading-list.memql -->
```memql
/// A reading-list item owned by the signed-in person.
@rowAuthz(owner="ownerUserId")
@displayCard(primary="title", status="finished")
concept readingItem {
  ownerUserId  string!
  title        string!
  finished     bool
}

/// Add an item. Ownership comes from the authenticated actor.
@actor
mutation readingItem addReadingItem {
  args {
    itemId  string!
    title   string!
  }
  insert {
    accept { title }
    stamp {
      id: args.itemId
      ownerUserId: actor.userId
      finished: false
    }
  }
}

/// Read your list, optionally narrowed to finished or unfinished items.
@actor
query readingItem readingItems {
  args {
    finished  bool
  }
  filter row => row.ownerUserId == actor.userId && (args.finished == nil || row.finished == args.finished)
  sort "row.createdAt", "desc"
  paginate 50
}

/// Give the same read operation a tool interface.
@handler(type="query", query="query readingItems(finished: args.finished)")
@executionTime("fast")
tool listReadingItems {
  finished  boolean @description("True for finished; false for unfinished. Omit for both.")
}
```

- **Concept:** the fields every reading item can hold. `!` makes a field required.
  The engine supplies intrinsic fields such as `id` and `createdAt`; do not
  redeclare them.
- **Mutation:** `accept` copies the listed argument; `stamp` supplies the id,
  authenticated owner, and initial completion state. The caller cannot choose
  someone else's owner id.
- **Query:** `@actor` makes the caller available. The filter narrows by owner,
  then by the optional argument. `nil` means no completion filter was supplied.
  Sorting and pagination bound the result to the newest 50 rows on the first page.
- **Tool:** gives the query a described argument surface. A tool declaration does
  not by itself grant an agent permission to use it or configure an AI provider.

The adjacent `namespace.pin` file contains `reading`, so this example retains
its namespace when the folder is named `reading-list`. Keep it with the source
and manifest when copying the example.
The concept is `v1:reading:readingItem`. Names and imports are covered in
[naming conventions](naming-conventions.md).

Keep a `memql.toml` beside the file when sharing or mounting it as a domain:

```toml
memql = "1.0"
edition = "2026"
```

The checked-in example includes this manifest. It declares the language edition;
it does not select the engine release.

## Validate before connecting

With a checkout of this repository and the Go toolchain declared by its
`go.mod`, run from the repository root:

```bash
go run ./cmd/memqllint examples/reading-list
```

For your own copy, replace the path with the example folder. Directory-mode
lint also runs engine load-time validation against the embedded core tree. It
does not test cluster authorization, persistence, or live execution.
The extension also reports diagnostics as you edit.

## Run it in VS Code or Cursor

1. [Install the extension](vscode.md#get-the-extension), open the example's
   folder, and trust the workspace when you intend to connect.
2. Use **MemQL: Add Cluster**, supply your cluster domain, select it, and sign in.
   Use a development cluster and an account allowed to train constructs.
3. Use the concept's training action to dry-run and then promote `readingItem`.
   Concepts cannot be staged privately; promoting this one registers a shared
   schema, while its rows remain governed by the ownership tier.
4. Run `addReadingItem` from its CodeLens. Supply `itemId` as a fresh unique
   string (for example, `reading-first-item`) and `title` as
   `Read the MemQL language guide`. This writes an actual row.
5. Run `readingItems` with `finished` set to `false`. Your new item should appear
   with `finished: false`. You can also run `listReadingItems` with the same
   argument; it calls the query.
6. Find `readingItem` in **Data** to inspect rows visible to your account, and
   inspect the run result in the editor.

The editor injects local runnable definitions into an authoring session when
needed. Running them is not promotion; a mutation run still writes data.
To make a query or tool persist beyond the session, explicitly stage it for
personal use or promote it to the cluster. Resolve any preflight or permission
refusal before retrying. See [runtime execution](vscode-runtime-panel.md) and
[training](training.md) for the exact scope of each action.

## Try one change

Change the query's sort direction from `"desc"` to `"asc"`, save, and run it
again. With several items, the first page now starts with the oldest ones.
Saving only changes your file; the connected cluster's training indicators tell
you whether a previously promoted definition differs from it.

To extend the program, follow the shipped
[to-do mutations](../../../dsl/todos/mutations.memql) and
[queries](../../../dsl/todos/queries.memql). Updates create another version of a
row; keep its required payload and ownership intact. Read the
[authoring rules](authoring-rules.md) before changing a schema with existing data.

## Continue

[Reusable predicates](specifications.md) · [Tools and automations](memql.md) ·
[Events](../concepts/events.md) · [Access model](../operate/auth/access-model.md).

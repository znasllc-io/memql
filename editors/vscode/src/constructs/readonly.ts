// Which `.memql` files this editor marks read-only, and why.
//
// ONE RULE, and the table in the design is its consequence rather than its
// definition:
//
//   a file is read-only exactly when editing it CANNOT CHANGE WHAT THE
//   CLUSTER RUNS.
//
// A remote cluster loads its bundle from its own image, so an edit to a local
// checkout of that bundle changes nothing THERE, and the engine's core-first
// invariant seals core constructs against promotion anywhere.
//
// ON A LOCAL CLUSTER WHOSE WORKSPACE IS THE CHECKOUT THE INSTALL RECORDED, an
// edit to ANY file -- core included -- changes what the cluster runs the next
// time it is rebuilt from that checkout (Deployments: Rebuild from checkout).
// So locality is TWO FACTS, not one: the cluster is local, AND this workspace
// is its checkout. A different clone stays editable (it is the developer's
// file) but is told it is not the one the cluster builds from.
//
// The second fact is the one that is easy to drop, and dropping it is not a
// near miss: a developer with the engine cloned twice would have both copies
// unlocked on the strength of one of them being wired to the cluster, and the
// edits they made in the other would reach nothing while looking live.
//
// THE MARKING IS A COURTESY, NOT THE CONTROL. Same doctrine `deploy/actions.ts`
// states for role tiers: a user can override `files.readonlyInclude`, and what
// actually refuses is `PromoteAuthoredConstruct`, which will not let a promoted
// construct shadow a core name. The editor explains; the engine enforces. An
// implementation that treats the setting as the boundary has misread the
// design -- and one that starts ADDING files to a blocked set has broken the
// epic, because adding a new file IS the training path.
//
// CLASSIFICATION COMES FROM THE CATALOG, NOT FROM THE SHAPE OF THE PATH. A
// construct's `origin` is the ENGINE's own answer to "which tree did this come
// from". Guessing it by matching a `dsl/` directory would be this extension
// re-deriving something the cluster already told it, and would be wrong the
// first time a product bundle also lives in a directory called `dsl/` -- which
// is the convention, so: immediately.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3762 #3745

/**
 * The two fields this module reads, declared STRUCTURALLY rather than imported.
 *
 * Not a style choice. Both `Construct` (the wire type) and `CatalogConstruct`
 * (the narrowed one the tree renders) satisfy it, so the caller can pass
 * whichever it is holding -- and more to the point, a file-permission question
 * has no business depending on how the RUN path narrows an argument's type. It
 * would have made this module fail to compile over a change to an arg
 * vocabulary it never looks at.
 */
export interface OriginatedConstruct {
  /**
   * Which tree the ENGINE says this came from.
   *
   * `staged` (memql#3928) is durable but owner-scoped, and it is EDITABLE for
   * the same reason `promoted` is: it lives in the database rather than in a
   * sealed tree, and its author is the person looking at the file.
   */
  origin: "core" | "bundle" | "promoted" | "staged";
  /** Relative to the CLUSTER's tree. Empty for a promoted construct. */
  originPath: string;
}

/**
 * Why a file is read-only. Distinct values because they are distinct
 * situations with distinct ways out: a bundle file becomes editable as soon as
 * the selected cluster is a local one this workspace is the checkout of, while
 * a core file needs that AND is otherwise sealed against promotion for
 * everyone, forever.
 *
 * NO THIRD VALUE for "a local cluster that builds from somewhere else". That
 * case is not read-only at all -- the file is the developer's and editing it is
 * legitimate -- so it is a hint (`checkoutHint`), not a lock.
 */
export type ReadonlyReason = "coreSealed" | "remoteCluster";

export interface ReadonlyVerdict {
  readonly: boolean;
  /** Present exactly when `readonly`. */
  reason?: ReadonlyReason;
}

const EDITABLE: ReadonlyVerdict = { readonly: false };

/** What the badge shows. Short, because it sits beside a filename. */
export function reasonBadge(reason: ReadonlyReason): string {
  return reason === "coreSealed" ? "core" : "remote";
}

/** What the hover says. The badge is a hint; this is the sentence. */
export function reasonTooltip(reason: ReadonlyReason, clusterName: string): string {
  if (reason === "coreSealed") {
    return (
      "Core engine DSL -- read-only. The engine's core-first invariant seals these " +
      `constructs against promotion, and ${clusterName} does not build its core tree ` +
      "from this folder -- so an edit here changes nothing it runs. Training a new " +
      "construct means a new file, not an edit to this one."
    );
  }
  return (
    `This is a product bundle, and ${clusterName} is not local -- read-only. ` +
    "A remote cluster loads its bundle from its own image, so editing this checkout " +
    "changes nothing there. Select the local cluster and open its checkout to edit it."
  );
}

export interface ReadonlyInput {
  /**
   * The file, as a workspace-relative POSIX path. Compared against the
   * catalog's `originPath` through `catalogKeyFor`, which reconciles the one
   * shape difference between the two (a checkout's `dsl/` prefix). When they
   * still do not agree nothing matches and the file stays editable, which is
   * the safe direction: see the note on `constructsByPath`.
   */
  path: string;
  /**
   * The connected cluster's catalog, or undefined when not connected.
   *
   * UNDEFINED LEAVES EVERY FILE EDITABLE, and this is the most important line
   * in the module. It is the same doctrine #3759 states for `unknown` versus
   * `untrained` -- an absent cluster is not an answer -- but the blast radius
   * is larger: getting that one wrong fills a screen with false alarms, and
   * getting this one wrong locks a developer out of their own checkout because
   * their laptop went to sleep.
   *
   * It also falls straight out of the rule at the top. With no cluster there is
   * no "what the cluster runs" for an edit to fail to change, so the condition
   * for read-only cannot be met.
   */
  catalog?: ReadonlyMap<string, OriginatedConstruct[]>;
  /** Whether the selected cluster is local. Absent means NOT local. */
  clusterLocal?: boolean;
  /**
   * Whether some workspace folder IS the selected cluster's recorded checkout.
   * Absent means no.
   *
   * THE SECOND HALF OF LOCALITY, and it is a separate input rather than a
   * refinement of `clusterLocal` because the two are answered by different
   * sources: the cluster registry says local, the install receipt says which
   * directory the cluster is rebuilt from. Folding them into one boolean would
   * make the caller do the comparison, which is where a caller that forgot it
   * would be indistinguishable from one that did it and got true.
   */
  workspaceIsClusterCheckout?: boolean;
}

/**
 * Whether this editor marks `path` read-only.
 *
 * A path the catalog does not know is EDITABLE. That is what makes the
 * new-file path work with no special case: a new file is the degenerate
 * instance of "the cluster has never heard of this", so the acceptance
 * criterion that adding one is never blocked holds by construction rather than
 * by a guard somebody can later delete.
 */
export function readonlyVerdict(input: ReadonlyInput): ReadonlyVerdict {
  const { catalog } = input;
  if (catalog === undefined) return EDITABLE;

  const constructs = catalog.get(catalogKeyFor(input.path));
  if (constructs === undefined || constructs.length === 0) return EDITABLE;

  // THE TWO FACTS, tested together and BEFORE either origin. On a local cluster
  // this workspace is the checkout of, every origin is live: the next rebuild
  // loads these bytes, core included. Testing them after `core` would seal the
  // very tree the rebuild is going to compile.
  if (input.clusterLocal === true && input.workspaceIsClusterCheckout === true) return EDITABLE;

  // A file holds many constructs and they share an origin -- one file comes
  // from one tree. `some` rather than `every` anyway, because a file that
  // somehow mixed them contains a core construct, and the core reason carries
  // the extra constraint the bundle one does not: a core name cannot be
  // promoted over at all, whichever cluster is selected.
  if (constructs.some((c) => c.origin === "core")) {
    return { readonly: true, reason: "coreSealed" };
  }
  if (constructs.some((c) => c.origin === "bundle")) {
    return { readonly: true, reason: "remoteCluster" };
  }
  // `promoted` and `staged` both fall through to EDITABLE, which is the answer
  // and not an omission: neither lives in a sealed tree, and for both the file
  // on disk is the developer's own working copy of something they are entitled
  // to change. Staged is if anything the clearer case -- the only person who can
  // call it is the person looking at the file (memql#3928).
  return EDITABLE;
}

/**
 * Index a catalog by the file its constructs came from.
 *
 * A PROMOTED CONSTRUCT IS EXCLUDED, because it has no file -- its `originPath`
 * is "", and indexing that would put every construct that lives in the
 * database under one empty key and make an empty path look like a real file.
 * A STAGED one is excluded by the same test for the same reason: it lives in
 * the database too.
 */
export function constructsByPath(
  constructs: readonly OriginatedConstruct[],
): ReadonlyMap<string, OriginatedConstruct[]> {
  const byPath = new Map<string, OriginatedConstruct[]>();
  for (const construct of constructs) {
    if (construct.originPath === "") continue;
    const key = catalogKeyFor(construct.originPath);
    const bucket = byPath.get(key);
    if (bucket === undefined) byPath.set(key, [construct]);
    else bucket.push(construct);
  }
  return byPath;
}

/**
 * The glob patterns for `files.readonlyInclude`.
 *
 * Patterns rather than a boolean per open file, because the setting is what VS
 * Code consults for files it opens LATER -- a per-file answer computed on open
 * would arrive after the editor had already decided. Each path is emitted
 * literally; `files.readonlyInclude` matches a bare path as itself, and no
 * `.memql` path from the catalog contains a glob metacharacter to escape.
 *
 * Empty when not connected, which CLEARS the setting rather than leaving the
 * last cluster's answer in place. A stale read-only marking outlives the
 * connection that justified it otherwise, and workspace settings persist across
 * restarts -- so the stale one would look permanent.
 */
export function readonlyPatterns(input: {
  catalog?: ReadonlyMap<string, OriginatedConstruct[]>;
  clusterLocal?: boolean;
  workspaceIsClusterCheckout?: boolean;
}): string[] {
  const { catalog } = input;
  if (catalog === undefined) return [];
  const out: string[] = [];
  for (const key of catalog.keys()) {
    const verdict = readonlyVerdict({
      path: key,
      catalog,
      clusterLocal: input.clusterLocal,
      workspaceIsClusterCheckout: input.workspaceIsClusterCheckout,
    });
    // BOTH SPELLINGS. `files.readonlyInclude` matches the path VS Code sees,
    // which is workspace-relative: a repo checkout holds the file at
    // `dsl/cognition/queries.memql` while a bare DSL tree holds it at
    // `cognition/queries.memql`, and this module cannot see which shape the
    // workspace has. Emitting the one that does not exist costs nothing (a
    // pattern matching no file is inert); omitting the one that does leaves the
    // buffer editable with a badge beside it saying otherwise.
    if (verdict.readonly) out.push(key, `dsl/${key}`);
  }
  // Sorted so the written setting is stable: an unordered rewrite churns the
  // workspace settings file on every reconnect and shows up as a diff.
  return out.sort();
}

/**
 * What a developer editing the wrong clone is told.
 *
 * A HINT, NOT A LOCK, and the distinction is the design: the file is theirs and
 * editing it is legitimate -- what is not true is that the cluster will see it.
 * Marking it read-only would be this editor claiming ownership of a repository
 * it merely happens to have open.
 */
export function checkoutHint(clusterName: string, checkout: string): string {
  return `This folder is not the checkout ${clusterName} rebuilds from (${checkout}). Edits here do not reach ${clusterName}; open that checkout to change what it runs.`;
}

/**
 * The catalog key for a path, with or without the checkout's `dsl/` prefix.
 *
 * The catalog's `originPath` is relative to the DSL TREE ROOT
 * (`cognition/queries.memql` -- see `construct_catalog.go`), while a repo
 * checkout holds the same file one level down, at `dsl/cognition/queries.memql`.
 * Stripping the prefix on BOTH sides is what makes the two agree; without it
 * every file in an engine checkout reads as one the cluster has never heard of,
 * which is silently editable -- the failure that looks like nothing is wrong.
 *
 * A product bundle whose own tree happens to contain a `dsl/` directory loses
 * that segment too. Harmless: the key only has to be derived the same way on
 * both sides, and it is.
 */
export function catalogKeyFor(path: string): string {
  return normalizePath(path).replace(/^dsl\//, "");
}

/**
 * A leading `./` and back-slashes normalised away.
 *
 * The catalog reports POSIX paths (the cluster is Linux) while a workspace-
 * relative path on Windows arrives with back-slashes, and the two must land on
 * the same key or every file on Windows reads as unknown -- silently editable,
 * which is the failure that looks like nothing is wrong.
 */
function normalizePath(path: string): string {
  return path.replace(/\\/g, "/").replace(/^\.\//, "");
}

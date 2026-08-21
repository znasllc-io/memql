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
// A LOCAL CLUSTER LOCKS NOTHING -- no origin, core included. It is rebuilt from
// a checkout on this machine (Deployments: Rebuild from checkout), so an edit to
// any file it loaded can change what it runs, and the editor has no business
// deciding which of a developer's clones is the real one. Nothing here is a
// permission system; the safe direction is the developer's own file staying
// writable.
//
// THE CHECKOUT MATCH DECIDES THE HINT, NOT THE VERDICT. Whether a workspace
// folder IS the checkout the install recorded is what `showsCheckoutHint`
// answers, and its consequence is a hover -- "this folder is not the checkout
// <cluster> rebuilds from" -- plus the "applies on the next rebuild" wording
// the lens carries. Wiring that fact into `readonlyVerdict` instead would lock
// a second clone of the engine, which is a file the developer is entitled to
// edit and which no rule of this module has anything to say about.
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

/**
 * What the badge shows.
 *
 * ONE CHARACTER, AND THAT IS A HARD LIMIT RATHER THAN A STYLE. VS Code
 * validates a `FileDecoration` badge at no more than two code points and DROPS
 * THE WHOLE DECORATION when it is longer -- badge, colour and hover together --
 * logging `The 'badge'-property must be undefined or a short character` to a
 * channel nobody is watching. (Checked against the editor this repository's
 * host lane downloads: `FileDecoration.validate` in
 * `resources/app/out/vs/workbench/api/node/extensionHostProcess.js`, 1.134.0.)
 * The words this used to return -- "core" and "remote" -- were four and six.
 *
 * So the badge is a MARK, and the tooltip is the sentence. `C` and `R` here,
 * `L` for the not-this-checkout hint the marker adds; three letters that differ
 * at a glance, each explained on hover.
 */
export function reasonBadge(reason: ReadonlyReason): string {
  return reason === "coreSealed" ? "C" : "R";
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
    "changes nothing there. Select the local cluster to edit it, and work in the " +
    "checkout that cluster rebuilds from for the edit to reach it."
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
   * READ BY `showsCheckoutHint` AND DELIBERATELY NOT BY `readonlyVerdict`. The
   * two facts come from different sources -- the cluster registry says local,
   * the install receipt says which directory the cluster is rebuilt from -- and
   * they answer different questions: the first decides whether a file can be
   * locked at all, the second only whether the developer is told this is not
   * the folder the cluster builds from. A verdict that consulted it would lock
   * a second clone, which is the outcome the header rejects.
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

  // LOCAL, AND THEREFORE EDITABLE -- before either origin is looked at, and
  // whatever `workspaceIsClusterCheckout` says. A local cluster is rebuilt from
  // a checkout on this machine, so a file it loaded is one an edit can reach;
  // testing origins first would seal the very tree the next rebuild compiles.
  if (input.clusterLocal === true) return EDITABLE;

  // A file holds many constructs and they share an origin -- one file comes
  // from one tree. `some` rather than `every` anyway, because a file that
  // somehow mixed them contains a core construct, and the core reason carries
  // the extra constraint the bundle one does not: a core name cannot be
  // promoted over, on any cluster.
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
 *
 * Empty on a LOCAL cluster too, for the reason the header gives: a local
 * cluster locks nothing. It takes no `workspaceIsClusterCheckout` because no
 * answer to that question can put a file in this list.
 */
export function readonlyPatterns(input: {
  catalog?: ReadonlyMap<string, OriginatedConstruct[]>;
  clusterLocal?: boolean;
}): string[] {
  const { catalog } = input;
  if (catalog === undefined) return [];
  const out: string[] = [];
  for (const key of catalog.keys()) {
    const verdict = readonlyVerdict({ path: key, catalog, clusterLocal: input.clusterLocal });
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
 * Whether this file should carry the "not the checkout" hint.
 *
 * THE DECISION LIVES HERE, not in the decoration adapter, for the reason that
 * adapter's header gives: it converts a Uri to a path and a verdict to a
 * FileDecoration, and decides nothing. Three facts, and each one drops a case
 * that would otherwise be told something false:
 *
 *   the cluster is LOCAL     -- a remote cluster is not rebuilt from anything
 *                               this developer has, and its own read-only
 *                               tooltip already says so;
 *   this is NOT its checkout -- when it is, the developer is in the right
 *                               folder and there is nothing to say;
 *   the catalog KNOWS the file -- a file the cluster never loaded is a new one,
 *                               and a new file reaches the cluster by being
 *                               promoted rather than by sitting in a directory.
 *
 * A caller that has no checkout PATH to name must not render the sentence
 * either; that is the caller's own fact (`checkoutHint` takes it as an
 * argument) and it is not knowable from this input.
 */
export function showsCheckoutHint(input: ReadonlyInput): boolean {
  if (input.clusterLocal !== true) return false;
  if (input.workspaceIsClusterCheckout === true) return false;
  return input.catalog?.has(catalogKeyFor(input.path)) === true;
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

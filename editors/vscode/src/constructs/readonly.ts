// Which `.memql` files this editor marks read-only, and why.
//
// ONE RULE, and the table in the design is its consequence rather than its
// definition:
//
//   a file is read-only exactly when editing it CANNOT CHANGE WHAT THE
//   CLUSTER RUNS.
//
// Core constructs are sealed by the engine's core-first invariant, so an edit
// to one changes nothing on any cluster. A remote cluster loads its bundle from
// its own image, so an edit to a local checkout of that bundle changes nothing
// THERE -- the same file against a local cluster is live, which is why the
// verdict moves when the selected cluster does.
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
 * situations with distinct ways out: a core file is sealed for everyone
 * forever, while a bundle file becomes editable the moment you select the
 * local cluster.
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
      "constructs, so an edit here changes nothing on any cluster. Training a new " +
      "construct means a new file, not an edit to this one."
    );
  }
  return (
    `This is a product bundle, and ${clusterName} is not local -- read-only. ` +
    "A remote cluster loads its bundle from its own image, so editing this checkout " +
    "changes nothing there. Select the local cluster to edit it."
  );
}

export interface ReadonlyInput {
  /**
   * The file, as a workspace-relative POSIX path. Compared against the
   * catalog's `originPath`, which is relative to the CLUSTER's tree -- so the
   * two agree only when the workspace IS that checkout. When they do not agree
   * nothing matches and the file stays editable, which is the safe direction:
   * see the note on `constructsByPath`.
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

  const constructs = catalog.get(normalizePath(input.path));
  if (constructs === undefined || constructs.length === 0) return EDITABLE;

  // A file holds many constructs and they share an origin -- one file comes
  // from one tree. `some` rather than `every` anyway, because a file that
  // somehow mixed them contains a core construct, and the core reason is the
  // one that cannot be resolved by selecting a different cluster.
  if (constructs.some((c) => c.origin === "core")) {
    return { readonly: true, reason: "coreSealed" };
  }
  if (constructs.some((c) => c.origin === "bundle") && input.clusterLocal !== true) {
    return { readonly: true, reason: "remoteCluster" };
  }
  return EDITABLE;
}

/**
 * Index a catalog by the file its constructs came from.
 *
 * A PROMOTED CONSTRUCT IS EXCLUDED, because it has no file -- its `originPath`
 * is "", and indexing that would put every construct that lives in the
 * database under one empty key and make an empty path look like a real file.
 */
export function constructsByPath(
  constructs: readonly OriginatedConstruct[],
): ReadonlyMap<string, OriginatedConstruct[]> {
  const byPath = new Map<string, OriginatedConstruct[]>();
  for (const construct of constructs) {
    if (construct.originPath === "") continue;
    const key = normalizePath(construct.originPath);
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
}): string[] {
  const { catalog } = input;
  if (catalog === undefined) return [];
  const out: string[] = [];
  for (const path of catalog.keys()) {
    const verdict = readonlyVerdict({ path, catalog, clusterLocal: input.clusterLocal });
    if (verdict.readonly) out.push(path);
  }
  // Sorted so the written setting is stable: an unordered rewrite churns the
  // workspace settings file on every reconnect and shows up as a diff.
  return out.sort();
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

// Pushing the connected cluster's construct catalog to the language server.
//
// THE SERVER OWNS THE STATE AND THE EXTENSION OWNS THE CONNECTION, and this
// module is the seam where those two facts meet. `memql/trainingState` compares
// what a buffer declares against what a cluster has loaded; the server has the
// parser and no cluster, this process has the authenticated stream and no
// parser. So the catalog travels one way -- from here to there -- and every
// judgement about what it MEANS stays on the side that reads the .memql.
//
// WHY A NOTIFICATION AND NOT A REQUEST FIELD. `memql/trainingState` is issued by
// the gutter on every document change and by the CodeLens provider on every
// refresh. The catalog is upwards of seventeen hundred entries. Carrying it in
// the request would serialize a six-figure byte count per keystroke, so it is
// pushed when it CHANGES -- which is when a cluster is connected, switched, or
// lost -- and cached server-side until the next push.
//
// NULL IS NOT AN EMPTY LIST, and this is the rule the whole training surface
// rests on. `null` means there is no cluster, and the server answers `unknown`
// for every construct, which renders as nothing. `[]` means a connected cluster
// that has loaded nothing, and the server answers `untrained` for everything,
// which renders as a screen of Promote affordances. Sending the wrong one of
// those two is the single failure this feature has to avoid, so `publish`
// takes `undefined` and `[]` and keeps them apart all the way to the wire.
//
// EVERY FAILURE PUBLISHES NULL. A catalog read that threw is not a catalog, and
// the honest report of "I could not find out" is the same as "there is nothing
// to find out": both are the absence of an answer, and the surface that renders
// them draws nothing either way. The alternative -- leaving the last cluster's
// catalog in place -- would decorate a developer's file on the authority of a
// call that just failed.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #3759 #3745

// ONE CAPABILITY KEY, IMPORTED RATHER THAN RESTATED. The push and the
// `memql/trainingState` request are one feature -- a catalog nothing reads is
// pointless, and a request with no catalog answers `unknown` -- so they ship
// together and gate on the same flag. Spelling it a second time here would
// create a pair of strings that must agree, which is a pair of strings that can
// disagree, and the disagreement is invisible: the surface simply stays blank.
import { TRAINING_STATE_CAPABILITY } from "../state/training.js";

/**
 * The notification method. Pinned to the server's `methodClusterCatalog` by
 * `TestTrainingWireNamesMatchTheExtension`, which exists because a push to the
 * wrong method is accepted by nothing and reported to nobody -- a notification
 * has no reply -- so the surface would simply stay blank.
 */
export const CLUSTER_CATALOG_METHOD = "memql/clusterCatalog";

/**
 * One catalog entry, projected to the four fields the state rules read.
 *
 * A SUBSET of the SDK's `Construct` on purpose. Every field copied across is a
 * field the two sides can disagree about, and description / args / runnable /
 * originPath / namespace have no bearing on training state. It is also what
 * makes the push affordable at this catalog's size.
 */
export interface ClusterCatalogEntry {
  /** A concept's canonical id; the declared name for every other kind. */
  name: string;
  /** The KIND, not the authored keyword -- `mutation` for a file's `mutate`. */
  kind: string;
  /** core | bundle | promoted. Server-derived; never re-derived here. */
  origin: string;
  /** Canonical hash of the authored source. Empty means "not available". */
  sourceHash: string;
}

/** The shape of a catalog entry this module is willing to read. */
export interface CatalogSourceConstruct {
  name: string;
  kind: string;
  origin: string;
  sourceHash: string;
}

/**
 * The slice of the language client this needs. Structural, so it is fakeable --
 * the same shape `TrainingStateClient` takes for the request half.
 */
export interface ClusterCatalogClient {
  sendNotification(method: string, params: unknown): Promise<void>;
  /** The server's advertised `capabilities.experimental`, or undefined before initialize. */
  experimentalCapabilities(): Record<string, unknown> | undefined;
}

/** How the catalog is obtained. `undefined` means there is no cluster to ask. */
export type CatalogFetch = () => Promise<readonly CatalogSourceConstruct[] | undefined>;

/** The wire payload. Exported for the tests that pin the null/empty rule. */
export function clusterCatalogParams(
  constructs: readonly CatalogSourceConstruct[] | undefined,
): { catalog: ClusterCatalogEntry[] | null } {
  if (constructs === undefined) return { catalog: null };
  return {
    catalog: constructs.map((c) => ({
      name: c.name,
      kind: c.kind,
      origin: c.origin,
      sourceHash: c.sourceHash,
    })),
  };
}

/**
 * Keeps the language server's copy of the cluster catalog current.
 *
 * Refreshed on a connection change and on the client appearing, and nowhere
 * else. Those are the moments the catalog can change for a reason outside this
 * editor; a promote or demote made FROM the editor is #3763's, which owns the
 * actions and will refresh after one.
 */
export class ClusterCatalogPublisher {
  private client: ClusterCatalogClient | undefined;
  /**
   * Monotonic, and the reason is ordering rather than cancellation. Two
   * refreshes overlapping -- a connect immediately after a disconnect, which is
   * exactly what a cluster switch is -- can complete out of order, and the
   * loser landing last would leave the server holding the catalog of a cluster
   * this editor is no longer talking to.
   */
  private generation = 0;
  private disposed = false;

  constructor(private readonly fetch: CatalogFetch) {}

  /** Point at a language client, or at nothing. Refreshes either way. */
  setClient(client: ClusterCatalogClient | undefined): void {
    this.client = client;
    void this.refresh();
  }

  /**
   * Read the catalog and push it.
   *
   * Never throws and never rejects: it is driven by connection events and by
   * editor lifecycle, neither of which has anywhere to report an error, and a
   * failed catalog read is already representable as `null`.
   */
  async refresh(): Promise<void> {
    if (this.disposed) return;
    const client = this.client;
    if (client === undefined) return;
    // Feature-detected, not called blind: an older `memql-lsp` on the PATH does
    // not know this method, and an unhandled notification is not an error worth
    // a popup on either side.
    const caps = client.experimentalCapabilities();
    if (caps === undefined || caps[TRAINING_STATE_CAPABILITY] !== true) return;

    const generation = ++this.generation;
    let constructs: readonly CatalogSourceConstruct[] | undefined;
    try {
      constructs = await this.fetch();
    } catch {
      // See the module comment: a read that failed is the absence of an answer,
      // not a cluster with nothing in it.
      constructs = undefined;
    }
    if (this.disposed || generation !== this.generation) return;
    try {
      await client.sendNotification(CLUSTER_CATALOG_METHOD, clusterCatalogParams(constructs));
    } catch {
      // The server has gone away or is restarting. A restarted server holds no
      // catalog, which is `unknown`, which renders as nothing -- so there is
      // nothing to repair and nothing to tell anyone.
    }
  }

  dispose(): void {
    this.disposed = true;
    this.client = undefined;
  }
}

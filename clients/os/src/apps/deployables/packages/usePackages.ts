import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../../live/useLiveCollection";
import { DEPLOYMENT_CONCEPT, PACKAGE_CONCEPT } from "./rows";

// The Packages feeds.
//
// TWO COLLECTIONS, NOT ONE, and they are different shapes on purpose. The
// package list is the app's population and is retained for the life of the
// window. A package's deployment TIMELINE belongs to one package, so it is
// retained only while that package is open -- keeping every package's timeline
// live would subscribe the window to every deploy in the cluster to render one.
//
// Both concepts broadcast created AND updated (component/node/routing.go), and
// `updated` is what carries the weight here: a deploy advances by writing its
// own status six times through the D6 order, so watching a deploy IS watching
// those updates arrive. Without the rules the list would be correct on load and
// frozen afterwards -- the failure clients/os/README.md names as the one a new
// app makes by default, because it looks like it is working.

/**
 * Every package the caller may read, live.
 *
 * NO ARGUMENTS, and a constant KEY. `packagesAll` carries the composite tier's
 * own predicate, so the engine decides how far "all" reaches -- a cluster
 * owner sees every package, everyone else sees their own. Folding an actor id
 * into the key would restart the collection from empty the moment access
 * resolved, unmounting whatever was open.
 */
export function usePackages(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("deployables:packages", (connection) => ({
    concept: PACKAGE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.packagesAll({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, PACKAGE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/**
 * One package's deployment timeline, live.
 *
 * The key CARRIES the package id, because it changes what is read -- which is
 * the opposite case from the actor id above, and the distinction is the whole
 * rule: a key must encode everything that changes what is READ and nothing
 * that merely arrives late.
 *
 * An empty id yields a collection that seeds to nothing rather than a null
 * handle, so the caller has one shape to render either way.
 */
export function usePackageDeployments(packageId: string): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(`deployables:deployments:${packageId}`, (connection) => ({
    concept: DEPLOYMENT_CONCEPT,
    seed: async (_cursor, signal) => {
      if (packageId === "") return { rows: [], nextCursor: "" };
      const result = await connection.query.packageDeployments({ packageId }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, DEPLOYMENT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

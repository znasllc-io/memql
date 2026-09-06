import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { FILE_CONCEPT } from "./concepts";

// The file feed: ONE LiveCollection over the caller's Library files.
//
// ===========================================================================
// LIVE, AND ADMITTED PER OWNER ON BOTH PATHS
// ===========================================================================
// `component/node/routing.go` carries `graph.node.created.v1:library:file`
// and `.updated.` to browser subscribers, so the status transitions the
// analysis pass writes -- stored -> analyzing -> ready, or failed -- land
// under the person watching with nothing polling.
//
// READ THE ROUTING RULES, NOT THE CONCEPT NAMES. The Fleet's first cut
// decided `v1:cluster:node` was dark by looking for a rule carrying that
// concept's own name rather than reading the patterns, and printed the
// mistake on the page as operator-facing copy.
//
// The SUBSCRIPTION is scoped as tightly as the seed, which the feed this
// replaced could not say: `v1:library:file` declares
// `@rowAuthz(owner="ownerUserId", clusterOwner)`, and row admission gates
// subscriptions through the same function it gates reads with (memql#4309).
// So there is no client-side owner filter here and no residual to record --
// the old plan feed needed both.
//
// The KEY is a constant. It must encode everything that changes what is READ
// and nothing that merely arrives late: an actor id folded into it would
// restart the collection from empty the moment access resolved, which
// unmounts the list somebody is watching an upload in.

export function useLibraryFiles(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("training:files", (connection) => ({
    concept: FILE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.libraryFilesForOwner({}, { signal });
      // ONE PAGE, DELIBERATELY. `libraryFilesForOwner` paginates at 50
      // newest-first, and this surface is about work that is happening or
      // just happened -- walking somebody's whole Library to show a dropzone
      // would be a read nobody asked for, and the Files app is where the
      // whole Library lives. The section says it is showing recent files
      // rather than claiming a total.
      return { rows: result.rows(), nextCursor: "" };
    },
    // The re-read a `payload_omitted` event lands on, and the one a gap
    // recovery uses. An owned tier decides against the row itself, so the
    // omitted case does not arise here; the fold uses the same seam for
    // both, so it is wired either way.
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, FILE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

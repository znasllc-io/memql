import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../../live/useLiveCollection";
import { SOURCE_CREDENTIAL_CONCEPT } from "./rows";

// The credentials feed: ONE LiveCollection over the caller's own source
// credentials (epic memql#4885, design section D).
//
// RETAINED AT THE APP ROOT, beside the site and package feeds, and that is
// the README's rule rather than a third exception to it: one feed PER
// CONCEPT. What must never happen is two subscriptions over the same concept
// free to disagree about what the cluster holds -- the chip on a Source stop
// and the Sources settings group are two readings of this one feed.
//
// `v1:platform:sourceCredential` broadcasts created AND updated, and
// `updated` is what carries the weight: a revocation is a status flip on a
// row somebody may be looking at through the chip of a source that uses it,
// and "sources fetching under it will refuse at their next fetch" is a thing
// the chip has to say the moment it is true.

/**
 * Every credential the caller may read, live.
 *
 * NO ARGUMENTS and a constant KEY: `sourceCredentialsMine` carries the
 * concept's own tier, so the engine decides how far "mine" reaches. The call
 * is rendered by hand rather than through a generated builder because the
 * builder does not exist in this tree yet; the compose task switches to it,
 * and the text here is exactly what that builder renders.
 */
export function useSourceCredentials(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("deployables:sourceCredentials", (connection) => ({
    concept: SOURCE_CREDENTIAL_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.executeNamed(
        "sourceCredentialsMine",
        "query sourceCredentialsMine()",
        { signal },
      );
      return { rows: result.rows(), nextCursor: "" };
    },
    // The re-read a `payload_omitted` event lands on, and the collection's
    // gap recovery -- the same seam every other feed in this app wires.
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, SOURCE_CREDENTIAL_CONCEPT, rowId, {
        signal,
      });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

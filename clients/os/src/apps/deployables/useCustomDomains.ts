import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { CUSTOM_DOMAIN_CONCEPT } from "./domains";

// The custom-domain feed: ONE LiveCollection over this cluster's bindings.
//
// ===========================================================================
// LIVE IS THE WHOLE POINT OF THIS SURFACE
// ===========================================================================
// `component/node/routing.go` carries broadcast rules for both
// `graph.node.created.v1:platform:customDomain` and its `updated` sibling, and
// this panel is the reason. These rows are written by the reconciliation sweep
// -- on whichever replica the automations cron leader elected -- and read on
// the bff-served surface somebody is watching WHILE THEY WAIT for DNS to
// propagate. Default-deny would leave the panel correct on load and frozen
// afterwards, which is the failure that looks like it is working: a person
// pastes two records, watches nothing happen for ten minutes, and concludes
// their records are wrong.
//
// Read the ROUTING RULES before deciding a concept's feed is live -- the Fleet
// got `v1:cluster:node` wrong by looking for a rule with the concept's name in
// it rather than reading the patterns, and printed the mistake on the page as
// operator-facing copy.
//
// ===========================================================================
// THE KEY IS A CONSTANT, AND THE SITE FILTER IS NOT IN IT
// ===========================================================================
// A key must encode everything that changes what is READ, and nothing that
// merely narrows what is shown. `customDomainsAll` takes no arguments -- the
// concept's clusterOwner tier decides how far "all" reaches -- so selecting a
// different deployable changes no read at all, and folding the site id in
// would tear down the subscription and re-seed from empty every time somebody
// clicked another row on the map.
//
// The panel filters by site in the view and keys its LiveList on the site id,
// which is the deliberate re-baseline the arrival cue's own rule asks for: a
// FILTER change is not the cluster sending rows, and animating it would claim
// news that did not arrive.

/**
 * Every custom-domain binding the caller may read, live.
 *
 * NO ARGUMENTS, and removed rows included. The concept's rows survive removal
 * because the history is the audit; a seed narrowed to non-terminal rows would
 * also give the browser a list that grows a `live` row the first time one is
 * touched and never has one on load.
 */
export function useCustomDomains(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("deployables:customDomains", (connection) => ({
    concept: CUSTOM_DOMAIN_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.customDomainsAll({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    // The re-read a `payload_omitted` event lands on, and the collection's gap
    // recovery. v1:platform:customDomain is clusterOwner rather than granted,
    // so the omitted-payload path is not expected here -- it is wired because
    // the fold uses the same seam either way.
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, CUSTOM_DOMAIN_CONCEPT, rowId, {
        signal,
      });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

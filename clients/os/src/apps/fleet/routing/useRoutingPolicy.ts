import { useCallback, useMemo, useState } from "react";
import { getRowByConceptAndId, newShortId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../../live/connection";
import { useLiveCollection } from "../../../live/useLiveCollection";
import type { LabelMap } from "../labels";
import { activePolicy, routingPolicyFromRow, type RoutingPolicyRow } from "../rows";

export const WORKER_ROUTING_POLICY_CONCEPT = "v1:worker:routingPolicy";

// The caller's routing policy: read the active row live, edit it in place,
// create one only when there is none.
//
// ===========================================================================
// ABSENT IS A VALID STATE, AND THE COMMON ONE
// ===========================================================================
// A person who has never opened this section has no policy row, and the
// router applies firstFit + nextMatching. So "no row" is not an error, not an
// empty state to apologise for, and NOT something to fix by writing a default
// row on first render -- that would turn every visit to this section into a
// write, and would replace "this account has never configured routing" with
// "this account configured routing to be exactly the default", which are
// different facts about a person.
//
// ===========================================================================
// WHY create IS GUARDED BY THE READ RATHER THAN BY THE SERVER
// ===========================================================================
// ONE ACTIVE ROW PER USER is the model and the DSL cannot enforce it: @unique
// is declared metadata with no check behind it, and a mutation carries no
// filter to make its write conditional. The invariant is held on the WRITE
// side by editing in place -- createRoutingPolicy runs exactly once, for a
// caller whose read came back with no active row.
//
// The read being LIVE is what makes that safe here. The portal's version has
// to re-read after a create, because otherwise its editor would still be
// holding "no policy" and the next save would mint a second active row. This
// one folds the created row off the subscription -- v1:worker:routingPolicy
// carries broadcast routing rules (component/node/routing.go), so the write's
// own echo arrives -- and the same mechanism is what makes an edit from
// another tab or from the portal show up here.
//
// ===========================================================================
// STALENESS RESOLVES TOWARD THE ROW
// ===========================================================================
// The editor's draft is local; the policy is not. When a row arrives that
// disagrees with an untouched draft, the ROW wins -- the alternative is an
// operator saving a policy assembled from a state the cluster never had. A
// draft the operator has actually edited is left alone and the disagreement
// is SHOWN, because silently discarding someone's typing is worse than either.

export interface RoutingPolicyDraft {
  strategy: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  fallback: string;
}

export interface RoutingPolicyState {
  /** The active row, or null when the caller has never set one. */
  policy: RoutingPolicyRow | null;
  loading: boolean;
  /** The feed's own condition, for the caption. */
  liveState: string;
  error: string;
  saving: boolean;
  saveError: string;
  /** A one-line announcement for a role="status" region, so a save that
   *  changes nothing visible is still reported. */
  announcement: string;
  /**
   * Resolves TRUE only when the cluster took the write.
   *
   * Not void, and the reason is the editor's draft: it hands authority back
   * to the row on a successful save, and doing that after a REFUSAL would
   * discard the operator's edits at the same moment the surface tells them
   * their edits are still there. A boolean is what keeps those two agreeing.
   */
  save: (draft: RoutingPolicyDraft) => Promise<boolean>;
  reseed: () => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useRoutingPolicy(): RoutingPolicyState {
  const connection = useOsConnection();
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [announcement, setAnnouncement] = useState("");

  const { snapshot, reseed } = useLiveCollection<RoutingPolicyRow>(
    connection === null ? null : "fleet:routingPolicy",
    (conn) => ({
      concept: WORKER_ROUTING_POLICY_CONCEPT,
      seed: async (_cursor, signal) => {
        const result = await conn.query.myRoutingPolicies({}, { signal });
        return { rows: result.rows().map(routingPolicyFromRow), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(
          conn.query,
          WORKER_ROUTING_POLICY_CONCEPT,
          rowId,
          { signal },
        );
        return row ? routingPolicyFromRow(row as Row) : null;
      },
      paged: false,
    }),
  );

  // The newest ACTIVE row. myRoutingPolicies sorts newest first, so this is
  // the same choice routingPolicyForOwner makes server-side -- and that
  // agreement is the point: an editor picking a different row from the one
  // the router reads would write its edits somewhere nothing dispatches
  // through. The collection preserves the read's order, and a superseded row
  // folding in later has active=false and cannot displace it.
  const policy = useMemo(() => activePolicy(snapshot.rows), [snapshot.rows]);

  const save = useCallback(
    async (draft: RoutingPolicyDraft): Promise<boolean> => {
      if (connection === null) {
        setSaveError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setSaving(true);
      setSaveError("");
      setAnnouncement("");

      const held = policy;
      const input = {
        // An edit keeps the row's own id; a first save mints one the same way
        // every other row id is minted.
        policyId: held === null ? newShortId() : held.id,
        strategy: draft.strategy,
        requireLabels: draft.requireLabels,
        preferLabels: draft.preferLabels,
        fallback: draft.fallback,
      };

      const write =
        held === null
          ? connection.query.createRoutingPolicy(input)
          : connection.query.updateRoutingPolicy(input);

      try {
        await write;
        setAnnouncement(
          held === null
            ? "Routing policy created. Every call the router dispatches for you uses it from now on."
            : "Routing policy saved.",
        );
        return true;
      } catch (err: unknown) {
        setSaveError(describe(err));
        return false;
      } finally {
        setSaving(false);
      }
    },
    [connection, policy],
  );

  return {
    policy,
    loading: snapshot.state === "seeding",
    liveState: snapshot.state,
    error: snapshot.error,
    saving,
    saveError,
    announcement,
    save,
    reseed,
  };
}

import { useCallback, useEffect, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import * as fleet from "./calls";
import { activePolicy, routingPolicyFromRow, type RoutingPolicy } from "./rows";
import type { LabelMap } from "./labels";

// The caller's routing policy: read the active row, edit it in place, create
// one only when there is none.
//
// ===========================================================================
// ABSENT IS A VALID STATE, AND THE COMMON ONE
// ===========================================================================
// A person who has never opened this page has no policy row, and the router
// applies firstFit + nextMatching. So "no row" is not an error, not an empty
// state to apologise for, and not something to fix by writing a default row on
// first render -- that would turn every visit to this page into a write.
//
// ===========================================================================
// WHY create IS GUARDED BY THE READ RATHER THAN BY THE SERVER
// ===========================================================================
// ONE ACTIVE ROW PER USER is the model and the DSL cannot enforce it: @unique
// is declared metadata with no check behind it, and a mutation carries no
// filter to make its write conditional. The invariant is held on the write
// side by editing in place, which is what this hook does -- createRoutingPolicy
// runs exactly once, for a caller whose read came back with no active row.
//
// Which is why the save RE-READS. Without it, the editor would still be
// holding "no policy" after a create, and the operator's next save would mint
// a second active row. The read side is deterministic either way (the router
// sorts newest first and takes one), so a second row would not break routing
// -- it would silently strand every edit made against the older one.

export interface RoutingPolicyDraft {
  strategy: string;
  requireLabels: LabelMap;
  preferLabels: LabelMap;
  fallback: string;
}

export interface RoutingPolicyState {
  // The active row, or null when the caller has never set one.
  policy: RoutingPolicy | null;
  loading: boolean;
  error: string;
  saving: boolean;
  saveError: string;
  // A one-line announcement for a role="status" live region on the page, so a
  // save that changes nothing visible is still reported. Overwritten by each
  // successful save.
  announcement: string;
  save: (draft: RoutingPolicyDraft) => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useRoutingPolicy(): RoutingPolicyState {
  const { query } = useCluster();
  const [policy, setPolicy] = useState<RoutingPolicy | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [announcement, setAnnouncement] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    void fleet
      .myRoutingPolicies(query)
      .then((result) => {
        if (!live) return;
        setPolicy(activePolicy(result.rows().map(routingPolicyFromRow)));
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, epoch]);

  const save = useCallback(
    (draft: RoutingPolicyDraft) => {
      if (query === null) return;
      setSaving(true);
      setSaveError("");
      setAnnouncement("");

      const held = policy;
      const input = {
        // An edit keeps the row's own id; a first save mints one the same way
        // every other row id in this portal is minted.
        policyId: held === null ? newShortId() : held.id,
        strategy: draft.strategy,
        requireLabels: draft.requireLabels,
        preferLabels: draft.preferLabels,
        fallback: draft.fallback,
      };

      const write =
        held === null
          ? fleet.createRoutingPolicy(query, input)
          : fleet.updateRoutingPolicy(query, input);

      void write
        .then(() => {
          setAnnouncement(
            held === null
              ? "Routing policy created. Every call the router dispatches for you uses it from now on."
              : "Routing policy saved.",
          );
          // See the header: the re-read is what stops a second save minting a
          // second active row.
          setEpoch((n) => n + 1);
        })
        .catch((err: unknown) => setSaveError(describe(err)))
        .finally(() => setSaving(false));
    },
    [query, policy],
  );

  return { policy, loading, error, saving, saveError, announcement, save };
}

import { useCallback, useMemo, useState } from "react";

import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import {
  delegationPolicyFromRow,
  DELEGATION_POLICY_DEFAULTS,
  type DelegationPolicyRow,
} from "../rows";

// The caller's delegation policy: read it, and save the WHOLE FORM in one
// write (epic memql#5009).
//
// ===========================================================================
// AN ON-DEMAND READ, NOT A FEED, AND THAT IS CHECKED RATHER THAN ASSUMED
// ===========================================================================
// The Fleet's other two editors read live collections because
// `v1:worker:registration` and `v1:worker:routingPolicy` carry explicit
// broadcast routing rules (component/node/routing.go). `v1:worker:
// delegationPolicy` does NOT -- the block naming those three stops there --
// so a `useLiveCollection` over it would render "Loading from the cluster"
// and then a policy that never moves. The README's own warning about
// reasoning from the absence of a rule with the concept's name in it cuts
// both ways: the patterns were read, and this one is not among them.
//
// So it reads once, says WHEN it looked, and offers to look again -- the
// Accounts ledger's rule, via `useReading`.
//
// ===========================================================================
// AN ABSENT ROW IS "NEVER DELEGATE", NOT AN EMPTY FORM
// ===========================================================================
// `preferSubscriptionApps` defaults to false on the concept and the planner
// treats a missing policy as off, so a person who has never opened this
// section is not delegating anything. `found` is what lets the surface say
// that as a STATEMENT: "delegation is off" and "here is a blank form" are
// different claims, and only one of them is true. Opening this section is a
// read and writes nothing -- the same rule the routing editor states.
//
// ===========================================================================
// ONE WRITE FOR THE WHOLE FORM
// ===========================================================================
// `save` takes the entire draft. A per-field save would let somebody switch
// delegation ON while the app list was still empty -- a policy that routes
// nothing, reports no_apps_configured, and says nothing to the person who
// thought they had turned a feature on.
//
// `ownerUserId` IS NOT AN ARGUMENT. The mutation stamps it from
// `actor.userId`; it used to accept one and that was a forgery hole (closed
// in 47ae58210). The POLICY ID is derived from the user id so a second save
// updates one row rather than forking a second the planner picks between.

export type DelegationPolicyDraft = Omit<
  DelegationPolicyRow,
  "id" | "ownerUserId" | "updatedAt"
>;

export const DELEGATION_DRAFT_DEFAULTS: DelegationPolicyDraft = DELEGATION_POLICY_DEFAULTS;

/**
 * The single per-user row id, derived from the user id.
 *
 * Kept beside the read so the two cannot drift: a save writing a different id
 * would silently create a second policy, and the planner would pick between
 * them arbitrarily.
 */
export function policyRowId(ownerUserId: string): string {
  return `v1:worker:delegationPolicy:${ownerUserId.replace(/[^A-Za-z0-9_-]+/g, "-")}`;
}

export interface DelegationPolicyState {
  /** The stored policy, or null when the caller has never saved one. */
  policy: DelegationPolicyRow | null;
  /** The draft's starting values: the row's, or the planner's defaults. */
  initial: DelegationPolicyDraft;
  /** Distinguishes "saved, and switched off" from "never configured". Both
   *  mean no delegation; only one of them is a decision somebody made. */
  found: boolean;
  loading: boolean;
  error: string;
  /** When the read landed, for the surface's own "read at" line. */
  readAt: Date | null;
  reread: () => void;
  saving: boolean;
  saveError: string;
  announcement: string;
  /** TRUE only when the cluster took the write -- the editor hands authority
   *  back to the row on a success, and doing that after a refusal would
   *  discard the edits in the same beat as an error saying they are kept. */
  save: (draft: DelegationPolicyDraft) => Promise<boolean>;
}

export function useDelegationPolicy(): DelegationPolicyState {
  const connection = useOsConnection();
  const { access } = useSession();
  const ownerUserId = access?.userId ?? "";
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [announcement, setAnnouncement] = useState("");

  const read = useMemo(() => {
    const query = connection?.query ?? null;
    if (query === null) return null;
    return async (signal: AbortSignal) => {
      const result = await query.delegationPolicyForUser({}, { signal });
      // No ownerUserId argument: the query scopes on actor.userId at the
      // engine. An id the client chose would be a scope the server has to
      // distrust anyway.
      const row = result.rows()[0];
      return row === undefined ? null : delegationPolicyFromRow(row);
    };
  }, [connection]);

  const reading = useReading<DelegationPolicyRow | null>(
    `fleet:delegationPolicy:${ownerUserId}`,
    read,
  );

  const policy = reading.value ?? null;
  const initial: DelegationPolicyDraft = useMemo(
    () =>
      policy === null
        ? { ...DELEGATION_DRAFT_DEFAULTS }
        : {
            preferSubscriptionApps: policy.preferSubscriptionApps,
            eligibleKinds: [...policy.eligibleKinds],
            appOrder: [...policy.appOrder],
            maxConcurrentSessions: policy.maxConcurrentSessions,
            workspaceRoot: policy.workspaceRoot,
            credentialLifetimeSeconds: policy.credentialLifetimeSeconds,
          },
    [policy],
  );

  const reread = reading.reread;
  const save = useCallback(
    async (draft: DelegationPolicyDraft): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setSaveError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      if (ownerUserId === "") {
        setSaveError("This browser does not know who is signed in, so nothing was written.");
        return false;
      }
      setSaving(true);
      setSaveError("");
      setAnnouncement("");
      try {
        // ONE CALL, THE WHOLE FORM. Every field goes together, so no
        // intermediate state where the switch is on and the app list is
        // empty is ever written.
        await query.setDelegationPolicy({
          policyId: policyRowId(ownerUserId),
          preferSubscriptionApps: draft.preferSubscriptionApps,
          eligibleKinds: draft.eligibleKinds,
          appOrder: draft.appOrder,
          maxConcurrentSessions: draft.maxConcurrentSessions,
          workspaceRoot: draft.workspaceRoot,
          credentialLifetimeSeconds: draft.credentialLifetimeSeconds,
          updatedAt: new Date().toISOString(),
        });
        setAnnouncement(
          draft.preferSubscriptionApps
            ? "Delegation policy saved. Eligible tasks go to a local app when one is online, and run in the cluster when none is."
            : "Delegation policy saved. Delegation is off, so every task runs in the cluster.",
        );
        // The concept does not broadcast, so the row this surface holds is
        // only as fresh as the last read -- ask again rather than patching a
        // local copy that nothing would ever correct.
        reread();
        return true;
      } catch (err: unknown) {
        setSaveError(err instanceof Error ? err.message : String(err));
        return false;
      } finally {
        setSaving(false);
      }
    },
    [connection, ownerUserId, reread],
  );

  return {
    policy,
    initial,
    found: policy !== null,
    loading: reading.state === "reading" || reading.state === "unread",
    error: reading.error,
    readAt: reading.at,
    reread,
    saving,
    saveError,
    announcement,
    save,
  };
}

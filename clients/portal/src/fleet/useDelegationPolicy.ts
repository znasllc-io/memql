import { useCallback, useEffect, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useMyAccess } from "../cluster/useMyAccess";

// The delegation-policy editor's read and write (memql#4362 / #4363).
//
// # An ABSENT row is "never delegate", and the form says so
//
// The planner treats a missing policy as preferSubscriptionApps=false, so a
// person who has never opened this screen is not delegating. The editor
// renders that as an explicit off state rather than as an empty form, because
// an empty form invites the reading "not configured yet, so the default
// applies" -- and here the default IS off, which is the thing worth being
// unambiguous about before an agent runs on somebody's laptop.
//
// # The id is derived, not minted
//
// One policy per user, at an id derived from the user id, so a second save
// updates rather than forking a second row the planner would have to choose
// between.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface DelegationPolicy {
  preferSubscriptionApps: boolean;
  eligibleKinds: string[];
  appOrder: string[];
  maxConcurrentSessions: number;
  workspaceRoot: string;
  credentialLifetimeSeconds: number;
}

export const DELEGATION_POLICY_DEFAULTS: DelegationPolicy = {
  preferSubscriptionApps: false,
  eligibleKinds: [],
  appOrder: [],
  maxConcurrentSessions: 1,
  workspaceRoot: "",
  credentialLifetimeSeconds: 14400,
};

export interface DelegationPolicyState {
  policy: DelegationPolicy;
  // found distinguishes "saved, and switched off" from "never configured".
  // Both mean no delegation; only one of them is a decision somebody made.
  found: boolean;
  loading: boolean;
  error: string;
  saving: boolean;
  saveError: string;
  save: (next: DelegationPolicy) => Promise<boolean>;
}

function stringList(row: Row, key: string): string[] {
  const raw = (row as Record<string, unknown>)[key];
  if (!Array.isArray(raw)) return [];
  return raw.filter((item): item is string => typeof item === "string" && item !== "");
}

function numberField(row: Row, key: string, fallback: number): number {
  const raw = (row as Record<string, unknown>)[key];
  return typeof raw === "number" && Number.isFinite(raw) ? raw : fallback;
}

// policyRowId derives the single per-user row id. Kept next to the read so
// the two cannot drift: a save that wrote a different id would silently
// create a second policy the planner picks between arbitrarily.
export function policyRowId(ownerUserId: string): string {
  return `v1:worker:delegationPolicy:${ownerUserId.replace(/[^A-Za-z0-9_-]+/g, "-")}`;
}

export function useDelegationPolicy(): DelegationPolicyState {
  const { query } = useCluster();
  const { access } = useMyAccess();
  const ownerUserId = access?.userId ?? "";

  const [policy, setPolicy] = useState<DelegationPolicy>(DELEGATION_POLICY_DEFAULTS);
  const [found, setFound] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null || ownerUserId === "") return;
    let live = true;
    setLoading(true);
    setError("");

    void query
      .delegationPolicyForUser({})
      .then((res) => {
        if (!live) return;
        const row = res.rows()[0];
        if (row === undefined) {
          setPolicy(DELEGATION_POLICY_DEFAULTS);
          setFound(false);
          return;
        }
        setPolicy({
          preferSubscriptionApps: (row as Record<string, unknown>)["preferSubscriptionApps"] === true,
          eligibleKinds: stringList(row, "eligibleKinds"),
          appOrder: stringList(row, "appOrder"),
          maxConcurrentSessions: numberField(row, "maxConcurrentSessions", 1),
          workspaceRoot: rowString(row, "workspaceRoot"),
          credentialLifetimeSeconds: numberField(row, "credentialLifetimeSeconds", 14400),
        });
        setFound(true);
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
  }, [query, ownerUserId, epoch]);

  const save = useCallback(
    async (next: DelegationPolicy): Promise<boolean> => {
      if (query === null || ownerUserId === "") return false;
      setSaving(true);
      setSaveError("");
      try {
        // ownerUserId is NOT passed: the mutation stamps it from the actor,
        // so a caller cannot author a policy attributed to somebody else.
        await query.setDelegationPolicy({
          policyId: policyRowId(ownerUserId),
          preferSubscriptionApps: next.preferSubscriptionApps,
          eligibleKinds: next.eligibleKinds,
          appOrder: next.appOrder,
          maxConcurrentSessions: next.maxConcurrentSessions,
          workspaceRoot: next.workspaceRoot,
          credentialLifetimeSeconds: next.credentialLifetimeSeconds,
          updatedAt: new Date().toISOString(),
        });
        setEpoch((n) => n + 1);
        return true;
      } catch (err: unknown) {
        setSaveError(describe(err));
        return false;
      } finally {
        setSaving(false);
      }
    },
    [query, ownerUserId],
  );

  return { policy, found, loading, error, saving, saveError, save };
}

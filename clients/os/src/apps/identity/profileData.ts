import { useCallback, useEffect, useState } from "react";
import { renderMemQLValue, type Connection } from "@znasllc-io/memql-sdk-core/client";

import { patchMyProfile, type MyProfile } from "../../modules/profile/myProfile";
import { useOsConnection } from "../../live/connection";
import type { StorageUsage } from "../files/backups/transfers";

// The Profile's reads and writes (memql#6220), each one hook with the states
// its section draws. Nothing here decides what the person may do: every write
// is a self-only mutation the engine stamps from actor.userId, and every read
// is the caller's own.

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// ---------------------------------------------------------------------------
// Storage: the per-person allowance the platform already keeps
// ---------------------------------------------------------------------------

export type StorageRead =
  | { state: "reading" }
  | { state: "ready"; usage: StorageUsage }
  /** No storage provider behind this cluster: a fact about the cluster, said once. */
  | { state: "unavailable" }
  | { state: "failed"; error: string };

/** The caller's own allowance, through the same read Files and Users use. */
export function useMyStorage(): StorageRead {
  const connection = useOsConnection();
  const [read, setRead] = useState<StorageRead>({ state: "reading" });
  useEffect(() => {
    if (connection === null) return;
    const abort = new AbortController();
    void (async () => {
      try {
        const result = await connection.query.libraryBackupUsage({}, { signal: abort.signal });
        const usage = result.rows()[0] as unknown as StorageUsage | undefined;
        if (abort.signal.aborted) return;
        if (!usage || !Number.isFinite(Number(usage.available))) {
          setRead({ state: "unavailable" });
          return;
        }
        setRead({ state: "ready", usage });
      } catch (err) {
        if (abort.signal.aborted) return;
        const why = message(err);
        // The engine's own sentence for a cluster with no storage behind it:
        // there is nothing for this person to do, so it is not drawn as an error.
        setRead(/unavailable/i.test(why) ? { state: "unavailable" } : { state: "failed", error: why });
      }
    })();
    return () => abort.abort();
  }, [connection]);
  return read;
}

/** The share of the allowance in use, 0..1, or null when there is no personal limit. */
export function storageShare(usage: StorageUsage): number | null {
  if (usage.exempt || !(usage.limit > 0)) return null;
  return Math.min(1, Math.max(0, (usage.used + usage.reserved) / usage.limit));
}

// ---------------------------------------------------------------------------
// Passkeys
// ---------------------------------------------------------------------------

export type PasskeyRead = { state: "reading" } | { state: "ready"; active: number } | { state: "failed"; error: string };

export function useMyPasskeys(): PasskeyRead {
  const connection = useOsConnection();
  const [read, setRead] = useState<PasskeyRead>({ state: "reading" });
  useEffect(() => {
    if (connection === null) return;
    const abort = new AbortController();
    void (async () => {
      try {
        const rows = (await connection.query.passkeysForSelf({}, { signal: abort.signal })).rows();
        // ABSENT `active` IS ACTIVE: a passkey is revoked by writing false.
        if (!abort.signal.aborted) setRead({ state: "ready", active: rows.filter((row) => row["active"] !== false).length });
      } catch (err) {
        if (!abort.signal.aborted) setRead({ state: "failed", error: message(err) });
      }
    })();
    return () => abort.abort();
  }, [connection]);
  return read;
}

// ---------------------------------------------------------------------------
// The monthly AI budget (routes design record): served by the routes engine
// ---------------------------------------------------------------------------

export interface MyBudget {
  /** null: no budget, Auto keeps its recommended balance. */
  monthlyLimitUSD: number | null;
  spentThisMonthUSD: number;
  resetsAt: string;
}

export type BudgetRead =
  | { state: "reading" }
  /** The cluster does not serve the budget builtins: the section is absent. */
  | { state: "absent" }
  | { state: "ready"; budget: MyBudget }
  | { state: "failed"; error: string };

export function budgetFromRow(row: Record<string, unknown>): MyBudget {
  const limit = row["monthlyLimitUSD"];
  const spent = Number(row["spentThisMonthUSD"]);
  return {
    monthlyLimitUSD: typeof limit === "number" && Number.isFinite(limit) ? limit : null,
    spentThisMonthUSD: Number.isFinite(spent) ? spent : 0,
    resetsAt: typeof row["resetsAt"] === "string" ? row["resetsAt"] : "",
  };
}

/** Whether a refusal says the builtin does not exist on this cluster, as
 *  opposed to the read failing on one that does. */
export function budgetNotServed(error: string): boolean {
  return /\bnot found\b|\bunknown (builtin|function)\b|\bno (such )?builtin\b/i.test(error);
}

export function useMyBudget(): BudgetRead & { save: (limit: number | null) => Promise<string> } {
  const connection = useOsConnection();
  const [read, setRead] = useState<BudgetRead>({ state: "reading" });
  useEffect(() => {
    if (connection === null) return;
    const abort = new AbortController();
    void (async () => {
      try {
        const rows = (await connection.query.executeNamed("myBudget", "builtin myBudget()", { signal: abort.signal })).rows();
        if (abort.signal.aborted) return;
        setRead(rows[0] ? { state: "ready", budget: budgetFromRow(rows[0]) } : { state: "absent" });
      } catch (err) {
        if (abort.signal.aborted) return;
        const why = message(err);
        setRead(budgetNotServed(why) ? { state: "absent" } : { state: "failed", error: why });
      }
    })();
    return () => abort.abort();
  }, [connection]);

  const save = useCallback(
    async (limit: number | null): Promise<string> => {
      if (connection === null) return "Not connected to the cluster.";
      try {
        // Removal is the argument left out, which is the builtin's own contract.
        const call = limit === null ? "builtin myBudgetSave()" : `builtin myBudgetSave(monthlyLimitUSD: ${renderMemQLValue(limit)})`;
        const rows = (await connection.query.executeNamed("myBudgetSave", call)).rows();
        if (rows[0]) setRead({ state: "ready", budget: budgetFromRow(rows[0]) });
        return "";
      } catch (err) {
        return message(err);
      }
    },
    [connection],
  );

  return { ...read, save };
}

// ---------------------------------------------------------------------------
// Writes to the person's own row
// ---------------------------------------------------------------------------

export interface ProfileWrites {
  rename: (firstName: string, lastName: string) => Promise<string>;
  setAvatar: (avatar: string) => Promise<string>;
  clearAvatar: () => Promise<string>;
  setComputerUse: (enabled: boolean) => Promise<string>;
  setTimezone: (timezone: string) => Promise<string>;
}

/** The display name the concept documents: first + " " + last. */
export function composedName(firstName: string, lastName: string): string {
  return `${firstName.trim()} ${lastName.trim()}`.trim();
}

/** Each write answers "" when the engine accepted it, or the engine's reason,
 *  and patches the shared profile so every avatar and name follows at once. */
export function profileWrites(connection: Connection | null): ProfileWrites {
  async function write(run: (c: Connection) => Promise<unknown>, patch: Partial<MyProfile>): Promise<string> {
    if (connection === null) return "Not connected to the cluster.";
    try {
      await run(connection);
      patchMyProfile(patch);
      return "";
    } catch (err) {
      return message(err);
    }
  }
  return {
    rename: (firstName, lastName) => {
      const first = firstName.trim();
      const last = lastName.trim();
      const displayName = composedName(first, last);
      return write((c) => c.query.updateMyName({ firstName: first, lastName: last, displayName }), { firstName: first, lastName: last, displayName });
    },
    setAvatar: (avatar) => write((c) => c.query.setMyAvatar({ avatar }), { avatar }),
    clearAvatar: () => write((c) => c.query.clearMyAvatar({}), { avatar: "" }),
    setComputerUse: (enabled) => write((c) => c.query.toggleComputerUseEnabled({ enabled }), { computerUseEnabled: enabled }),
    setTimezone: (timezone) => write((c) => c.query.updateMyPreferences({ timezone }), { timezone }),
  };
}

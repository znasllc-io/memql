import { useCallback, useEffect, useState } from "react";
import { rowBool, rowObject, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import type { SignInPolicy } from "@znasllc-io/memql-sdk-core/identity";

import { useCluster } from "../cluster/ClusterProvider";

// The profile page's account read and its one write (memql#4318 / #4319).
//
// # Named, self-scoped queries -- not the generic concept browse
//
// `currentUser` and `passkeysForSelf` both resolve their row set from
// `actor.userId` and take no argument at all. That is not a convenience: it
// is the authorization. v1:identity:user declares no @rowAuthz tier, so a
// generic `concept==v1:identity:user` browse would return the whole
// directory -- primaryEmail, phone, birthdate, role, preferences -- to
// anybody who reached this page. The memql#2800 / #3178 split exists exactly
// so a self-service surface has a read it cannot point at a stranger, and
// this is that surface.
//
// # A read failure is not an empty account
//
// `error` is carried separately from the row, and the page renders it rather
// than an empty facts list. The same rule identity's own /me/devices follows:
// silence reads as "nothing", and "nothing" is the reassuring answer.
//
// # The preferences bag rides THIS read (memql#4523)
//
// The Settings tab needs `preferences`, and `userFull` -- the shape currentUser
// already projects -- carries it. So there is nothing to extend and, more to
// the point, nothing to extend in the WRONG place: `userDisplayCard` is the
// client-callable half of the memql#2800 split and every field added there is a
// PII leak on a query anyone can point at any user id. The settings read rides
// the self-scoped query that already returns the bag.
//
// ABSENT KEYS READ AS THEIR DOCUMENTED DEFAULT, and that is a real decision
// rather than tidiness. A concept-field @default is NEVER applied on write, so
// a row that has never had a preference set simply lacks the key -- exactly as
// the server's own Go readers see it. Rendering absent as an empty select would
// tell a person their pace was unset when the system is treating it as
// "steady"; the form must show what the cluster acts on.
//
// # Passkeys are counted here because the SWITCH depends on them
//
// The passkey-only control is disabled with zero enrolled, and the server
// refuses the change independently (component/identity/adminops). The count
// is what lets the page say WHY the control is disabled instead of just
// disabling it -- but it is not the gate, and `passkeyCountKnown` exists so
// an unreadable list renders as "we could not check" rather than as zero.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// The writable half of v1:identity:user.preferences, plus the two keys the
// Settings tab shows but does not write through the general mutation.
//
// Defaults mirror the concept block (dsl/identity/concepts.memql). They are
// spelled out here rather than left to `||` at each call site because two of
// them are FALSY-VALID: `notifications` and `dailySpaceEnabled` default to
// TRUE, so `rowBool(...) || true` is always true and `rowBool(...)` alone reads
// an unset row as off. Absent and false are different facts and only an
// explicit presence check tells them apart.
export interface MePreferences {
  language: string;
  notifications: boolean;
  theme: string;
  timezone: string;
  archiveRetentionDays: number;
  dailySpaceEnabled: boolean;
  dailySpaceRolloverAction: string;
  cursorTweenMs: number;
  takeoverMode: string;
  interactivePace: string;
  // Read-only here: written ONLY through toggleComputerUseEnabled, never
  // through updateMyPreferences, which cannot reach it (memql#2840 / #4522).
  computerUseEnabled: boolean;
}

export const PREFERENCE_DEFAULTS: MePreferences = {
  language: "",
  notifications: true,
  theme: "system",
  timezone: "",
  archiveRetentionDays: 30,
  dailySpaceEnabled: true,
  dailySpaceRolloverAction: "archive",
  cursorTweenMs: 1000,
  takeoverMode: "clean",
  interactivePace: "steady",
  computerUseEnabled: true,
};

// preferencesFrom reads the bag defensively. It is a WIRE payload: a node that
// predates a key sends nothing, and every value arrives as `unknown`.
export function preferencesFrom(bag: Record<string, unknown> | null): MePreferences {
  const str = (key: string, fallback: string): string => {
    const v = bag?.[key];
    return typeof v === "string" && v.trim() !== "" ? v : fallback;
  };
  const bool = (key: string, fallback: boolean): boolean => {
    const v = bag?.[key];
    return typeof v === "boolean" ? v : fallback;
  };
  const num = (key: string, fallback: number): number => {
    const v = bag?.[key];
    return typeof v === "number" && Number.isFinite(v) ? v : fallback;
  };
  return {
    // language and timezone default to "", so an empty stored value is not a
    // fallback case -- str() would otherwise turn a deliberate clear into the
    // default, which for these two IS the empty string anyway.
    language: str("language", PREFERENCE_DEFAULTS.language),
    notifications: bool("notifications", PREFERENCE_DEFAULTS.notifications),
    theme: str("theme", PREFERENCE_DEFAULTS.theme),
    timezone: str("timezone", PREFERENCE_DEFAULTS.timezone),
    archiveRetentionDays: num("archiveRetentionDays", PREFERENCE_DEFAULTS.archiveRetentionDays),
    dailySpaceEnabled: bool("dailySpaceEnabled", PREFERENCE_DEFAULTS.dailySpaceEnabled),
    dailySpaceRolloverAction: str(
      "dailySpaceRolloverAction",
      PREFERENCE_DEFAULTS.dailySpaceRolloverAction,
    ),
    cursorTweenMs: num("cursorTweenMs", PREFERENCE_DEFAULTS.cursorTweenMs),
    takeoverMode: str("takeoverMode", PREFERENCE_DEFAULTS.takeoverMode),
    interactivePace: str("interactivePace", PREFERENCE_DEFAULTS.interactivePace),
    computerUseEnabled: bool("computerUseEnabled", PREFERENCE_DEFAULTS.computerUseEnabled),
  };
}

export interface MeAccount {
  userId: string;
  displayName: string;
  primaryEmail: string;
  role: string;
  memberSince: string;
  lastSeenAt: string;
  sharedMailbox: boolean;
  // "any" | "passkey_only". Normalized: an empty stored value reads as "any",
  // the same permissive reading the server takes, because rows written before
  // the field existed carry nothing and the alternative would render every
  // legacy account as locked down.
  signInPolicy: SignInPolicy;
  preferences: MePreferences;
}

export interface MePasskey {
  id: string;
  label: string;
  // "Recoverable" when the credential syncs to the authenticator's backup
  // (a platform passkey in an account keychain); "This device only" when it
  // does not. It is the fact that decides whether losing the phone loses the
  // key, which is the only thing a person needs from this row.
  backedUp: boolean;
  addedAt: string;
}

export interface MeState {
  account: MeAccount | null;
  passkeys: MePasskey[];
  passkeyCountKnown: boolean;
  loading: boolean;
  error: string;
  reload: () => void;
  // The passkey-only switch. Busy while in flight; policyError carries the
  // server's own sentence on a refusal, never a paraphrase.
  policyBusy: boolean;
  policyError: string;
  setSignInPolicy: (policy: SignInPolicy) => void;
}

function normalizePolicy(value: string): SignInPolicy {
  return value.trim() === "passkey_only" ? "passkey_only" : "any";
}

function accountFromRow(row: Row | null): MeAccount | null {
  if (row === null) return null;
  return {
    userId: rowString(row, "id"),
    displayName: rowString(row, "displayName"),
    primaryEmail: rowString(row, "primaryEmail"),
    role: rowString(row, "role"),
    memberSince: rowString(row, "createdAt"),
    lastSeenAt: rowString(row, "lastSeenAt"),
    sharedMailbox: rowBool(row, "sharedMailbox"),
    signInPolicy: normalizePolicy(rowString(row, "signInPolicy")),
    preferences: preferencesFrom(rowObject(row, "preferences")),
  };
}

function passkeyFromRow(row: Row): MePasskey {
  return {
    id: rowString(row, "id"),
    // A passkey with no label is one somebody never named. "Unnamed passkey"
    // rather than an empty cell: a blank reads as a rendering fault on a page
    // whose whole job is telling you what can reach your account.
    label: rowString(row, "label") || "Unnamed passkey",
    backedUp: rowBool(row, "backupState"),
    addedAt: rowString(row, "createdAt"),
  };
}

export function useMe(): MeState {
  const { query, clients } = useCluster();
  const [account, setAccount] = useState<MeAccount | null>(null);
  const [passkeys, setPasskeys] = useState<MePasskey[]>([]);
  const [passkeyCountKnown, setPasskeyCountKnown] = useState(false);
  // Starts true, for the reason useArtifacts states at length: a read is
  // effectively in flight from mount, and `false` would claim "confirmed
  // empty" before anything had been attempted.
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [policyBusy, setPolicyBusy] = useState(false);
  const [policyError, setPolicyError] = useState("");

  useEffect(() => {
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    // The two reads settle TOGETHER but fail SEPARATELY, and both halves of
    // that matter.
    //
    // Together, because staggered arrivals would render an account whose
    // Security tab still said "checking" and whose switch flickered between
    // disabled reasons.
    //
    // Separately, because they answer different questions and one failing
    // does not make the other unknown. Promise.all would have collapsed them:
    // an unreadable passkey list would blank the account facts, and an
    // unreadable account row would report the passkey count as unknown when
    // it had arrived perfectly well.
    void Promise.allSettled([query.currentUser({}), query.passkeysForSelf({})])
      .then(([user, keys]) => {
        if (!live) return;

        if (user.status === "fulfilled") {
          const rows = user.value.rows();
          setAccount(accountFromRow(rows.length > 0 ? (rows[0] ?? null) : null));
        } else {
          setError(describe(user.reason));
        }

        if (keys.status === "fulfilled") {
          setPasskeys(keys.value.rows().map(passkeyFromRow));
          setPasskeyCountKnown(true);
        } else {
          // FAIL CLOSED on the count, matching the server. An unreadable list
          // must not render as "you have no passkeys", which would then
          // explain a disabled switch with a reason that is not true.
          setPasskeys([]);
          setPasskeyCountKnown(false);
        }
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  const setPolicy = useCallback(
    (policy: SignInPolicy) => {
      if (clients === null) return;
      setPolicyBusy(true);
      setPolicyError("");
      void clients
        .setSignInPolicy(policy)
        .then((res) => {
          if (res.success) {
            setEpoch((n) => n + 1);
            return;
          }
          // The SERVER'S sentence, verbatim. It distinguishes "add a passkey
          // first" from "we could not check just now", and those ask the
          // reader for different next steps. A refusal shown as a silent
          // no-op is the one outcome this control must never produce.
          setPolicyError(res.errorMessage || "That change was refused.");
          // Re-read so the switch shows what the account actually holds
          // rather than the position the click left it in.
          setEpoch((n) => n + 1);
        })
        .catch((err: unknown) => setPolicyError(describe(err)))
        .finally(() => setPolicyBusy(false));
    },
    [clients],
  );

  return {
    account,
    passkeys,
    passkeyCountKnown,
    loading,
    error,
    reload,
    policyBusy,
    policyError,
    setSignInPolicy: setPolicy,
  };
}

import { useCallback, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";
import {
  mintAccountToken,
  revokeAccountToken,
  type AccountTokenMintResult,
} from "@znasllc-io/memql-sdk-core/identity";

import { useOsConnection } from "../../live/connection";

// Every write the Accounts app makes, and the busy/error pair each one owns.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// `v1:accounts:account` declares the composite tier, so guardRowAuthzWrite
// resolves the target row and admits its owner, with the cluster-owner path as
// the separate explicit escape. A user edits their own accounts, an operator
// edits any, and a cross-user write is refused before the merge. Editing a
// boolean in a browser changes none of it.
//
// ===========================================================================
// A REFUSAL IS THE SERVER'S OWN SENTENCE, AND IT RENDERS BESIDE THE CONTROL
// ===========================================================================
// Never a toast. The create form, the edit form and the archive confirm are
// three different places on screen, so they get three different error slots --
// a shared one would put a refusal under a control somebody was looking at
// that names a failure they did not cause.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * Blank strings are OMITTED rather than sent.
 *
 * `updateClientAccount` read-merges, so an omitted field inherits its stored
 * value -- which is what lets the edit form send only what changed. An
 * explicit "" is a VALUE and would blank the stored one, so a form that sent
 * every field on every save would let somebody clear a colleague's notes by
 * correcting a domain.
 */
function omitBlank(value: string): string | undefined {
  const trimmed = value.trim();
  return trimmed === "" ? undefined : trimmed;
}

export interface AccountFacts {
  name: string;
  domain: string;
  primaryContactName: string;
  primaryContactEmail: string;
  notes: string;
}

export interface WriteState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  reset: () => void;
}

export interface CreateAccountState extends WriteState {
  /** The id of the last account created here, so the surface can say which. */
  createdId: string;
  create: (facts: AccountFacts) => Promise<string>;
}

export function useCreateAccount(): CreateAccountState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [createdId, setCreatedId] = useState("");

  const create = useCallback(
    async (facts: AccountFacts): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return "";
      }
      const name = facts.name.trim();
      if (name === "") {
        // The one rule a browser can answer, answered here rather than sent.
        // `name` is `string!` on the concept, so the server would refuse it
        // too -- but a round trip to be told what this form already knows is
        // a round trip somebody waits for.
        setError("Give the client a name first.");
        return "";
      }
      const accountId = newShortId();
      setBusy(true);
      setError("");
      setCreatedId("");
      try {
        await query.createClientAccount({
          accountId,
          name,
          domain: omitBlank(facts.domain),
          primaryContactName: omitBlank(facts.primaryContactName),
          primaryContactEmail: omitBlank(facts.primaryContactEmail),
          notes: omitBlank(facts.notes),
        });
        setCreatedId(accountId);
        // NOTHING IS INSERTED LOCALLY. `graph.node.created.v1:accounts:account`
        // is broadcast, so the row arrives on the feed the list already draws
        // -- with the arrival cue, exactly like an account somebody else
        // created. A local insert would put a row on screen the cluster had
        // not confirmed, and the two would differ in whatever the optimistic
        // copy guessed wrong.
        return accountId;
      } catch (err: unknown) {
        setError(describe(err));
        return "";
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return { busy, error, createdId, create, reset: () => setError("") };
}

export interface UpdateAccountState extends WriteState {
  /**
   * Save the facts a person edited.
   *
   * `patch` carries only what the form changed. Everything blank is omitted,
   * so the read-merge inherits -- see `omitBlank`.
   */
  update: (accountId: string, patch: Partial<AccountFacts>) => Promise<boolean>;
}

export function useUpdateAccount(): UpdateAccountState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const update = useCallback(
    async (accountId: string, patch: Partial<AccountFacts>): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(true);
      setError("");
      try {
        // `configuredAt` is stamped by this mutation and by nothing else, so
        // this call is also what retires the first-run card (D7). There is no
        // separate "mark configured" write to forget.
        await query.updateClientAccount({
          accountId,
          name: patch.name === undefined ? undefined : omitBlank(patch.name),
          domain: patch.domain === undefined ? undefined : omitBlank(patch.domain),
          primaryContactName:
            patch.primaryContactName === undefined
              ? undefined
              : omitBlank(patch.primaryContactName),
          primaryContactEmail:
            patch.primaryContactEmail === undefined
              ? undefined
              : omitBlank(patch.primaryContactEmail),
          notes: patch.notes === undefined ? undefined : omitBlank(patch.notes),
        });
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return { busy, error, update, reset: () => setError("") };
}

export interface ArchiveAccountState extends WriteState {
  archive: (accountId: string) => Promise<boolean>;
}

export function useArchiveAccount(): ArchiveAccountState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const archive = useCallback(
    async (accountId: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(true);
      setError("");
      try {
        await query.archiveClientAccount({ accountId });
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return { busy, error, archive, reset: () => setError("") };
}

// ---------------------------------------------------------------------------
// Credentials issued on behalf of a client
// ---------------------------------------------------------------------------

/**
 * A refused mint or revoke, unpacked into what the surface renders.
 *
 * `auditEventId` comes back on a REFUSAL as well as on success -- a blocked
 * mint writes `account_token_create_blocked` -- and it is surfaced rather
 * than swallowed because it is what an operator quotes when they ask why.
 */
export interface AccountTokenRefusal {
  /** The server's own message, verbatim and in the data voice. */
  detail: string;
  /** The `v1:identity:auditEvent` this attempt wrote. "" when unknown. */
  auditEventId: string;
}

export interface AccountTokenActions {
  /**
   * What is in flight: `"mint"`, a credential id, or "". A KEY rather than a
   * boolean because the revokes are per row -- one shared flag would spin
   * every row's control while one of them was being revoked.
   */
  busyKey: string;
  refusal: AccountTokenRefusal | null;
  reset: () => void;
  /**
   * Issue a credential. THE PLAINTEXT IS RETURNED, NEVER HELD.
   *
   * `plainToken` exists in exactly one place -- this reply -- because the
   * cluster persisted only its SHA-256 digest and no later call can retrieve
   * it. A hook that kept it would keep it for the life of the window, which
   * is precisely the custody this surface is built to avoid; the caller holds
   * it in state that dies with the panel and discards it on Done.
   */
  mint: (accountId: string, label: string) => Promise<AccountTokenMintResult | null>;
  revoke: (identityId: string) => Promise<boolean>;
}

/**
 * The two credential writes, over the DISPATCHER rather than the query path.
 *
 * These are the app's only calls that are not named queries or mutations.
 * They are gRPC envelopes for a reason the handler states: the mint's
 * plaintext must never reach the engine, a log line or a payload, and the
 * revoke's audit id must come back on the reply.
 *
 * NOTHING HERE CHECKS A ROLE, exactly as the writes above do not. The mint's
 * gate is `query accountById` run AS THE CALLER inside the handler, against a
 * concept declaring `@rowAuthz(owner="ownerUserId")` -- and that read has no
 * cluster-owner escape, so an admin is refused a colleague's account like
 * anybody else. Zero rows IS the refusal. A boolean edited in a browser
 * changes none of it, and a role gate written here would be a second copy of
 * a rule that would then be free to disagree with the first.
 */
export function useAccountTokenActions(): AccountTokenActions {
  const connection = useOsConnection();
  const [busyKey, setBusyKey] = useState("");
  const [refusal, setRefusal] = useState<AccountTokenRefusal | null>(null);

  const mint = useCallback(
    async (accountId: string, label: string): Promise<AccountTokenMintResult | null> => {
      const dispatcher = connection?.dispatcher ?? null;
      if (dispatcher === null) {
        setRefusal({
          detail: "Not connected to the cluster, so nothing was issued.",
          auditEventId: "",
        });
        return null;
      }
      setBusyKey("mint");
      setRefusal(null);
      try {
        const result = await mintAccountToken(dispatcher, { accountId, label });
        // TWO FAILURE SHAPES, and only one of them throws. A transport fault
        // or an unexpected envelope rejects; a REFUSAL comes back as an
        // ordinary reply carrying success=false, which a `try` alone reads as
        // a success and renders as a credential with no token in it.
        if (!result.success || result.plainToken === "") {
          setRefusal({
            detail:
              result.errorMessage ||
              result.errorCode ||
              "The cluster refused this and said nothing about why.",
            auditEventId: result.auditEventId,
          });
          return null;
        }
        return result;
      } catch (err: unknown) {
        setRefusal({
          detail: err instanceof Error ? err.message : String(err),
          auditEventId: "",
        });
        return null;
      } finally {
        setBusyKey("");
      }
    },
    [connection],
  );

  const revoke = useCallback(
    async (identityId: string): Promise<boolean> => {
      const dispatcher = connection?.dispatcher ?? null;
      if (dispatcher === null) {
        setRefusal({
          detail: "Not connected to the cluster, so nothing was revoked.",
          auditEventId: "",
        });
        return false;
      }
      setBusyKey(identityId);
      setRefusal(null);
      try {
        const result = await revokeAccountToken(dispatcher, identityId);
        if (!result.success) {
          setRefusal({
            detail:
              result.errorMessage ||
              result.errorCode ||
              "The cluster refused this and said nothing about why.",
            auditEventId: result.auditEventId,
          });
          return false;
        }
        return true;
      } catch (err: unknown) {
        setRefusal({
          detail: err instanceof Error ? err.message : String(err),
          auditEventId: "",
        });
        return false;
      } finally {
        setBusyKey("");
      }
    },
    [connection],
  );

  return { busyKey, refusal, reset: () => setRefusal(null), mint, revoke };
}

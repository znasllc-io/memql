import { useCallback, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

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

import { useCallback, useState } from "react";
import { rowString } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { normalizeHostname } from "./domains";

// The two writes the Domains panel makes.
//
// ===========================================================================
// NEITHER OF THESE IS THE AUTHORIZATION
// ===========================================================================
// `v1:platform:customDomain` declares @rowAuthz(clusterOwner), so the engine
// decides who may bind a domain and the three D10 guards decide which hostnames
// may be bound -- in Go, beside executeWrite, because none of them is
// expressible in a mutation body. The panel's admin gate is presentation
// (design D1): showing somebody a button that always fails teaches nobody who
// can use it.
//
// TWO HOOKS, NOT ONE, for the reason actions.ts already states: a refusal
// belongs beside the control that produced it, and the add form and a row's
// remove confirm are two different places on the screen. A shared error pair
// would put a hostname refusal under a Remove button somebody was looking at.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// ---------------------------------------------------------------------------
// Bind a domain
// ---------------------------------------------------------------------------

export interface AddDomainOutcome {
  domainId: string;
  hostname: string;
  token: string;
  verifyRecordName: string;
  pointsToKind: string;
  pointsToTarget: string;
}

export interface AddDomainState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt succeeded. */
  error: string;
  outcome: AddDomainOutcome | null;
  add: (siteId: string, hostname: string) => Promise<boolean>;
  reset: () => void;
}

export function useAddDomain(): AddDomainState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [outcome, setOutcome] = useState<AddDomainOutcome | null>(null);

  const add = useCallback(
    async (siteId: string, hostname: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      const host = normalizeHostname(hostname);
      if (siteId === "" || host === "") {
        setError("Type the domain you want this deployable served at.");
        return false;
      }
      setBusy(true);
      setError("");
      setOutcome(null);
      try {
        // A CAPABILITY, NOT A MUTATION, and the reason is the token: the value
        // a client publishes at `_memql-verify.<hostname>` proves control of
        // the name, so a caller who chooses it proves nothing -- one constant
        // published under a thousand domains would verify all of them. It is
        // minted server-side, which is why `createCustomDomain` is @serverOnly
        // and this is the only way in.
        const result = await query.customDomainAdd({ siteId, hostname: host });
        const row = result.rows()[0] ?? null;
        setOutcome({
          domainId: rowString(row, "domainId"),
          hostname: rowString(row, "hostname"),
          token: rowString(row, "token"),
          verifyRecordName: rowString(row, "verifyRecordName"),
          pointsToKind: rowString(row, "pointsToKind"),
          pointsToTarget: rowString(row, "pointsToTarget"),
        });
        // NOTHING IS INSERTED LOCALLY. The row arrives on its own broadcast,
        // with the arrival cue, exactly like one somebody else created -- a
        // local insert would put a row on screen the cluster had not confirmed,
        // and the two would differ in whatever the optimistic copy guessed.
        return true;
      } catch (err: unknown) {
        // VERBATIM. The three guards a browser cannot mirror -- the cluster's
        // own domain, a collision with a site or another binding, the per-site
        // maximum -- are refused server-side, and their messages name the
        // colliding row and the rule. A friendlier paraphrase would drop the
        // one fact that helps.
        setError(describe(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    outcome,
    add,
    reset: () => {
      setError("");
      setOutcome(null);
    },
  };
}

// ---------------------------------------------------------------------------
// Remove a binding
// ---------------------------------------------------------------------------

export interface RemoveDomainState {
  busy: string;
  error: string;
  remove: (domainId: string) => Promise<boolean>;
  reset: () => void;
}

export function useRemoveDomain(): RemoveDomainState {
  const connection = useOsConnection();
  // The id being removed rather than a boolean, so a list of rows can disable
  // exactly the one in flight instead of all of them.
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  const remove = useCallback(
    async (domainId: string): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was changed.");
        return false;
      }
      setBusy(domainId);
      setError("");
      try {
        // THE HOSTNAME STOPS RESOLVING AT THIS WRITE, not when the Ingress is
        // deleted: the edge's own read filters `status=="live"`, so serving
        // stops the moment the row lands. The sweep takes the cluster objects
        // away afterwards and walks the row to `removed`.
        await query.removeCustomDomain({ domainId });
        return true;
      } catch (err: unknown) {
        setError(describe(err));
        return false;
      } finally {
        setBusy("");
      }
    },
    [connection],
  );

  return { busy, error, remove, reset: () => setError("") };
}

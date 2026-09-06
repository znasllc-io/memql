import { useCallback, useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { fetchSigningKeys, keysetFingerprint, readAuditEvents, type SigningKey } from "./adminWire";

// The signing keys, and the question the section is actually about.
//
// ===========================================================================
// THE INTERESTING FACT IS NOT THE KEYS. IT IS WHETHER THE REPLICAS AGREE.
// ===========================================================================
// A list of published keys is a fact an operator can get from `curl`. What
// they cannot get from `curl` -- and what actually breaks clusters -- is
// whether every identity replica is publishing the SAME keyset. Divergent
// keysets fail roughly half of all auth (memql#3400): a token minted by
// replica A is rejected by a verifier that fetched JWKS from replica B, so
// sign-in works, then does not, then does, and every manifest looks correct.
// `make status` checks it on the command line; nothing in a browser did.
//
// HOW THIS CHECKS IT, AND WHAT THE CHECK IS WORTH. The front door routes each
// request independently, so N reads of the same URL land on replicas of the
// front door's choosing. Therefore:
//
//   - DISAGREEMENT IS PROOF. Two different keysets came back from the same
//     hostname; there is no reading of that in which the cluster is coherent.
//   - AGREEMENT IS EVIDENCE, NOT PROOF. N reads may all have landed on one
//     replica. The section says the number of distinct answers and the number
//     of reads, and never claims more than that.
//
// A check that reported "coherent" from one read would be worse than no check:
// it is the reassuring answer, it is the one an operator would stop at, and it
// is the answer a broken cluster gives half the time.

/** How many independent reads one probe makes. Four is enough to make a
 *  single-replica sample unlikely at two replicas without turning a section
 *  open into a burst; the operator can press again. */
export const PROBE_READS = 4;

export interface KeysetProbe {
  /** Every distinct keyset fingerprint seen, in the order first seen. */
  distinct: string[];
  /** How many reads answered at all. */
  reads: number;
  /** The keys from the first successful read, for the listing. */
  keys: SigningKey[];
  /** Per-read failures, verbatim. A read that failed is not a keyset that
   *  disagreed, and conflating them would report an outage as divergence. */
  failures: string[];
}

export function agreementOf(probe: KeysetProbe): {
  tone: "agree" | "diverged" | "unknown";
  sentence: string;
} {
  if (probe.reads === 0) {
    return {
      tone: "unknown",
      sentence: "No read of the JWKS feed answered, so nothing can be said about the keysets.",
    };
  }
  if (probe.distinct.length > 1) {
    return {
      tone: "diverged",
      sentence: `${probe.distinct.length} different keysets came back from the same hostname across ${probe.reads} reads. Replicas are publishing different keys, and roughly that share of sign-ins will fail.`,
    };
  }
  return {
    tone: "agree",
    sentence: `${probe.reads} reads all returned the same keyset. That is evidence the replicas agree, not proof -- the front door chooses which replica answers each read, so they may all have landed on one.`,
  };
}

/**
 * Probe the JWKS feed `PROBE_READS` times and report what came back.
 *
 * Sequential rather than concurrent, deliberately: concurrent requests over
 * HTTP/2 to one origin share a connection and are far more likely to reach the
 * same backend, which would make the agreement reading weaker while looking
 * stronger.
 */
export async function probeKeyset(
  origin: string,
  fetchImpl: typeof globalThis.fetch,
  signal: AbortSignal,
): Promise<KeysetProbe> {
  const distinct: string[] = [];
  const failures: string[] = [];
  let keys: SigningKey[] = [];
  let reads = 0;

  for (let i = 0; i < PROBE_READS; i += 1) {
    if (signal.aborted) break;
    try {
      const answer = await fetchSigningKeys(origin, fetchImpl, signal);
      reads += 1;
      if (keys.length === 0) keys = answer;
      const print = keysetFingerprint(answer);
      if (!distinct.includes(print)) distinct.push(print);
    } catch (err: unknown) {
      failures.push(err instanceof Error ? err.message : String(err));
    }
  }

  return { distinct, reads, keys, failures };
}

export interface Rotation {
  at: string;
  by: string;
}

/** The most recent successful key rotation in the audit page, or null. */
export function lastRotation(events: readonly Row[]): Rotation | null {
  for (const event of events) {
    if (event["action"] !== "jwks_rotated") continue;
    if (event["outcome"] !== "success") continue;
    const at = typeof event["occurredAt"] === "string" ? event["occurredAt"] : "";
    const by = typeof event["actorEmail"] === "string" ? event["actorEmail"] : "";
    return { at, by: by === "" ? "the rotation schedule" : by };
  }
  return null;
}

export interface KeyFacts {
  probe: KeysetProbe;
  rotation: Rotation | null;
  /** True when the rotation read was not attempted because the caller is not
   *  the cluster owner. Absent history and un-asked history are different
   *  answers. */
  rotationWithheld: boolean;
  loading: boolean;
  error: string;
  fetchedAt: number | null;
  reload: () => void;
}

const EMPTY_PROBE: KeysetProbe = { distinct: [], reads: 0, keys: [], failures: [] };

export function useKeyFacts(origin: string, isOwner: boolean): KeyFacts {
  const connection = useOsConnection();
  const [probe, setProbe] = useState<KeysetProbe>(EMPTY_PROBE);
  const [rotation, setRotation] = useState<Rotation | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (origin === "") {
      setError("This deployment publishes no identity origin, so there is no feed to read.");
      return;
    }
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    void (async () => {
      const answer = await probeKeyset(origin, globalThis.fetch, controller.signal);
      if (stale) return;
      setProbe(answer);
      setFetchedAt(Date.now());
      // The rotation history is owner-only (`recentAuditEvents` filters on
      // actor.isClusterOwner). Gate the CALL so an admin does not issue a read
      // whose empty answer we already know; the engine remains the authority
      // either way.
      if (!isOwner || connection === null) {
        setRotation(null);
        return;
      }
      const events = await readAuditEvents(connection, "configuration", controller.signal);
      if (stale) return;
      setRotation(lastRotation(events));
    })()
      .catch((err: unknown) => {
        if (stale) return;
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, origin, isOwner, epoch]);

  return useMemo(
    () => ({
      probe,
      rotation,
      rotationWithheld: !isOwner,
      loading,
      error,
      fetchedAt,
      reload,
    }),
    [probe, rotation, isOwner, loading, error, fetchedAt, reload],
  );
}

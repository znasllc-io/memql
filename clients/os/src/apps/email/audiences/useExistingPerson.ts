import { useEffect, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { addressDigest, normalizeAddress } from "./addressDigest";

export interface Existing {
  /** `idle` until the address looks like one; `checking` while the lookup runs. */
  state: "idle" | "checking" | "free" | "taken" | "failed";
  error: string;
  /** The membership the lookup found, whole, when the address is `taken`. */
  person: Row | null;
}

const SETTLE_MS = 200;
const IDLE: Existing = { state: "idle", error: "", person: null };
const CHECKING: Existing = { state: "checking", error: "", person: null };

/**
 * Whether an audience already holds an address, asked before Add is offered.
 *
 * The lookup is by the address's digest, so it cannot see a person written
 * before digests existed; the engine refuses that duplicate itself and the
 * wizard shows its sentence. A row the digest finds is believed only when its
 * own address agrees, because a raw write can set a digest that is not its
 * address's.
 */
export function useExistingPerson(audienceId: string, address: string, looksLikeAnAddress: boolean): Existing & { again: () => void } {
  const connection = useOsConnection();
  const wanted = normalizeAddress(address);
  const key = `${audienceId}|${wanted}`;
  // Bumped to ask again after a lookup that failed.
  const [attempt, setAttempt] = useState(0);
  const [answer, setAnswer] = useState<{ key: string; existing: Existing } | null>(null);
  useEffect(() => {
    if (!connection || !looksLikeAnAddress || wanted === "") return;
    const request = new AbortController();
    const timer = setTimeout(() => {
      void (async () => {
        try {
          const rows = (await connection.query.recipientInAudienceByDigest({ audienceId, emailDigest: await addressDigest(address) }, { signal: request.signal })).rows();
          const found = rows.find(row => normalizeAddress(rowString(flatten(row), "email")) === wanted) ?? null;
          if (!request.signal.aborted) setAnswer({ key, existing: { state: found ? "taken" : "free", error: "", person: found } });
        } catch (failure) {
          if (!request.signal.aborted) setAnswer({ key, existing: { state: "failed", error: failure instanceof Error ? failure.message : String(failure), person: null } });
        }
      })();
    }, SETTLE_MS);
    return () => { clearTimeout(timer); request.abort(); };
    // `address` is what `wanted` is made from; the key already carries it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connection, audienceId, key, looksLikeAnAddress, attempt]);
  const again = () => { setAnswer(null); setAttempt(count => count + 1); };
  if (!looksLikeAnAddress || wanted === "") return { ...IDLE, again };
  return { ...(answer?.key === key ? answer.existing : CHECKING), again };
}

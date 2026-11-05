import { useCallback, useEffect, useRef, useState } from "react";
import { isTransportInterruption } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";

export interface AzureEmailScope {
  subscriptionId: string; resourceGroup: string; dataLocation: string;
  createResourceGroup?: boolean; resourceGroupLocation?: string;
}
export interface AzureEmailPlan extends AzureEmailScope {
  domain: string; emailService: string; communicationService: string;
}
export interface AzureEmailReply {
  requestedSender?: { username: string; displayName: string; replyTo?: string };
  senders?: Record<string, string>; tenantId?: string;
  status: string; applicationReady?: boolean; capture?: boolean;
  sessionId?: string; userCode?: string; verificationUri?: string; interval?: number;
  subscriptionId?: string; resourceGroup?: string; dataLocation?: string;
  createResourceGroup?: boolean; resourceGroupLocation?: string;
  choices?: { id: string; name: string }[];
  plan?: AzureEmailPlan; planId?: string; sender?: string; replyTo?: string;
  records?: { purpose: string; name: string; type: string; value: string; status: string }[];
  deliveryFeedback?: "not-configured" | "pending" | "ready";
  operations?: { operationId: string; status: string; detail: string; submittedAt: string; checkedAt?: string; deliveryStatus?: string; deliveryAt?: string }[];
}
export const AZURE_EMAIL_LOCATIONS = ["Africa", "Asia Pacific", "Australia", "Brazil", "Canada", "Europe", "France", "Germany", "India", "Japan", "Korea", "Norway", "Switzerland", "United Arab Emirates", "United Kingdom", "United States"];

/**
 * Steps the engine saves and resumes, so asking again after a dropped
 * connection is safe: the page keeps asking by itself for a short while.
 */
const RESUMABLE = new Set(["provision", "verify", "sender", "domainStatus", "status", "clusterStatus"]);
const RETRY_BACKOFF_MS = [1000, 2000, 4000, 8000];
const RETRY_EVERY_MS = 10_000;
const RETRY_FOR_MS = 120_000;
const WORDS: Record<string, string> = { provision: "Continue setup", verify: "Check domain records", sender: "Save sender" };

/** What a person reads when the connection dropped, never the transport's words. */
export function droppedSentence(action: string): string {
  return WORDS[action] ? `The connection to MemQL dropped. ${WORDS[action]} picks up where it left off.` : "The connection to MemQL dropped. Try again.";
}

const pause = (ms: number, signal: AbortSignal) => new Promise<void>(done => {
  const finish = () => { window.clearTimeout(timer); signal.removeEventListener("abort", finish); done(); };
  const timer = window.setTimeout(finish, ms);
  signal.addEventListener("abort", finish);
});

/** One request at a time; discard replies from an old connection or organization. */
export function useAzureEmailCall(accountId: string) {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const epoch = useRef(0);
  const pending = useRef<AbortController | null>(null);
  useEffect(() => {
    epoch.current++; setBusy(false); setError("");
    return () => { epoch.current++; pending.current?.abort(); pending.current = null; };
  }, [connection, accountId]);
  const call = useCallback(async (action: string, options: Record<string, unknown> = {}, sessionId = ""): Promise<AzureEmailReply | null> => {
    if (!connection || pending.current) return null;
    const generation = epoch.current, request = new AbortController();
    pending.current = request; setBusy(true); setError("");
    try {
      const ask = () => connection.query.emailAzureSetup({ accountId, action, options, sessionId }, { signal: request.signal });
      let response: Awaited<ReturnType<typeof ask>>;
      for (let attempt = 0, waited = 0;; attempt++) {
        try { response = await ask(); break; }
        catch (reason) {
          // A dropped connection is not the step's failure. Resumable steps are asked again
          // with a short backoff, and the person sees only a plain sentence if it never returns.
          if (!isTransportInterruption(reason) || request.signal.aborted) throw reason;
          const wait = RETRY_BACKOFF_MS[attempt] ?? RETRY_EVERY_MS;
          if (!RESUMABLE.has(action) || waited + wait > RETRY_FOR_MS) throw Error(droppedSentence(action));
          waited += wait;
          await pause(wait, request.signal);
          if (generation !== epoch.current) return null;
        }
      }
      if (generation !== epoch.current) return null;
      const row = response.rows()[0];
      const reply = row ? flatten(row) as unknown as AzureEmailReply : null;
      if (!reply || typeof reply.status !== "string") throw Error("The cluster did not confirm this setup step.");
      return reply;
    } catch (reason) {
      if (generation === epoch.current) setError(reason instanceof Error ? reason.message : String(reason));
      return null;
    } finally {
      if (pending.current === request) pending.current = null;
      if (generation === epoch.current) setBusy(false);
    }
  }, [connection, accountId]);
  return { call, busy, error };
}

import { useCallback, useEffect, useRef, useState } from "react";
import { isTransportInterruption } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../chrome/access";
import { canConfigure } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";

export interface AzureEmailScope {
  subscriptionId: string; resourceGroup: string; dataLocation: string;
  createResourceGroup?: boolean; resourceGroupLocation?: string;
}
export interface AzureEmailPlan extends AzureEmailScope {
  domain: string; emailService: string; communicationService: string;
  /** Azure's own domain for the installation, whose one sender Azure made. */
  managedDomain?: boolean;
}
export interface AzureEmailReply {
  requestedSender?: { username: string; displayName: string; replyTo?: string };
  /** Every sender Azure has registered on the domain: address -> the display name Azure holds. */
  senders?: Record<string, string>; tenantId?: string;
  /** Where replies to each sender's mail go: address -> reply address. Absent when not known. */
  replies?: Record<string, string>;
  status: string; applicationReady?: boolean; capture?: boolean;
  sessionId?: string; userCode?: string; verificationUri?: string; interval?: number;
  subscriptionId?: string; resourceGroup?: string; dataLocation?: string;
  createResourceGroup?: boolean; resourceGroupLocation?: string;
  choices?: { id: string; name: string }[];
  /** `sender` is the default: the address mail that names none goes out as. */
  plan?: AzureEmailPlan; planId?: string; sender?: string; replyTo?: string;
  records?: { purpose: string; name: string; type: string; value: string; status: string }[];
  deliveryFeedback?: "not-configured" | "pending" | "ready";
  operations?: { operationId: string; status: string; detail: string; submittedAt: string; checkedAt?: string; deliveryStatus?: string; deliveryAt?: string }[];
}
export const AZURE_EMAIL_LOCATIONS = ["Africa", "Asia Pacific", "Australia", "Brazil", "Canada", "Europe", "France", "Germany", "India", "Japan", "Korea", "Norway", "Switzerland", "United Arab Emirates", "United Kingdom", "United States"];

/**
 * Whether senders are added on this organization's Azure domain: it is sending,
 * and it is the organization's own domain. Azure's own domain for the
 * installation has the one sender Azure made, and takes no others.
 */
export function addsSendersOnAzure(state: AzureEmailReply | null | undefined): boolean {
  return state?.status === "ready" && !!state.plan?.domain && !state.plan.managedDomain;
}

/**
 * The addresses an organization's mail can go out as, when Azure sends it: what
 * Azure has registered, lowercased. Null when that is not known or does not
 * apply -- the domain is not sending yet, or there is none -- so nothing is
 * flagged on a guess.
 */
export function azureRegisteredSenders(state: AzureEmailReply | null | undefined): ReadonlySet<string> | null {
  if (state?.status !== "ready" || !state.senders) return null;
  return new Set(Object.keys(state.senders).map(address => address.trim().toLowerCase()));
}

/** Whether Azure, sending an organization's mail, does not have this address. */
export function notRegisteredWithAzure(address: string, registered: ReadonlySet<string> | null): boolean {
  return registered !== null && !registered.has(address.trim().toLowerCase());
}

/**
 * Steps the engine saves and resumes, so asking again after a dropped
 * connection is safe: the page keeps asking by itself for a short while.
 */
const RESUMABLE = new Set(["provision", "verify", "sender", "makeDefault", "domainStatus", "status", "clusterStatus"]);
const RETRY_BACKOFF_MS = [1000, 2000, 4000, 8000];
const RETRY_EVERY_MS = 10_000;
const RETRY_FOR_MS = 120_000;
const WORDS: Record<string, string> = { provision: "Continue setup", verify: "Check domain records", sender: "Save sender", makeDefault: "Make default" };

/**
 * What a person reads when the connection dropped, never the transport's words.
 * `act` is the label of the act that asked, when a page's own words differ.
 */
export function droppedSentence(action: string, act = WORDS[action]): string {
  return act ? `The connection to MemQL dropped. ${act} picks up where it left off.` : "The connection to MemQL dropped. Try again.";
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
  // The timed re-check in flight, if any. A request the person makes ends it first.
  const quietRead = useRef<AbortController | null>(null);
  useEffect(() => {
    epoch.current++; setBusy(false); setError("");
    return () => { epoch.current++; pending.current?.abort(); pending.current = null; quietRead.current?.abort(); quietRead.current = null; };
  }, [connection, accountId]);
  /** Stop whatever is being asked, retries included, and let the next request start. */
  const cancel = useCallback(() => {
    if (!pending.current) return;
    epoch.current++; pending.current.abort(); pending.current = null; setBusy(false);
  }, []);
  const call = useCallback(async (action: string, options: Record<string, unknown> = {}, sessionId = "", act?: string): Promise<AzureEmailReply | null> => {
    if (!connection || pending.current) return null;
    quietRead.current?.abort();
    const generation = epoch.current, request = new AbortController();
    pending.current = request; setBusy(true); setError("");
    try {
      const ask = () => connection.query.emailAzureSetup({ accountId, action, options, sessionId }, { signal: request.signal });
      let response: Awaited<ReturnType<typeof ask>>;
      const began = Date.now();
      for (let attempt = 0;; attempt++) {
        try { response = await ask(); break; }
        catch (reason) {
          // A dropped connection is not the step's failure. Resumable steps are asked again
          // with a short backoff, and the person sees only a plain sentence if it never returns.
          if (!isTransportInterruption(reason) || request.signal.aborted) throw reason;
          const wait = RETRY_BACKOFF_MS[attempt] ?? RETRY_EVERY_MS;
          if (!RESUMABLE.has(action) || Date.now() - began + wait > RETRY_FOR_MS) throw Error(droppedSentence(action, act));
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
  /**
   * A read that changes nothing on screen but its answer: no busy state, no error,
   * no retries, and none while a request the person made is in flight. A request
   * the person makes while it runs ends it, so the two never run together. For timed
   * re-checks, which must not disable or flicker anything. A failure answers null;
   * the next tick asks again.
   */
  const quiet = useCallback(async (action: string): Promise<AzureEmailReply | null> => {
    if (!connection || pending.current || quietRead.current) return null;
    const generation = epoch.current, read = new AbortController();
    quietRead.current = read;
    try {
      const row = (await connection.query.emailAzureSetup({ accountId, action, options: {}, sessionId: "" }, { signal: read.signal })).rows()[0];
      const reply = row ? flatten(row) as unknown as AzureEmailReply : null;
      return generation === epoch.current && !read.signal.aborted && reply && typeof reply.status === "string" ? reply : null;
    } catch { return null; }
    finally { if (quietRead.current === read) quietRead.current = null; }
  }, [connection, accountId]);
  return { call, quiet, cancel, busy, error };
}

/**
 * What an organization's Azure domain can send as, for a page that checks a
 * sender before it is used (an automation before it is turned on). Read quietly:
 * nothing on the page waits or flickers for it. `reload` reads it again, for the
 * page's AutoRefresh, so a sender added on the domain while the page was parked
 * stops being flagged.
 *
 * `registered` is null when that is not known or does not apply -- the domain is
 * not sending, the read failed, or this person cannot read sending domains, as
 * only an owner or a developer can, and they are not asked -- because a sender
 * is never flagged on a guess. `settled` says the first answer is in, whatever
 * it was; a later read that fails takes back nothing already known.
 */
export function useAzureRegisteredSenders(accountId: string): { registered: ReadonlySet<string> | null; settled: boolean; reload: () => Promise<void> } {
  const { access } = useSession();
  const reads = canConfigure(access?.role || "") && accountId !== "";
  const { quiet } = useAzureEmailCall(accountId);
  const [answer, setAnswer] = useState<{ accountId: string; registered: ReadonlySet<string> | null } | null>(null);
  // The organization being asked about now: a late answer about another is dropped.
  const asking = useRef(accountId);
  useEffect(() => { asking.current = accountId; }, [accountId]);
  // One read at a time, so a reload while one is out is not taken for a failure.
  const reading = useRef("");
  const reload = useCallback(async () => {
    if (!reads || reading.current === accountId) return;
    reading.current = accountId;
    try {
      const reply = await quiet("status");
      if (asking.current !== accountId) return;
      setAnswer(known => reply === null && known?.accountId === accountId ? known : { accountId, registered: azureRegisteredSenders(reply) });
    } finally {
      if (reading.current === accountId) reading.current = "";
    }
  }, [reads, quiet, accountId]);
  useEffect(() => { void reload(); }, [reload]);
  if (!reads) return { registered: null, settled: true, reload };
  return answer?.accountId === accountId ? { registered: answer.registered, settled: true, reload } : { registered: null, settled: false, reload };
}

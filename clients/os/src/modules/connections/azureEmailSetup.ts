import { useCallback, useEffect, useRef, useState } from "react";
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
  status: string; applicationReady?: boolean; capture?: boolean;
  sessionId?: string; userCode?: string; verificationUri?: string; interval?: number;
  subscriptionId?: string; resourceGroup?: string; dataLocation?: string;
  createResourceGroup?: boolean; resourceGroupLocation?: string;
  choices?: { id: string; name: string }[];
  plan?: AzureEmailPlan; planId?: string; sender?: string; replyTo?: string;
  records?: { purpose: string; name: string; type: string; value: string; status: string }[];
  operations?: { operationId: string; status: string; detail: string; submittedAt: string }[];
}
export const AZURE_EMAIL_LOCATIONS = ["Africa", "Asia Pacific", "Australia", "Brazil", "Canada", "Europe", "France", "Germany", "India", "Japan", "Korea", "Norway", "Switzerland", "United Arab Emirates", "United Kingdom", "United States"];

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
      const response = await connection.query.emailAzureSetup({ accountId, action, options, sessionId }, { signal: request.signal });
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

import { useEffect, useState } from "react";
import { rowArray, rowString } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import type { DomainDNSGuidance } from "./domains";

/** A read, scoped to the binding. Ignore late responses after navigation. */
export function useDomainDNSGuidance(domainId: string, enabled: boolean) {
  const connection = useOsConnection();
  const [attempt, setAttempt] = useState(0);
  const [value, setValue] = useState<DomainDNSGuidance | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    setValue(null);
    setError("");
    if (!enabled) return;
    if (!connection?.query) {
      setError("Connect to the cluster to read its DNS targets.");
      return;
    }
    void connection.query.customDomainDNSGuidance({ domainId }).then(result => {
      if (!active) return;
      const row = result.rows()[0] ?? null;
      const edgeHost = rowString(row, "edgeHost");
      const ipv4 = (rowArray(row, "ipv4") ?? []).filter((v): v is string => typeof v === "string" && v !== "");
      const ipv6 = (rowArray(row, "ipv6") ?? []).filter((v): v is string => typeof v === "string" && v !== "");
      if (!edgeHost || (!ipv4.length && !ipv6.length)) {
        setError("The cluster did not return its DNS targets. Try again.");
        return;
      }
      setValue({ edgeHost, ipv4, ipv6 });
    }).catch(reason => {
      if (active) setError(reason instanceof Error ? reason.message : String(reason));
    });
    return () => { active = false; };
  }, [connection, domainId, enabled, attempt]);
  return { value, error, retry: () => setAttempt(v => v + 1) };
}

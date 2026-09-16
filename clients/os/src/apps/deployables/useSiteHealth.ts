import { useEffect, useMemo, useState } from "react";
import { rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";
import { useOsConnection } from "../../live/connection";
import type { SiteRow } from "./rows";
import type { SiteHealth } from "./health";

// One window-level observation read, shared by list, map and detail. No browser
// HTTP probe, and no new site-history or arrival cue for a monitor heartbeat.
export function useSiteHealth(sites: readonly SiteRow[], enabled: boolean): SiteRow[] {
  const connection = useOsConnection();
  const [health, setHealth] = useState<Map<string, SiteHealth>>(new Map());
  const [tick, setTick] = useState(0);
  const key = JSON.stringify(sites.filter(s => s.status === "live").map(s => [s.id, s.hostname, s.bundleRef]).sort());
  useEffect(() => {
    if (!enabled) return;
    setTick(v => v + 1);
    const timer = setInterval(() => setTick(v => v + 1), 30_000);
    return () => clearInterval(timer);
  }, [enabled]);
  useEffect(() => {
    if (!enabled || !connection) return;
    let stopped = false;
    let running = false;
    let controller: AbortController | undefined;
    const ids = (JSON.parse(key) as string[][]).map(s => s[0]!);
    const read = async () => {
      if (running) return;
      running = true;
      controller = new AbortController();
      const timeout = setTimeout(() => controller?.abort(), 10_000);
      try {
        const next = new Map<string, SiteHealth>();
        for (let offset = 0; offset < ids.length; offset += 200) {
          const result = await connection.query.executeNamed("siteHealthRead", `builtin siteHealthRead(siteIds: ${JSON.stringify(ids.slice(offset, offset + 200))})`, {signal:controller.signal});
          for (const row of result.rows()) {
            const id = rowString(row, "siteId");
            if (!id) continue;
            next.set(id, {siteId:id, hostname:rowString(row,"hostname"), bundleRef:rowString(row,"bundleRef"), checkedAt:rowString(row,"checkedAt"), state:rowString(row,"state"), reason:rowString(row,"reason"), httpStatus:rowNumber(row,"httpStatus")});
          }
        }
        if (!stopped) setHealth(next);
      } catch {
        if (!stopped) setHealth(new Map());
      } finally { clearTimeout(timeout); running = false; }
    };
    void read();
    const timer = setInterval(() => void read(), 30_000);
    return () => { stopped = true; controller?.abort(); clearInterval(timer); };
  }, [connection, key, enabled]);
  return useMemo(() => sites.map(site => ({...site, health:health.get(site.id.replace(/^v1:platform:site:/, ""))})), [sites, health, tick]);
}

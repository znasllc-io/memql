import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../chrome/access";
import { useConnectionStatus } from "../../chrome/connection";
import { useOsConnection } from "../../live/connection";
import { AutoRefresh, Button, Caption, ContentSkeleton, Fact, Facts, Notice, Panel, Subhead, useAppReach } from "../../kit";
import { formatMoment } from "../../kit/format";
import { useNow } from "../../kit/useNow";
import { percentUsed, readStorageReport, reportFresh, storageWarning, warningSentence, type StorageReport } from "./model";
import "./storage.css";

type StorageQuery = Pick<QueryClient, "clusterStorageStatus">;
interface Reading { report: StorageReport | null; warning: StorageReport | null; error: string; fresh: boolean; busy: boolean }
const Ctx = createContext<Reading | null>(null);

export function StorageHealthProvider({ children }: { children: ReactNode }) {
  const { access } = useSession();
  const connection = useOsConnection();
  const status = useConnectionStatus();
  if (access?.role !== "owner") return <>{children}</>;
  return <StorageHealthFeed key={access.userId} query={connection?.query ?? null} connected={status === "connected"}>{children}</StorageHealthFeed>;
}

export function StorageHealthFeed({ query, connected, children }: { query: StorageQuery | null; connected: boolean; children: ReactNode }) {
  const [revision, reread] = useState(0);
  const [snapshot, setSnapshot] = useState<{ source: StorageQuery | null; report: StorageReport | null; warning: StorageReport | null; error: string; busy: boolean }>({ source: query, report: null, warning: null, error: "", busy: false });
  const now = useNow(30_000).getTime();
  useEffect(() => {
    if (!query || !connected) return;
    let live = true;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10_000);
    setSnapshot((old) => ({ source: query, report: old.source === query ? old.report : null, warning: old.source === query ? old.warning : null, error: "", busy: true }));
    query.clusterStorageStatus({}, { signal: controller.signal }).then((result) => {
      if (!live) return;
      if (controller.signal.aborted) throw new Error("The storage report timed out.");
      const report = readStorageReport(result.single());
      if (!reportFresh(report, Date.now())) throw new Error("The storage report is out of date.");
      setSnapshot({ source: query, report, warning: storageWarning(report) ? report : null, error: "", busy: false });
    }).catch((error: unknown) => {
      if (live) setSnapshot((old) => ({ source: query, report: null, warning: old.source === query ? old.warning : null, error: controller.signal.aborted ? "The storage report timed out." : error instanceof Error ? error.message : String(error), busy: false }));
    }).finally(() => clearTimeout(timer));
    return () => { live = false; clearTimeout(timer); controller.abort(); };
  }, [query, connected, revision]);
  const same = snapshot.source === query;
  const report = same ? snapshot.report : null;
  const value: Reading = { report, warning: same ? snapshot.warning : null, fresh: connected && report !== null && reportFresh(report, now), busy: connected && snapshot.busy, error: !connected ? "Storage measurements will update when the cluster reconnects." : same ? snapshot.error : "" };
  return <Ctx.Provider value={value}><AutoRefresh intervalMs={60_000} enabled={connected && query !== null} busy={value.busy} onRefresh={() => reread((n) => n + 1)} />{children}</Ctx.Provider>;
}

export function StorageWarning() {
  const reading = useContext(Ctx);
  const cluster = useAppReach("cluster");
  if (!reading?.warning) return null;
  return <div className="os-storage-warning" role="status">
    <span>{reading.fresh ? warningSentence(reading.warning) : "Storage was nearly full. A current reading is unavailable."}</span>
    {cluster.sections.includes("readiness") ? <Button onClick={() => cluster.open("readiness")}>Review storage</Button> : null}
  </div>;
}

const bytes = (value: number) => `${(value / 1024 ** 3).toLocaleString(undefined, { maximumFractionDigits: 1 })} GiB`;
export function StoragePanel() {
  const reading = useContext(Ctx);
  if (!reading) return null;
  const report = reading.fresh ? reading.report : null;
  return <Panel label="Storage"><Subhead>Storage</Subhead>
    {report ? <>
      {reading.warning ? <Notice tone="warn" sentence={warningSentence(reading.warning)} next="Increase capacity or review retention before writes run out of space." /> : null}
      <Facts>
        <Fact label="Database" value={`${percentUsed(report.dataBytes, report.dataCapacityBytes)} · ${bytes(report.dataBytes)} of ${bytes(report.dataCapacityBytes)}`} />
        <Fact label="Recovery log" value={`${percentUsed(report.walBytes, report.walCapacityBytes)} · ${bytes(report.walBytes)} of ${bytes(report.walCapacityBytes)}`} />
        <Fact label="Measured" value={formatMoment(new Date(report.observedAt).toISOString())} />
      </Facts>
      <Caption>Database usage is estimated from database sizes and configured capacity.</Caption>
    </> : reading.busy && !reading.error ? <ContentSkeleton kind="detail" label="Loading storage measurements" /> : <Notice sentence={reading.error || "Storage measurements are out of date."} next="Check the database monitor. Measurements update automatically." />}
    {!report && reading.warning ? <Notice tone="warn" sentence="The last storage report warned that capacity was nearly full." next="Check capacity before resuming heavy writes." /> : null}
  </Panel>;
}

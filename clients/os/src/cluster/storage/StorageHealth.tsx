import { useOsIfPresent } from "../../chrome/state";
import { createContext, useContext, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
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
    if (!query || !connected || document.visibilityState === "hidden" || navigator.onLine === false) return;
    let live = true;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10_000);
    setSnapshot((old) => ({ source: query, report: old.source === query ? old.report : null, warning: old.source === query ? old.warning : null, error: "", busy: true }));
    let abortRead!: () => void;
    const cancelled = new Promise<never>((_, reject) => {
      abortRead = () => reject(new Error("The storage report timed out."));
      controller.signal.addEventListener("abort", abortRead, { once: true });
    });
    Promise.race([query.clusterStorageStatus({}, { signal: controller.signal }), cancelled]).then((result) => {
      if (!live) return;
      if (controller.signal.aborted) throw new Error("The storage report timed out.");
      const report = readStorageReport(result.single());
      if (!reportFresh(report, Date.now())) throw new Error("The storage report is out of date.");
      setSnapshot({ source: query, report, warning: storageWarning(report) ? report : null, error: "", busy: false });
    }).catch((error: unknown) => {
      if (live) setSnapshot((old) => ({ source: query, report: null, warning: old.source === query ? old.warning : null, error: controller.signal.aborted ? "The storage report timed out." : error instanceof Error ? error.message : String(error), busy: false }));
    }).finally(() => { clearTimeout(timer); controller.signal.removeEventListener("abort", abortRead); });
    return () => { live = false; clearTimeout(timer); controller.abort(); };
  }, [query, connected, revision]);
  const same = snapshot.source === query;
  const report = same ? snapshot.report : null;
  const value: Reading = { report, warning: same ? snapshot.warning : null, fresh: connected && report !== null && reportFresh(report, now), busy: connected && snapshot.busy, error: !connected ? "Storage measurements will update when the cluster reconnects." : same ? snapshot.error : "" };
  return <Ctx.Provider value={value}><AutoRefresh intervalMs={60_000} enabled={connected && query !== null} busy={value.busy} onRefresh={() => reread((n) => n + 1)} />{children}</Ctx.Provider>;
}

export function StorageWarning({ onReview, inline = false, onHeightChange }: { onReview?: () => void; inline?: boolean; onHeightChange?: (height: number) => void }) {
  const reading = useContext(Ctx);
  const cluster = useAppReach("cluster");
  const os = useOsIfPresent();
  const desk = os?.state.shell.desks.find((item) => item.id === os.state.shell.activeDeskId);
  const reviewing = !inline && desk?.windows.some((id) => {
    const win = os?.state.shell.windows[id];
    return win?.appId === "cluster" && win.sectionId === "readiness" && win.mode !== "minimized";
  });
  const element = useRef<HTMLDivElement>(null);
  const present = !!reading?.warning && !reviewing;
  useLayoutEffect(() => {
    const update = () => onHeightChange?.(element.current?.getBoundingClientRect().height ?? 0);
    update();
    const observer = new ResizeObserver(update);
    if (element.current) observer.observe(element.current);
    return () => { observer.disconnect(); onHeightChange?.(0); };
  }, [present, onHeightChange]);
  if (!reading?.warning || reviewing) return null;
  return <div ref={element} className="os-storage-warning" data-inline={inline || undefined} role="status">
    <span>{reading.fresh ? warningSentence(reading.warning) : "Storage was nearly full. A current reading is unavailable."}</span>
    {onReview || (cluster.canOpenWindows && cluster.sections.includes("readiness")) ? <Button onClick={onReview ?? (() => cluster.open("readiness", { storageWarning: true }))}>Review storage</Button> : null}
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

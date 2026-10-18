import { useEffect, useRef, useState } from "react";
import { useSession } from "../../../chrome/access";
import { useConnectionStatus } from "../../../chrome/connection";
import { useOsConnection } from "../../../live/connection";
import { AutoRefresh, Button, ContentSkeleton, Details, DetailsSection, Fact, Facts, Head, Input, Notice, SettingRow, Switch } from "../../../kit";
import { LocalTabs } from "../../../kit/LocalTabs";
import { PageHelp } from "../../../kit/pageHelp";
import { formatMoment } from "../../../kit/format";
import { cores, gib, readCapacity, type Capacity } from "./model";
import { useCapacity, type CapacityQueries } from "./useCapacity";

export function CapacitySection({ visible = true }: { visible?: boolean }) {
  const { access } = useSession();
  const connection = useOsConnection();
  const status = useConnectionStatus();
  if (access?.role !== "owner") return <div className="os-cluster"><Head title="Capacity" /><Notice sentence="A cluster owner can manage background capacity." /></div>;
  return <CapacityPage key={access.userId} query={connection?.query ?? null} connected={status === "connected"} visible={visible} />;
}

export function CapacityPage({ query, connected, visible }: { query: CapacityQueries | null; connected: boolean; visible: boolean }) {
  const reading = useCapacity(query, connected && visible);
  const [tab, setTab] = useState<"overview" | "settings">("overview");
  const report = reading.value;
  return <div className="os-cluster">
    <Head title="Capacity" />
    <LocalTabs label="Capacity views" value={tab} onChange={setTab} options={[["overview", "Overview"], ["settings", "Settings"]]} />
    <AutoRefresh enabled={connected && visible && query !== null} busy={reading.busy} onRefresh={reading.refresh} intervalMs={20_000} />
    {!connected ? <Notice sentence="Capacity will update when the cluster reconnects." /> : null}
    {reading.error ? <Notice tone="error" sentence="Capacity could not be refreshed." next="Try refreshing when the cluster is available." /> : null}
    {!report && connected && !reading.error ? <ContentSkeleton kind="detail" label="Loading capacity" /> : null}
    {report && !report.available ? <Notice sentence={report.reason || "Background resource admission is unavailable."} /> : null}
    {report?.available && tab === "overview" ? <Details>
      <DetailsSection title="Pipelines">
        <Facts>
          <Fact label="New work" value={report.accepting ? "Accepted" : "Paused"} />
          <Fact label="Running" value={report.running === undefined ? "Not reported" : String(report.running)} />
          <Fact label="Waiting" value={report.waiting === undefined ? "Not reported" : String(report.waiting)} />
        </Facts>
      </DetailsSection>
      <DetailsSection title="Resource budget">
        <PageHelp title="Resource budget"><p>Reservations cover each job and its supporting containers. Work waits until its whole reservation fits. CPU may burst within the dedicated machine; memory and temporary disk remain bounded. These figures are reservations, not measured usage.</p></PageHelp>
        <Facts>
          <Fact label="Processing" value={`${report.reserved ? cores(report.reserved.cpuMilli) : "Not reported"} / ${cores(report.budget!.cpuMilli)} cores`} />
          <Fact label="Memory" value={`${report.reserved ? gib(report.reserved.memoryMiB) : "Not reported"} / ${gib(report.budget!.memoryMiB)}`} />
          <Fact label="Temporary disk" value={`${report.reserved ? gib(report.reserved.diskMiB) : "Not reported"} / ${gib(report.budget!.diskMiB)}`} />
          <Fact label="Concurrent jobs" value={`Up to ${report.budget!.jobs}, when resources fit`} />
          <Fact label="Checked" value={formatMoment(report.observedAt)} />
        </Facts>
      </DetailsSection>
      <DetailsSection title="Protection">
        <p className="os-caption">This budget is restricted to the dedicated background pool. Customer services and database capacity remain outside it.</p>
        <p className="os-caption">Changing the budget does not add machines or resize them. More work can increase running hours and usage charges.</p>
        <p className="os-caption">Workbench commands have a separate execution limit. This budget currently covers pipeline jobs.</p>
      </DetailsSection>
    </Details> : null}
    {report?.available && tab === "settings" ? <CapacitySettings query={query} report={report} fresh={reading.fresh} refresh={reading.refresh} /> : null}
  </div>;
}

function CapacitySettings({ query, report, fresh, refresh }: { query: CapacityQueries | null; report: Capacity; fresh: boolean; refresh: () => void }) {
  const [base, setBase] = useState(report);
  const [cpu, setCpu] = useState(String(report.budget!.cpuMilli / 1000));
  const [memory, setMemory] = useState(String(report.budget!.memoryMiB / 1024));
  const [jobs, setJobs] = useState(String(report.budget!.jobs));
  const [accepting, setAccepting] = useState(report.accepting);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const inflight = useRef(false);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const changed = cpu !== String(base.budget!.cpuMilli / 1000) || memory !== String(base.budget!.memoryMiB / 1024) || jobs !== String(base.budget!.jobs) || accepting !== base.accepting;
  const stale = base.revision !== report.revision || base.policyRevision !== report.policyRevision;
  const reset = () => { setBase(report); setCpu(String(report.budget!.cpuMilli / 1000)); setMemory(String(report.budget!.memoryMiB / 1024)); setJobs(String(report.budget!.jobs)); setAccepting(report.accepting); setError(""); setMessage(""); };
  async function save() {
    if (inflight.current || !query || !fresh || stale) return;
    const values = [Number(cpu) * 1000, Number(memory) * 1024, Number(jobs)];
    if ([cpu, memory, jobs].some(v => !/^\d+(?:\.\d+)?$/.test(v.trim())) || values.some(v => !Number.isSafeInteger(v) || v < 1) || values[0]! > report.ceiling!.cpuMilli || values[1]! > report.ceiling!.memoryMiB || values[2]! > report.ceiling!.jobs) { setError("Enter positive values within the limits shown below."); return; }
    inflight.current = true; setBusy(true); setError(""); setMessage("");
    try {
      const result = readCapacity((await query.clusterSetBackgroundCapacity({ revision: base.revision!, policyRevision: base.policyRevision!, cpuMilli: values[0]!, memoryMiB: values[1]!, jobs: values[2]!, accepting })).single());
      if (!result.available || result.budget?.cpuMilli !== values[0] || result.budget?.memoryMiB !== values[1] || result.budget?.jobs !== values[2] || result.accepting !== accepting) throw new Error("The requested settings could not be verified. Check the current values before saving again.");
      if (alive.current) { setBase(result); setMessage("Capacity settings saved."); }
    } catch { if (alive.current) setError("The change could not be confirmed. Check the current values before saving again."); }
    finally { inflight.current = false; if (alive.current) { setBusy(false); refresh(); } }
  }
  return <Details>
    <DetailsSection title="Pipelines">
      <SettingRow label="Admission"><Switch checked={accepting} onChange={setAccepting} disabled={busy}>Accept new work</Switch><p className="os-caption">Pausing lets running jobs finish.</p></SettingRow>
      <SettingRow label="Processing"><Input id="capacity-cpu" label="Processing cores" value={cpu} onChange={setCpu} disabled={busy} /><p className="os-caption">Up to {cores(report.ceiling!.cpuMilli)} cores.</p></SettingRow>
      <SettingRow label="Memory"><Input id="capacity-memory" label="Memory in GiB" value={memory} onChange={setMemory} disabled={busy} /><p className="os-caption">Up to {gib(report.ceiling!.memoryMiB)}.</p></SettingRow>
      <SettingRow label="Concurrent jobs"><Input id="capacity-jobs" label="Maximum concurrent jobs" value={jobs} onChange={setJobs} disabled={busy} /><p className="os-caption">Up to {report.ceiling!.jobs}. Each job also needs to fit the resource budget.</p></SettingRow>
      <p className="os-caption">Smaller budgets can delay larger builds. Existing jobs finish before a reduced budget admits more work.</p>
      {stale ? <Notice sentence="Capacity settings have changed." next="Load the current values before saving."><Button onClick={reset} disabled={busy}>Load current values</Button></Notice> : null}
      {error ? <Notice tone="error" sentence={error} /> : null}
      {message ? <p role="status" className="os-caption">{message}</p> : null}
      <div className="os-panel-actions"><Button tone="primary" onClick={() => void save()} busy={busy} busyLabel="Saving capacity" disabled={!fresh || stale || !changed}>Save changes</Button>{changed ? <Button onClick={reset} disabled={busy}>Reset</Button> : null}</div>
    </DetailsSection>
    <DetailsSection title="Limits"><p className="os-caption">The operator sets these limits after reserving capacity for system services. Machine count and size are managed separately.</p></DetailsSection>
  </Details>;
}

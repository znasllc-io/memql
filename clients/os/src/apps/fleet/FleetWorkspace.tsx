import { AddButton } from "../../kit/AddButton";
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { ActivityTarget } from "../../kit/SemanticActivity";
import { ArrowUpRight, Box, ChevronRight, Cpu, History, Info, Monitor, SlidersHorizontal, Terminal, Wrench } from "lucide-react";
import type { OsAppProps } from "../../system/registry";
import { Button, EmptyState, Refine, Head, Notice, ProvenanceDot, Select, formatBytes, formatFreshness, useNow } from "../../kit";
import { IconButton } from "../../kit/IconButton";
import { InfoDetail } from "../../kit/InfoDetail";
import { useLiveView } from "../../live/liveView";
import { useMachines } from "../../live/machines";
import { useAppSessions } from "./apps/useAppSessions";
import { RefreshButton, useFleetScroll } from "./FleetControls";
import { AddMachinePage } from "./addMachine/AddMachinePage";
import type { AddMachineFlow } from "./addMachine/useAddMachineFlow";
import { MachineDetail, MachineAppsHelp } from "./machines/MachineDetail";
import { machineModelsFrom, type MachineModel } from "./machines/models";
import { useMachineWrites } from "./machines/useMachineWrites";
import { isWorkerOnline } from "./online";
import { computerUseStatus, isRevoked, machineFromRow, machineName, type MachineRow } from "./rows";

export type MachineView = "equipment" | "details" | "models" | "apps" | "sharing" | "activity";
export interface FleetSelection { machineId: string; view: MachineView }
const VIEW_NAMES: Record<MachineView, string> = { equipment: "Equipment", details: "Details", models: "Models", apps: "Apps", sharing: "Sharing", activity: "History" };

export function FleetWorkspace({ flow, showRevoked, selection, select, navigate, onOpenSession, intent, consumeIntent }: {
  onOpenSession?: (id: string) => void;
  flow: AddMachineFlow; showRevoked: boolean; selection: FleetSelection;
  select: (selection: FleetSelection) => void; navigate: OsAppProps["navigate"];
  intent?: OsAppProps["intent"]; consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const { collection, settled, feedState, reload } = useMachines();
  const sessions = useAppSessions();
  const consumed = useRef("");
  const now = useNow(15_000);
  const [machineSearch, setMachineSearch] = useState("");
  const source = useLiveView<Row, MachineRow>(collection, `revoked:${showRevoked}`, rows => rows.map(machineFromRow).filter(m => m.id && (showRevoked || !isRevoked(m))));
  const subscribe = useCallback((listener: () => void) => source?.subscribe(listener) ?? (() => {}), [source]);
  const snapshot = useSyncExternalStore(subscribe, () => source?.snapshot ?? null, () => null);
  const [removed, setRemoved] = useState<string[]>([]);
  const machines = (snapshot?.rows ?? []).filter(m => showRevoked || !removed.includes(m.id));
  const selectionRef = useRef(selection);
  selectionRef.current = selection;
  const onRemoved = useCallback((id: string) => {
    setRemoved(previous => [...previous, id]);
    if (selectionRef.current.machineId === id) select({ machineId: "", view: "equipment" });
  }, [select]);
  // A disappearing selection returns to equipment, never another machine's editor.
  const machine = selection.machineId ? machines.find(m => m.id === selection.machineId) : machines[0];
  useEffect(() => {
    if (!selection.machineId && machine) select({ machineId: machine.id, view: "equipment" });
    else if (selection.machineId && !machine && settled && feedState === "live" && !snapshot?.error) {
      select({ machineId: "", view: "equipment" });
    }
  }, [selection.machineId, machine?.id, settled, feedState, snapshot?.error, select]);
  const request = intent?.payload.addMachine;
  const wants = typeof request === "object" && request !== null && !Array.isArray(request);
  useEffect(() => {
    if (!intent || !wants || consumed.current === intent.id) return;
    consumed.current = intent.id;
    if (!flow.active) flow.start({ inference: (request as { inference?: unknown }).inference === true });
    consumeIntent?.(intent.id);
  }, [intent, wants, request, flow.active, flow.start, consumeIntent]);

  const [visited, setVisited] = useState<FleetSelection[]>([]);
  useEffect(() => {
    if (!machine) return;
    setVisited(previous => previous.some(item => item.machineId === machine.id && item.view === selection.view) ? previous : [...previous, { machineId: machine.id, view: selection.view }]);
  }, [machine?.id, selection.view]);
  const retained = machine && !visited.some(item => item.machineId === machine.id && item.view === selection.view) ? [...visited, { machineId: machine.id, view: selection.view }] : visited;
  const scrollRoot = useFleetScroll(`${selection.machineId}:${selection.view}`);
  const behind = feedState === "degraded" || feedState === "disconnected";
  return (<>
    {flow.active ? <AddMachinePage flow={flow} onLeave={machineId => { if (machineId) select({ machineId, view: "equipment" }); }} /> : null}
    <div ref={scrollRoot} className="fleet-home" hidden={flow.active}>
    <div className="fleet-workspace" data-os-page-context={JSON.stringify({ page: "Machines", machineId: machine?.id, machine: machine ? machineName(machine) : undefined, view: VIEW_NAMES[selection.view], search: machineSearch || undefined })}>
      <div className="fleet-workspace-header">
      {/* EQUIPMENT IS A DEPTH TOO. Every other machine view publishes
          "Machines > <machine> > <view>" from the inspector heading below, and
          this one used to publish only its title -- so the trail read
          "Machines" on Equipment and "Machines > local > Details" one click
          away, as though selecting a machine had not happened. The ancestors
          carry no handler here on purpose: both lead to this machine's
          Equipment view, which is where the person already is, and a crumb
          that goes nowhere is not a link. */}
      <Head title="Machines" breadcrumbs={machine ? [{ label: "Machines" }, { label: machineName(machine) }, { label: VIEW_NAMES.equipment }] : undefined} navigation={selection.view === "equipment" || !machine} meta={<span className="fleet-feed-label">{snapshot?.error && machines.length === 0 ? "Unavailable" : settled ? `${machines.length} ${machines.length === 1 ? "machine" : "machines"}` : "Connecting"}</span>}>
        {machines.length ? <Select id="fleet-selected-machine" label="Selected machine" value={machine?.id ?? ""} onChange={id => select({ machineId: id, view: "equipment" })}>
          {!machine ? <option value="">Choose a machine</option> : null}
          {machines.map(m => <option key={m.id} value={m.id}>{machineName(m)} · {isRevoked(m) ? "Revoked" : isWorkerOnline(m, now) ? "Online" : "Offline"}</option>)}
        </Select> : null}
        <Refine iconOnly label="Find machines" placeholder="Search machines" search={machineSearch} onSearch={setMachineSearch}>
          <div className="fleet-machine-results">{machines.filter(m => machineName(m).toLowerCase().includes(machineSearch.toLowerCase())).map(m => <button type="button" className="fleet-reading-link" key={m.id} onClick={() => select({ machineId: m.id, view: "equipment" })}>{machineName(m)}<ChevronRight size={14} aria-hidden /></button>)}
          {!machines.some(m => machineName(m).toLowerCase().includes(machineSearch.toLowerCase())) ? <EmptyState icon={Monitor} title="No matching machines">{machines.length ? "Try another machine name." : "Connect a machine to find it here."}</EmptyState> : null}</div>
        </Refine>
        <AddButton label="Add a machine" onClick={() => flow.start({})} />
      </Head>
      {machine ? <nav className="fleet-local-tabs" aria-label="Machine views">{(Object.keys(VIEW_NAMES) as MachineView[]).map(view => <button key={view} type="button" aria-current={selection.view === view ? "page" : undefined} onClick={() => select({ machineId: machine.id, view })}>{VIEW_NAMES[view]}</button>)}</nav> : null}
      </div>
      {behind ? <Notice tone="warn" sentence="Machine updates are interrupted." next="Showing the last known state." detail={snapshot?.error || undefined}><RefreshButton label="Reconnect machines" onClick={reload} /></Notice> : null}
      <div className="fleet-workspace-body" data-has-machines={machines.length > 0 || undefined}>
        <main className="fleet-equipment-area" aria-label="Machine workspace">
          {!settled && machines.length === 0 ? <div className="fleet-empty"><Monitor size={40} aria-hidden /><h4>Reading your fleet</h4><p>Waiting for the cluster’s machine inventory.</p>{snapshot?.error ? <Notice tone="error" sentence="Your machines could not be read." detail={snapshot.error} /> : null}</div>
            : snapshot?.error && machines.length === 0 ? <EmptyState title="Machines unavailable" action={<RefreshButton label="Retry reading machines" onClick={reload} />}>Reconnect to read your machine inventory. No changes have been made.</EmptyState>
            : machine ? retained.map(item => {
              const retainedMachine = machines.find(value => value.id === item.machineId);
              if (!retainedMachine) return null;
              return <div key={`${item.machineId}:${item.view}`} hidden={item.machineId !== machine.id || item.view !== selection.view}>
                {item.view === "equipment" ? <><MachineEquipment machine={retainedMachine} now={now} onInspect={view => select({ machineId: item.machineId, view })} />
                <section className="fleet-recent-work" aria-label="Recent app sessions"><div className="fleet-bank-heading"><h4>Recent work</h4><span className="fleet-heading-actions"><RefreshButton label="Refresh recent work" busy={sessions.loading} onClick={sessions.reread} /><IconButton label="All app sessions" onClick={() => navigate("apps", { fromContent: true })}><ArrowUpRight size={16} aria-hidden /></IconButton></span></div>
                  {sessions.error ? <Notice sentence="Recent app sessions could not be read." detail={sessions.error} /> : null}
                  {sessions.sessions.filter(session => session.workerId === item.machineId).slice(0, 5).map(session => <button type="button" className="fleet-session-link" key={session.id} onClick={() => onOpenSession?.(session.id)}>{session.app} · {session.runId || session.id}<span>{session.status}</span></button>)}
                  {!sessions.loading && !sessions.error && !sessions.sessions.some(session => session.workerId === item.machineId) ? <EmptyState icon={History} title="No recent work">App sessions will appear here when an app runs on this machine.</EmptyState> : null}
                </section></> : <div className="fleet-inspector">
                  <Head title={VIEW_NAMES[item.view]} breadcrumbs={[{ label: "Machines", onSelect: () => select({ machineId: item.machineId, view: "equipment" }) }, { label: machineName(retainedMachine), onSelect: () => select({ machineId: item.machineId, view: "equipment" }) }, { label: VIEW_NAMES[item.view] }]} back={{ label: machineName(retainedMachine), onSelect: () => select({ machineId: item.machineId, view: "equipment" }) }}>
                    {item.view === "apps" ? <MachineAppsHelp /> : null}
                    {item.view === "apps" || item.view === "activity" ? <IconButton label="App sessions" onClick={() => navigate("apps", { fromContent: true })}><History size={16} aria-hidden /></IconButton> : null}
                  </Head>
                  <ScopedMachineDetail machine={retainedMachine} now={now} view={item.view} onRemoved={onRemoved} />
                </div>}
              </div>;
            })
            : <EmptyEquipment onConnect={() => flow.start({})} showRevoked={showRevoked} />}
        </main>


      </div>
    </div></div>
  </>);
}

function EmptyEquipment({ onConnect, showRevoked }: { onConnect: () => void; showRevoked: boolean }) {
  return <div className="fleet-empty-equipment">
    <div className="fleet-empty-device" aria-hidden><Monitor size={62} strokeWidth={1} /><span className="fleet-device-port" /></div>
    <h4>A place for your machines.</h4>
    <p>Connect a computer to see what it can run and put its capabilities to work.</p>
    <Button tone="primary" onClick={onConnect}>Connect your first machine</Button>
    <div className="fleet-empty-capabilities" aria-label="What a connected machine can offer">
      <div><Cpu size={20} aria-hidden /><strong>Local models</strong><span>See what’s installed and what fits.</span></div>
      <div><Terminal size={20} aria-hidden /><strong>Your apps</strong><span>Use the apps already on your machine.</span></div>
      <div><Wrench size={20} aria-hidden /><strong>Tools & work</strong><span>Inspect capabilities and recent activity.</span></div>
    </div>
    <p className="fleet-empty-footnote">Already connected a machine? Check that you signed in with its owner’s account.{!showRevoked ? " Revoked machines are in Fleet settings." : ""}</p>
  </div>;
}

/** The spatial view is all reported inventory. Selecting equipment inspects
 * it; managing, probing and installing remain distinct, explicit actions. */
export function MachineEquipment({ machine, now, onInspect }: { machine: MachineRow; now: Date; onInspect: (view: MachineView) => void }) {
  const [chosen, choose] = useState<{ kind: "model" | "app"; id: string } | null>(null);
  const models = machineModelsFrom(machine.reportedLabels);
  const online = isWorkerOnline(machine, now);
  const revoked = isRevoked(machine);
  const chosenModel = chosen?.kind === "model" ? models.find(m => m.modelId === chosen.id) : undefined;
  const chosenApp = chosen?.kind === "app" ? machine.apps.find(a => a.id === chosen.id) : undefined;
  const selectedModel = chosenModel ?? (!chosenApp ? models[0] : undefined);
  const selectedApp = chosenApp ?? (!selectedModel ? machine.apps[0] : undefined);
  const hw = machine.hardware;
  const desktop = computerUseStatus(machine);
  return <ActivityTarget target={`fleet:machine:${machine.id}`} className="fleet-equipment">
    <header className="fleet-machine-heading"><div><h4>{machineName(machine)}</h4><span className="fleet-machine-presence"><ProvenanceDot tone={online ? "reachable" : "unreachable"} />{revoked ? "Revoked" : online ? "Online" : "Offline"}<span>{formatFreshness(machine.lastSeenAt, now)}</span></span></div><IconButton label="Machine details" onClick={() => onInspect("details")}><Info size={16} aria-hidden /></IconButton></header>
    <div className="fleet-equipment-rig">
      <section className="fleet-equipment-bank" aria-label="Local model equipment"><div className="fleet-bank-heading"><Cpu size={16} aria-hidden /><h5>Local models</h5><span>{models.length}</span><IconButton label={models.length ? "Manage models" : "Find models that fit"} onClick={() => onInspect("models")}><SlidersHorizontal size={16} aria-hidden /></IconButton></div>
        <ul>{models.map(model => <li key={model.modelId}><button className="fleet-equipment-slot" type="button" aria-pressed={selectedModel?.modelId === model.modelId} onClick={() => choose({ kind: "model", id: model.modelId })}><Box size={17} aria-hidden /><span><strong>{model.modelId}</strong><small>{model.embeddings ? "Embeddings" : model.tools ? "Chat & tools" : "Chat"}</small></span></button></li>)}</ul>
        {models.length === 0 ? <EmptyState icon={Cpu} title="No local models">Find a model that fits this machine in Models.</EmptyState> : null}
      </section>
      <div className="fleet-device" aria-label="Machine hardware">
        <div className="fleet-device-screen"><Monitor size={35} strokeWidth={1.2} aria-hidden /><strong>{hw.chip || machine.platform || "Hardware not reported"}</strong><dl><div><dt>Memory</dt><dd>{hw.memoryBytes > 0 ? formatBytes(hw.memoryBytes) : "Unknown"}</dd></div><div><dt>CPU cores</dt><dd>{hw.cpuCores || "Unknown"}</dd></div></dl></div><div className="fleet-device-base" aria-hidden />
        <div className="fleet-device-work"><span>{machine.activeCount} active {machine.activeCount === 1 ? "call" : "calls"}</span><InfoDetail title="Active work"><p>Reported at the last heartbeat. A newly started call can take a few seconds to appear.</p></InfoDetail><IconButton label="Call history" onClick={() => onInspect("activity")}><History size={16} aria-hidden /></IconButton></div>
      </div>
      <section className="fleet-equipment-bank" aria-label="App equipment"><div className="fleet-bank-heading"><Terminal size={16} aria-hidden /><h5>Installed apps</h5><span>{machine.apps.length}</span><IconButton label="Inspect apps" onClick={() => onInspect("apps")}><ArrowUpRight size={16} aria-hidden /></IconButton></div>
        <ul>{machine.apps.map(app => <li key={app.id}><button className="fleet-equipment-slot" type="button" aria-pressed={selectedApp?.id === app.id} onClick={() => choose({ kind: "app", id: app.id })}><Terminal size={17} aria-hidden /><span><strong>{app.label}</strong><small>{!online ? "Machine offline" : app.runnable ? "Ready" : "Needs attention"}</small></span></button></li>)}</ul>
        {machine.apps.length === 0 ? <EmptyState icon={Terminal} title="No installed apps">Apps reported by this machine will appear here.</EmptyState> : null}
      </section>
    </div>
    {selectedModel || selectedApp ? <section className="fleet-equipment-reading" aria-label="Selected equipment" aria-live="polite" data-os-page-context={JSON.stringify({ selectedModel: selectedModel?.modelId, selectedApp: selectedApp?.id })}>
      {selectedModel ? <div><strong>{selectedModel.modelId}</strong><p>{modelSummary(selectedModel)}{!online ? " · Machine offline" : ""}</p></div>
        : selectedApp ? <div><strong>{selectedApp.label}</strong><p>{selectedApp.why || (selectedApp.runnable ? "Available to run on this machine." : "Not available to run.")}{!online ? " Machine offline." : ""}</p></div> : null}
    </section> : null}
    <div className="fleet-machine-tools"><div><Wrench size={16} aria-hidden /><strong>Tools</strong><span>{machine.capabilities.filter(c => c !== "MODEL").map(c => c === "HEADLESS" ? "Terminal" : c === "COMPUTERUSE" ? "Computer use" : c).join(", ") || "None reported"}</span><InfoDetail title="Machine capabilities"><p>{desktop.answer}</p><p>Capabilities are reported by this machine. Connecting it does not grant missing operating-system permissions.</p></InfoDetail></div><button type="button" className="fleet-reading-link" onClick={() => onInspect("sharing")}>{machine.sharingMode === "cluster" && machine.inferenceServe === "cluster" ? "Shared inference" : "Personal inference"}<ChevronRight size={14} aria-hidden /></button></div>
  </ActivityTarget>;
}

function modelSummary(model: MachineModel): string {
  return [model.contextWindow ? `${model.contextWindow.toLocaleString()} context` : "Context not reported", model.quant, model.tools ? "Tool calling" : "", model.structuredOutput ? "Structured output" : ""].filter(Boolean).join(" · ");
}

function ScopedMachineDetail({ machine, now, view, onRemoved }: { machine: MachineRow; now: Date; view: Exclude<MachineView, "equipment">; onRemoved: (id: string) => void }) {
  const writes = useMachineWrites();
  const revoke = async (id: string, reason: string) => {
    const ok = await writes.revoke(id, reason);
    if (ok) onRemoved(id);
    return ok;
  };
  return <MachineDetail machine={machine} writes={{ ...writes, revoke }} now={now} view={view} />;
}

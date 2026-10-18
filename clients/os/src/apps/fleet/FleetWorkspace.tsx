import { Fact, Facts } from "../../kit";
import { machineFeatureStates, machineAppAvailability } from "./machines/features";
import { LocalTabs } from "../../kit/LocalTabs";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { AddButton } from "../../kit/AddButton";
import { RecordList, RecordRow, listCount } from "../../kit/RecordRow";
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { ActivityTarget } from "../../kit/SemanticActivity";
import { ArrowUpRight, ChevronRight, Cpu, Monitor, SlidersHorizontal, Terminal, Wrench } from "lucide-react";
import type { OsAppProps } from "../../system/registry";
import { Button, EmptyState, Refine, Head, Notice, ProvenanceDot, formatBytes, formatFreshness, useNow } from "../../kit";
import { IconButton } from "../../kit/IconButton";
import { PageHelp } from "../../kit/pageHelp";
import type { PageTrail } from "../../kit/pageNavigation";
import { useLiveView } from "../../live/liveView";
import { useMachines } from "../../live/machines";
import { AutoRefresh, useFleetScroll } from "./FleetControls";
import { AddMachinePage } from "./addMachine/AddMachinePage";
import type { AddMachineFlow } from "./addMachine/useAddMachineFlow";
import { MachineDetail, MachineAppsHelp } from "./machines/MachineDetail";
import { AppPermissionHelp } from "./machines/AppPermissionHelp";
import { machineModelsFrom, type MachineModel } from "./machines/models";
import { EquipmentModelCarousel } from "./EquipmentModelCarousel";
import { useSession } from "../../chrome/access";
import { machineActivity } from "./machines/activity";
import { ownsMachine } from "./machines/sharing";
import { RecentWork } from "./apps/RecentWork";
import { useMachineWrites } from "./machines/useMachineWrites";
import { isWorkerOnline } from "./online";
import { isRevoked, machineFromRow, machineName, RUNNABLE_APPS, type MachineRow } from "./rows";

export type MachineView = "equipment" | "models" | "apps" | "activity" | "settings";
export interface FleetSelection { machineId: string; view: MachineView }
const VIEW_NAMES: Record<MachineView, string> = { equipment: "Equipment", models: "Models", apps: "Apps", activity: "Activity", settings: "Settings" };

export function FleetWorkspace({ flow, showRevoked, selection, select, navigate, onOpenSession, intent, consumeIntent, onAllSessions }: {
  onOpenSession?: (id: string) => void;
  onAllSessions?: () => void;
  flow: AddMachineFlow; showRevoked: boolean; selection: FleetSelection;
  select: (selection: FleetSelection) => void; navigate: OsAppProps["navigate"];
  intent?: OsAppProps["intent"]; consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const { access } = useSession();
  const { collection, settled, feedState, reload } = useMachines();
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
  // NO MACHINE SELECTED IS THE LIST, NOT "PICK THE FIRST ONE". An empty
  // `machineId` used to select `machines[0]` on sight, so this section was
  // always INSIDE a machine and a dropdown in the Head was the only way to
  // reach another. That is what made "Machines" in the trail a crumb with
  // nowhere to go: there was no machines list for it to name. Now the empty
  // selection is the list, a row opens a machine, and the crumb goes home.
  //
  // A disappearing selection still returns to the list, never to another
  // machine's editor.
  const machine = selection.machineId ? machines.find(m => m.id === selection.machineId) : undefined;
  useEffect(() => {
    if (selection.machineId && !machine && settled && feedState === "live" && !snapshot?.error) {
      select({ machineId: "", view: "equipment" });
    }
  }, [selection.machineId, machine?.id, settled, feedState, snapshot?.error, select]);
  const toList = useCallback(() => select({ machineId: "", view: "equipment" }), [select]);
  const shown = machines.filter(m => machineName(m).toLowerCase().includes(machineSearch.toLowerCase()));
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
    <div ref={scrollRoot} className="fleet-home" data-machine-view={selection.view} hidden={flow.active}>
    <div className="fleet-workspace" data-os-page-context={JSON.stringify({ page: "Machines", machineId: machine?.id, machine: machine ? machineName(machine) : undefined, view: VIEW_NAMES[selection.view], search: machineSearch || undefined })}>
      <div className="fleet-workspace-header">
      {/* THE HEAD OWNS THE TRAIL on the list and on Equipment; every other
          machine view publishes its own from the inspector heading below.
          With a real list behind it, "Machines" is a link home and Back has
          somewhere to go. The machine's own crumb stays plain on Equipment:
          it leads to this very page, and a crumb that goes nowhere is not a
          link.

          NO MACHINE DROPDOWN. It existed because there was no list; there is
          one now, and two ways to pick a machine is one too many. */}
      <Head title="Machines"
        breadcrumbs={machine ? [{ label: "Machines", onSelect: toList }, { label: machineName(machine) }, { label: VIEW_NAMES.equipment }] : undefined}
        back={machine ? { label: "Machines", onSelect: toList } : undefined}
        navigation={selection.view === "equipment" || !machine}
        meta={machine ? undefined : listCount(snapshot, shown.length)}>
        {/* ON THE LIST the search narrows the rows beneath it (DESIGN.md rule 2:
            a filter is a question asked of the content). Inside a machine it
            keeps its results, which are the quick way across to another.
            ALWAYS RENDERED, machines or none: it is the target the window's own
            Search Fleet control focuses, and that control is there either way. */}
        {machine ? <Refine iconOnly label="Find machines" placeholder="Search machines" search={machineSearch} onSearch={setMachineSearch}>
          <div className="fleet-machine-results">{shown.map(m => <button type="button" className="fleet-reading-link" key={m.id} onClick={() => select({ machineId: m.id, view: "equipment" })}>{machineName(m)}<ChevronRight size={14} aria-hidden /></button>)}
          {!shown.length ? <EmptyState icon={Monitor} title="No matching machines">{machines.length ? "Try another machine name." : "Connect a machine to find it here."}</EmptyState> : null}</div>
        </Refine> : <Refine iconOnly label="Find machines" placeholder="Search machines" search={machineSearch} onSearch={setMachineSearch} />}
        <AddButton label="Add a machine" onClick={() => flow.start({})} />
      </Head>
      {machine ? <LocalTabs label="Machine views" value={selection.view} onChange={view => select({ machineId: machine.id, view })} options={(Object.keys(VIEW_NAMES) as MachineView[]).map(view => [view, VIEW_NAMES[view]] as const)}  /> : null}
      </div>
      {behind ? <Notice tone="warn" sentence="Machine updates are interrupted." next="Showing the last known state." detail={snapshot?.error || undefined}><AutoRefresh onRefresh={reload} /></Notice> : null}
      <div className="fleet-workspace-body" data-has-machines={machines.length > 0 || undefined}>
        <main className="fleet-equipment-area" aria-label="Machine workspace">
          {!settled && machines.length === 0 && !snapshot?.error ? <RecordListSkeleton label="Loading machines" rows={3} />
            : snapshot?.error && machines.length === 0 ? <EmptyState title="Machines unavailable">Reconnect to read your machine inventory. No changes have been made.</EmptyState>
            : machine ? retained.map(item => {
              const retainedMachine = machines.find(value => value.id === item.machineId);
              if (!retainedMachine) return null;
              const shown = item.machineId === machine.id && item.view === selection.view;
              return <div key={`${item.machineId}:${item.view}`} data-machine-view={item.view} hidden={!shown}>
                {item.view === "equipment" ? <><MachineEquipment machine={retainedMachine} now={now} viewerId={access?.userId ?? ""} onInspect={view => select({ machineId: item.machineId, view })} />
                <RecentWork machineId={item.machineId} onOpenSession={onOpenSession} onAllSessions={onAllSessions ?? (() => navigate("apps", { fromContent: true }))} /></> : <div className="fleet-inspector">
                  {!["models", "activity"].includes(item.view) ? <Head title={VIEW_NAMES[item.view]} meta={item.view === "apps" ? listCount(snapshot, RUNNABLE_APPS.length) : undefined} breadcrumbs={[{ label: "Machines", onSelect: toList }, { label: machineName(retainedMachine), onSelect: () => select({ machineId: item.machineId, view: "equipment" }) }, { label: VIEW_NAMES[item.view] }]} back={{ label: machineName(retainedMachine), onSelect: () => select({ machineId: item.machineId, view: "equipment" }) }}>
                    {item.view === "apps" ? <MachineAppsHelp /> : null}
                    {item.view === "apps" || item.view === "activity" ? <IconButton label="App sessions" onClick={() => navigate("apps", { fromContent: true })}><ArrowUpRight size={16} aria-hidden /></IconButton> : null}
                  </Head> : null}
                  <ScopedMachineDetail machine={retainedMachine} now={now} view={item.view} onRemoved={onRemoved}
                    modelNavigation={{ breadcrumbs: [{ label: "Machines", onSelect: toList }, { label: machineName(retainedMachine), onSelect: () => select({ machineId: item.machineId, view: "equipment" }) }], back: { label: machineName(retainedMachine), onSelect: () => select({ machineId: item.machineId, view: "equipment" }) } }} />
                </div>}
              </div>;
            })
            : machines.length === 0 ? <EmptyEquipment onConnect={() => flow.start({})} showRevoked={showRevoked} />
            : <MachineList machines={shown} now={now} searching={machineSearch !== ""} onOpen={id => select({ machineId: id, view: "equipment" })} />}
        </main>


      </div>
    </div></div>
  </>);
}

// THE MACHINES, AS A LIST -- on the kit's `RecordRow`, the same row the
// Deployables list draws. The name stands over where the machine is (its host
// and platform), the quiet middle says what it is doing, and the state is one
// word with a dot: Online, Offline or Revoked. A row opens its machine.
function MachineList({ machines, now, searching, onOpen }: {
  machines: readonly MachineRow[]; now: Date; searching: boolean; onOpen: (id: string) => void;
}) {
  if (machines.length === 0) return <EmptyState icon={Monitor} title="No matching machines">{searching ? "Try another machine name." : "Connect a machine to find it here."}</EmptyState>;
  return <RecordList as="ul" label="Machines">
    {machines.map(m => {
      const revoked = isRevoked(m);
      const online = !revoked && isWorkerOnline(m, now);
      const name = machineName(m);
      const calls = m.activeCount;
      // Where it is, when that says more than its name already did -- the
      // Deployables row does the same with a hostname that repeats the name.
      const platform = [m.os, m.arch].filter(Boolean).join(" ");
      const where = [m.hostname && m.hostname !== name ? m.hostname : "", platform].filter(Boolean).join(" \u00b7 ");
      return <RecordRow key={m.id}
        icon={<Monitor size={18} aria-hidden />}
        name={name}
        secondary={where || undefined}
        state={revoked ? "Revoked" : online ? "Online" : "Offline"}
        tone={online ? "accent" : "muted"}
        current={online}
        dim={revoked}
        label={`Open ${name}, ${revoked ? "revoked" : online ? "online" : "offline"}`}

        onOpen={() => onOpen(m.id)}>
        {calls > 0 ? <span>{calls} active {calls === 1 ? "call" : "calls"}</span> : null}
        <span>seen {formatFreshness(m.lastSeenAt, now)}</span>
      </RecordRow>;
    })}
  </RecordList>;
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
export function MachineEquipment({ machine, now, onInspect, viewerId = "" }: { machine: MachineRow; now: Date; onInspect: (view: MachineView) => void; viewerId?: string }) {
  const activity = machineActivity(machine, now);
  const ownerLabel = ownsMachine(machine, viewerId) ? "Your calls" : "Owner’s calls";
  const total = activity?.total ?? machine.activeCount;
  const [chosen, choose] = useState<{ kind: "model" | "app"; id: string } | null>(null);
  const models = machineModelsFrom(machine.reportedLabels);
  const online = isWorkerOnline(machine, now);
  const revoked = isRevoked(machine);
  const chosenModel = chosen?.kind === "model" ? models.find(m => m.modelId === chosen.id) : undefined;
  const chosenApp = chosen?.kind === "app" ? machine.apps.find(a => a.id === chosen.id) : undefined;
  const selectedModel = chosenModel ?? (!chosenApp ? models[0] : undefined);
  const selectedApp = chosenApp ?? (!selectedModel ? machine.apps[0] : undefined);
  const hw = machine.hardware;
  return <ActivityTarget target={`fleet:machine:${machine.id}`} className="fleet-equipment">
    <PageHelp title="Machine status"><p>Online means Cockpit is connected. The last-seen time reflects the most recent report; an offline machine cannot accept new work. Open Settings to manage sharing and inspect its connection and permissions.</p></PageHelp>
    <PageHelp title="Hardware"><p>The display shows the machine’s reported chip, memory and CPU cores. Unknown means that measurement was not reported.</p></PageHelp>
    <PageHelp title="Local models"><p>Use the arrows to browse models, then select one to inspect its context size, quantization and supported features. Manage models opens the installed models and available downloads for this machine. Installed inventory is reported by Cockpit; it does not by itself prove that a model can answer successfully.</p></PageHelp>
    <MachineAppsHelp />
    <header className="fleet-machine-heading"><div><h4>{machineName(machine)}</h4><span className="fleet-machine-presence"><ProvenanceDot tone={online ? "reachable" : "unreachable"} />{revoked ? "Revoked" : online ? "Online" : "Offline"}<span>{formatFreshness(machine.lastSeenAt, now)}</span></span></div></header>
    <div className="fleet-equipment-rig">
      <section className="fleet-equipment-bank" aria-label="Local model equipment"><div className="fleet-bank-heading"><Cpu size={16} aria-hidden /><h5>Local models</h5><IconButton label={models.length ? "Manage models" : "Find models that fit"} onClick={() => onInspect("models")}><SlidersHorizontal size={16} aria-hidden /></IconButton></div>
        <EquipmentModelCarousel models={models} selectedId={selectedModel?.modelId} onSelect={id => choose({ kind: "model", id })} />
        {models.length === 0 ? <EmptyState icon={Cpu} title="No local models">Find a model that fits this machine in Models.</EmptyState> : null}
      </section>
      <div className="fleet-device" aria-label="Machine hardware">
        <div className="fleet-device-screen"><Monitor size={35} strokeWidth={1.2} aria-hidden /><strong>{hw.chip || machine.platform || "Hardware not reported"}</strong><dl><div><dt>Memory</dt><dd>{hw.memoryBytes > 0 ? formatBytes(hw.memoryBytes) : "Unknown"}</dd></div><div><dt>CPU cores</dt><dd>{hw.cpuCores || "Unknown"}</dd></div></dl></div><div className="fleet-device-base" aria-hidden />
        <div className="fleet-device-work"><span>{total} active {total === 1 ? "call" : "calls"}</span><PageHelp title="Active work"><p>Calls dispatched by this cluster: models, commands and app sessions. The breakdown separates the machine owner’s work from work admitted through sharing. Each shared call counts once, even when several sharing grants apply.</p><p>Counts update automatically and can take a few seconds to appear. A dash means the breakdown is unavailable or out of date; the total then shows the last heartbeat report. Downloads are not calls.</p></PageHelp><IconButton label="Activity" onClick={() => onInspect("activity")}><ArrowUpRight size={16} aria-hidden /></IconButton></div>
        <dl className="fleet-device-call-breakdown" aria-label="Active call breakdown">
          <div><dt>{ownerLabel}</dt><dd>{activity?.owner ?? "—"}</dd></div>
          <div><dt>Shared calls</dt><dd>{activity?.shared ?? "—"}</dd></div>
        </dl>
      </div>
      <section className="fleet-equipment-bank" aria-label="App equipment"><div className="fleet-bank-heading"><Terminal size={16} aria-hidden /><h5>Installed apps</h5><span>{machine.apps.length}</span><IconButton label="Inspect apps" onClick={() => onInspect("apps")}><ArrowUpRight size={16} aria-hidden /></IconButton></div>
        <ul>{machine.apps.map(app => <li key={app.id}><button className="fleet-equipment-slot" type="button" aria-pressed={selectedApp?.id === app.id} onClick={() => choose({ kind: "app", id: app.id })}><span><strong>{app.label}</strong><small>{machineAppAvailability(machine, app.runnable, now)}</small></span></button></li>)}</ul>
        {machine.apps.length === 0 ? <EmptyState icon={Terminal} title="No installed apps">Apps reported by this machine will appear here.</EmptyState> : null}
      </section>
    </div>
    <section aria-label="Machine features"><Facts>{machineFeatureStates(machine, now).map(feature => <Fact key={feature.key} label={feature.label} value={feature.answer} title={feature.detail} />)}</Facts></section>
    {selectedModel || selectedApp ? <section className="fleet-equipment-reading" aria-label="Selected equipment" aria-live="polite" data-os-page-context={JSON.stringify({ selectedModel: selectedModel?.modelId, selectedApp: selectedApp?.id })}>
      {selectedModel ? <div><strong>{selectedModel.modelId}</strong><p>{modelSummary(selectedModel)}{!online ? " · Machine offline" : ""}</p></div>
        : selectedApp ? <div><strong>{selectedApp.label}</strong><p>{machineAppAvailability(machine, selectedApp.runnable, now)}{selectedApp.why ? ` · ${selectedApp.why}` : ""}{!online ? " Machine offline." : ""}</p><AppPermissionHelp app={selectedApp} machine={machine} /></div> : null}
    </section> : null}

  </ActivityTarget>;
}

function modelSummary(model: MachineModel): string {
  return [model.contextWindow ? `${model.contextWindow.toLocaleString()} context` : "Context not reported", model.quant, model.tools ? "Tool calling" : "", model.structuredOutput ? "Structured output" : ""].filter(Boolean).join(" · ");
}

function ScopedMachineDetail({ machine, now, view, onRemoved, modelNavigation }: { machine: MachineRow; now: Date; view: Exclude<MachineView, "equipment">; onRemoved: (id: string) => void; modelNavigation: Omit<PageTrail, "title"> }) {
  const writes = useMachineWrites();
  const revoke = async (id: string, reason: string) => {
    const ok = await writes.revoke(id, reason);
    if (ok) onRemoved(id);
    return ok;
  };
  return <MachineDetail machine={machine} writes={{ ...writes, revoke }} now={now} view={view} modelNavigation={modelNavigation} />;
}

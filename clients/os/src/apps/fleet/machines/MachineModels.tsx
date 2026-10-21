import { useGraphRevision } from "../../../live/useGraphRevision";
import { useCallback, useMemo, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { ArrowUpRight, Box, Pencil } from "lucide-react";
import { useSession } from "../../../chrome/access";
import { useReading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { EmptyState, Fact, Facts, Head, Notice, Refine, Select, useNow } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { ActionBar } from "../../../kit/ActionBar";
import { AutoRefresh } from "../../../kit/AutoRefresh";
import { PageHelp } from "../../../kit/pageHelp";
import type { PageTrail } from "../../../kit/pageNavigation";
import { CardCarousel } from "../../../kit/CardCarousel";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { formatBytes, formatMoment } from "../../../kit/format";
import type { MachineRow } from "../rows";
import { isWorkerOnline } from "../online";
import { formatParams, formatContext } from "../models/ordering";
import { EmbeddingStatus } from "../models/EmbeddingStatus";
import { MachineDetailsSection } from "./MachineDetailsInfo";
import { machineModelsFrom, modelPurposeLabel } from "./models";
import { LivePull, PullHistory, SetupLiveDownload } from "./ModelDownloads";
import { setupDownloading, setupNow } from "./setup";
import { useModelPulls } from "./useModelPulls";
import { MODEL_PURPOSES, modelSetupFromRow, safeModelSource, sameModelChoices, setupCapacity, type ModelChoices, type ModelSetupView, type SetupModel } from "./modelSetup";
import { ModelAssignmentBoard } from "./ModelAssignmentBoard";
import "./machineModels.css";

export function MachineModels({ machine, navigation }: { machine: MachineRow; standalone?: boolean; navigation?: Omit<PageTrail, "title"> }) {
  const { access } = useSession();
  const connection = useOsConnection();
  const now = useNow();
  const own = !!access?.userId && access.userId === machine.ownerUserId;
  const online = isWorkerOnline(machine, now);
  const downloads = useModelPulls(machine.id);
  // The installer's own download (memql#6229): shown where a pull from here
  // would be, and holding Install model back for the same reason.
  const installerDownload = setupDownloading(setupNow(machine.setup, now));
  const installed = useMemo(() => machineModelsFrom(machine.reportedLabels), [machine.reportedLabels]);
  const read = useCallback(async (signal: AbortSignal) => {
    const result = await connection!.query.fleetModelSetup({ registrationId: machine.id }, { signal, priority: "background" });
    const row = result.rows()[0];
    if (!row) throw new Error("The machine did not return its model setup.");
    return modelSetupFromRow(row as Row);
  }, [connection, machine.id]);
  const liveRevision = useGraphRevision(["v1:worker:registration", "v1:models:modelProfile", "v1:models:catalogResearchSettings", "v1:models:catalogReview", "v1:platform:embedderBinding", "v1:worker:modelPull"]);
  const reading = useReading(`model-setup:${machine.id}:${own}`, connection && own ? read : null, liveRevision);
  const view = reading.value;
  const [draft, setDraft] = useState<ModelChoices | null>(null);
  const [editBase, setEditBase] = useState<ModelSetupView | null>(null);
  const [revision, setRevision] = useState("");
  const [saving, setSaving] = useState(false);
  const savePending = useRef(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [catalogOpen, setCatalogOpen] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [purpose, setPurpose] = useState("");
  const [availability, setAvailability] = useState("all");
  const choices = draft ?? view?.choices ?? {};
  // AN AUTOMATIC SETUP SAVES NO CHOICES, yet MemQL still loads a model for each
  // task: the picks the engine suggests, which Edit starts from. Measuring the
  // empty saved choices instead drew an empty bar until someone saved a setup.
  const automatic = !!view && !view.revision && draft === null;
  const capacity = setupCapacity(automatic ? view.suggestedChoices : choices, view?.catalog ?? [], view?.usableBytes ?? 0);
  const profile = view?.catalog.find(m => m.modelId === selected);
  const localModel = installed.find(m => m.modelId === selected);
  // What the model's facts report; an empty answer reads as a placeholder
  // and copies nothing.
  const purposes = profile?.purposes.map(p => MODEL_PURPOSES.find(([id]) => id === p)?.[1] ?? p).join(", ") || (localModel ? modelPurposeLabel(localModel) : "");
  const params = localModel ? formatParams(localModel.params) : "";
  const contextLimit = localModel ? formatContext(localModel.contextWindow) : "";
  const placement = localModel ? "Installed" : profile?.blocked || "Fits this machine";
  const memory = profile?.memoryBytes ? formatBytes(profile.memoryBytes) : "";
  const download = profile?.sizeBytes ? formatBytes(profile.sizeBytes) : "";
  const source = safeModelSource(profile?.source ?? "");
  const editing = draft !== null;
  const dirty = editing && !sameModelChoices(draft, editBase?.choices ?? {});
  const editView = view ?? editBase;
  const staleDraft = editing && !!view && view.revision !== revision;
  const beginEdit = () => { if (!view) return; setEditBase(view); setDraft({ ...(view.revision ? view.choices : view.suggestedChoices) }); setRevision(view.revision); setError(""); setSaved(false); };
  const save = async () => {
    if (!own || !connection || !draft || !view || staleDraft || reading.error || !capacity.fits || savePending.current) return;
    savePending.current = true;
    setSaving(true); setError("");
    try {
      await connection.query.fleetSetModelSetup({ registrationId: machine.id, revision, choices: draft });
      setDraft(null); setEditBase(null); setSaved(true); reading.reread();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); reading.reread(); }
    finally { savePending.current = false; setSaving(false); }
  };
  const back = () => { setSelected(null); setError(""); };
  const toModels = () => { setSelected(null); setCatalogOpen(false); setError(""); };
  const install = async (model: SetupModel) => {
    setError(""); const refusal = await downloads.start(model.modelId);
    if (refusal) setError(refusal); else { setSelected(null); setCatalogOpen(false); reading.reread(); }
  };
  const shown = (view?.catalog ?? []).filter(m => (!search || m.modelId.toLowerCase().includes(search.toLowerCase())) && (!purpose || m.purposes.includes(purpose)) && (availability === "all" || (availability === "installed" ? m.installed : !m.blocked)));
  const title = selected ?? (catalogOpen ? "Catalog" : "Models");
  const breadcrumbs = [
    ...(navigation?.breadcrumbs ?? []),
    { label: "Models", onSelect: selected || catalogOpen ? toModels : undefined },
    ...(catalogOpen ? [{ label: "Catalog", onSelect: selected ? back : undefined }] : []),
    ...(selected ? [{ label: selected }] : []),
  ];
  return <div className="fleet-machine-details fleet-models">
    <Head navigationKey={`models:${machine.id}:${selected ?? (catalogOpen ? "catalog" : "installed")}`} title={title} meta={selected ? undefined : catalogOpen ? shown.length : installed.length}
      breadcrumbs={breadcrumbs} back={selected ? { label: catalogOpen ? "Catalog" : "Models", onSelect: back } : catalogOpen ? { label: "Models", onSelect: toModels } : navigation?.back}>
      {catalogOpen && !selected ? <Refine label="Refine model catalog" search={search} onSearch={setSearch} placeholder="Find a model">
        <Select id="model-purpose-filter" label="Task" value={purpose} onChange={setPurpose}><option value="">All tasks</option>{MODEL_PURPOSES.map(([id,label]) => <option key={id} value={id}>{label}</option>)}</Select>
        <Select id="model-fit-filter" label="Availability" value={availability} onChange={setAvailability}><option value="all">All models</option><option value="fits">Fits this machine</option><option value="installed">Installed</option></Select>
      </Refine> : null}
    </Head>
    <AutoRefresh enabled={own && (!editing || reading.state === "failed")} busy={reading.state === "reading"} onRefresh={reading.reread} />
    <AutoRefresh enabled={feedIsBehind(downloads.state, downloads.feedError)} busy={downloads.loading} onRefresh={downloads.reseed} />
    <PageHelp title="Models on this machine">
      <p>Installed models occupy storage. Your setup chooses which model serves each task. One model can serve several tasks; its memory counts once. Automatic means MemQL chooses among the models this machine offers.</p>
      <p>Memory is a planning estimate at up to 32K context, with room left for the operating system. It is not a live measurement. With Automatic, it counts the model MemQL picks for each task. Saved setups limit subsequent calls to that context and run one model call at a time. Other applications and larger images can change actual use.</p>
      <p>Catalog recommendations favor local models that fit. Installing does not change your setup. Existing memory search keeps its bound model; switching it requires a separate reindex.</p>
      <p>In Model setup, select an installed model and click a task, or drag the model onto it. Highlighted tasks use the selected model. Plain solid borders mark available assignments; dashed borders mark unavailable ones. Use matching tasks to share one model across them. Save setup applies the choices to new calls; existing work finishes.</p>
      <p>Catalog research can review primary sources weekly using a local model. It starts off and must be enabled separately. It cannot install models or change your choices.</p>
    </PageHelp>
    {reading.error ? <Notice tone="error" sentence="The catalog could not be read." next="Your installed models are still shown below." detail={reading.error} /> : null}
    {error && !editing ? <Notice tone="error" sentence="That change could not be completed." detail={error} /> : null}
    {downloads.feedError ? <Notice tone="warn" sentence="Download updates are interrupted." detail={downloads.feedError} /> : null}
    {downloads.live ? <LivePull pull={downloads.live} /> : installerDownload ? <SetupLiveDownload stage={installerDownload} /> : null}
    {selected ? <>
      <MachineDetailsSection title="Overview"><Facts>
        <Fact label="Available for" value={purposes || "Not reported"} />
        {localModel ? <><Fact label="Parameters" value={params || "Not reported"} copyValue={params || undefined} /><Fact label="Quantization" value={localModel.quant || "Not reported"} copyValue={localModel.quant || undefined} /><Fact label="Maximum context" value={contextLimit || "Not reported"} copyValue={contextLimit || undefined} /></> : null}
        <Fact label="On this machine" value={placement} />
        <Fact label="Estimated memory" value={memory || "Not verified"} copyValue={memory || undefined} />
        <Fact label="Download" value={download || "Not verified"} copyValue={download || undefined} />
        {profile?.reviewedAt ? <Fact label="Source reviewed" value={new Date(profile.reviewedAt).toLocaleDateString(undefined, { timeZone: "UTC", year: "numeric", month: "short", day: "numeric" })} copyValue={profile.reviewedAt} /> : null}
        {profile ? <Fact label="Runtime" value={profile.runtime} copyValue={profile.runtime} /> : null}
        {source ? <Fact label="Source" value={<a href={source} target="_blank" rel="noreferrer">Model card</a>} copyValue={source} /> : null}
      </Facts></MachineDetailsSection>
      {profile?.notes ? <MachineDetailsSection title="About"><p className="os-caption">{profile.notes}</p></MachineDetailsSection> : null}
      {localModel?.embeddings ? <EmbeddingStatus model={localModel.modelId} /> : null}
      {profile && !localModel ? <ActionBar state={profile.installBlocked || (online ? "Ready to install" : "Machine offline")}
        acts={own && online && !profile.installBlocked && !downloads.live && !installerDownload ? [{ label: "Install model", tone: "primary", busy: downloads.starting, onAct: () => { void install(profile); } }] : []} /> : null}
    </> : catalogOpen ? <>
      {shown.length ? <RecordList as="ul" label="Model catalog">{shown.map(m => <RecordRow key={m.modelId} name={m.modelId}
        secondary={m.purposes.map(p => MODEL_PURPOSES.find(([id]) => id === p)?.[1]).filter(Boolean).join(" · ")}
        state={m.installed ? "Installed" : m.blocked ? "Unavailable" : m.recommended ? "Recommended" : "Fits"}
        stateTitle={m.blocked || undefined} tone={m.installed || m.recommended && !m.blocked ? "accent" : "muted"}
        onOpen={() => setSelected(m.modelId)}>{m.blocked || (m.memoryBytes ? `${formatBytes(m.memoryBytes)} memory` : "Memory not verified")}</RecordRow>)}</RecordList>
        : <EmptyState icon={Box} title="No matching models">Try another task or clear the filter.</EmptyState>}
    </> : editing && editView ? <>
      {staleDraft ? <Notice tone="warn" sentence="This setup changed elsewhere." next="Discard changes to load the latest setup before editing again." /> : null}
      <ModelAssignmentBoard view={editView} choices={draft} onChange={setDraft} saving={saving} saveError={error}
        canSave={own && !!connection && !!view && !reading.error && !staleDraft && (dirty || !revision)} onSave={() => { void save(); }}
        onDiscard={() => { if (!saving) { setDraft(null); setEditBase(null); setError(""); } }} />
    </> : <>
      {own && !view && (reading.state === "reading" || reading.state === "unread") ? <RecordListSkeleton label="Loading model setup" /> : null}
      {view ? <>
        <MachineDetailsSection title="Capacity">
          <div className="fleet-models-capacity" aria-label="Model memory estimate">
            <div><strong>{capacity.known ? formatBytes(capacity.used) : "Unverified"}</strong><span>{`of ${formatBytes(view.usableBytes)} for models`}</span></div>
            <div><strong>{view.diskFreeBytes ? formatBytes(view.diskFreeBytes) : "Not reported"}</strong><span>free storage</span></div>
          </div>
          <div className="fleet-models-capacity-track" role="meter" aria-label="Planned model memory" aria-valuemin={0} aria-valuemax={view.usableBytes || 1} aria-valuenow={Math.min(capacity.used,view.usableBytes)} aria-valuetext={`${formatBytes(capacity.used)} planned${automatic ? " automatically" : ""}`} data-over={!capacity.fits}>
            {capacity.selected.map(id => { const m = view.catalog.find(item => item.modelId === id); return <span key={id} title={`${id}: ${m?.memoryBytes ? formatBytes(m.memoryBytes) : "unverified"}`} style={{ width: `${view.usableBytes ? Math.min(100,(m?.memoryBytes ?? 0)/view.usableBytes*100) : 0}%` }} />; })}
          </div>
          <p className="os-caption">{editing && !capacity.fits ? "Choose smaller models or reuse one across tasks." : automatic ? "Memory estimate for MemQL’s automatic picks · room left for your system" : "Memory estimate · room left for your system"}</p>
        </MachineDetailsSection>
        {installed.length > 0 || view.revision ? <MachineDetailsSection title="Model setup">
          <div className="fleet-models-setup-title"><span className="os-caption">{view.revision ? "Your choices" : "MemQL chooses automatically"}</span>
            {own && !editing ? <IconButton label="Change model setup" onClick={beginEdit}><Pencil size={16} aria-hidden /></IconButton> : null}
          </div>
          <div className="fleet-models-choices">
            {MODEL_PURPOSES.map(([id,label]) => <div className="fleet-models-choice" key={id}>
              <span>{label}</span>
              <span className={choices[id] ? "" : "os-caption"}>{choices[id] || (view.revision ? "Not assigned" : installed.some(m => view.catalog.some(p => p.modelId === m.modelId && p.purposes.includes(id))) ? "Automatic" : "No model installed")}</span>
            </div>)}
          </div>
          {view.warning ? <Notice tone="warn" sentence="Your saved setup needs attention." detail={view.warning} /> : null}
          {saved && !editing ? <p className="os-caption" role="status">Model setup saved.</p> : null}
        </MachineDetailsSection> : null}
      </> : null}
      <MachineDetailsSection title="Installed">
        {installed.length ? <CardCarousel items={installed} itemKey={m => m.modelId} label="Installed models" noun="model" renderItem={m => <button className="fleet-equipment-slot" onClick={() => setSelected(m.modelId)} type="button"><Box size={18} aria-hidden /><span><strong>{m.modelId}</strong><small>{modelPurposeLabel(m)}</small></span></button>} />
          : <p className="os-caption">{machine.hardware.runtimes.length || Object.keys(machine.reportedLabels).some(k => k.startsWith("runtime:")) ? "No models installed yet." : "No model runtime reported yet."}</p>}
      </MachineDetailsSection>
      {view ? <MachineDetailsSection title="Catalog">
        <div className="fleet-models-catalog-intro"><span><strong>{view.catalog.filter(m => !m.blocked).length} models fit this machine</strong><small>Local models first. Your setup stays yours.</small></span>
          <IconButton label="Browse catalog" onClick={() => setCatalogOpen(true)}><ArrowUpRight size={16} aria-hidden /></IconButton></div>
        <p className="os-caption">{view.researchEnabled ? "Weekly research is on." : "Weekly research is off."}{view.researchAt ? ` Last review ${formatMoment(view.researchAt)}.` : ""}</p>
        {view.researchSummary ? <p className="os-caption">{view.researchSummary}</p> : null}
      </MachineDetailsSection> : null}
      {downloads.pulls.length ? <MachineDetailsSection title="Downloads"><PullHistory pulls={downloads.pulls} live={downloads.live} loading={downloads.loading} error={downloads.feedError} /></MachineDetailsSection> : null}
    </>}
  </div>;
}

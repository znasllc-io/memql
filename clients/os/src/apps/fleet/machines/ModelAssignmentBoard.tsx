import { useId, useRef, useState } from "react";
import { ArrowRight, Braces, GripVertical, Image, LockKeyhole, MessageCircle, Mic, ScanEye, Search, Volume2, Wrench, X } from "lucide-react";
import { Button, Notice, Subhead } from "../../../kit";
import { IconButton } from "../../../kit/IconButton";
import { formatBytes } from "../../../kit/format";
import { MODEL_PURPOSES, assignmentProposal, sameModelChoices, setupCapacity, type ModelChoices, type ModelSetupView } from "./modelSetup";

const taskIcons = { conversation: MessageCircle, structured: Braces, tools: Wrench, vision: ScanEye, embeddings: Search, audioIn: Mic, audioOut: Volume2, imageGen: Image };
const dragType = "text/plain";
const taskSummary = (purposes: string[]) => {
  if (purposes.includes("conversation") && purposes.includes("tools") && purposes.includes("vision")) return "Conversation, tools & vision";
  const labels = MODEL_PURPOSES.filter(([id]) => purposes.includes(id)).map(([, label]) => label);
  return labels.length > 2 ? `${labels.slice(0, 2).join(", ")} +${labels.length - 2}` : labels.join(", ") || "No tasks reported";
};

export function ModelAssignmentBoard({ view, choices, onChange, onSave, onDiscard, saving, canSave, saveError = "" }: {
  view: ModelSetupView; choices: ModelChoices; onChange: (choices: ModelChoices) => void;
  onSave: () => void; onDiscard: () => void; saving: boolean; canSave: boolean; saveError?: string;
}) {
  const installed = view.catalog.filter(m => m.installed);
  const [selectedId, setSelectedId] = useState(() => installed.find(m => !m.blocked && m.memoryBytes)?.modelId ?? installed[0]?.modelId ?? "");
  const [preview, setPreview] = useState<ModelChoices | null>(null);
  const [message, setMessage] = useState("");
  const [refusal, setRefusal] = useState("");
  const [dropTarget, setDropTarget] = useState("");
  const dragging = useRef("");
  const taskButtons = useRef(new Map<string, HTMLButtonElement>());
  const helpId = useId();
  const selected = installed.find(m => m.modelId === selectedId);
  const capacity = setupCapacity(choices, view.catalog, view.usableBytes);
  const displayed = setupCapacity(preview ?? choices, view.catalog, view.usableBytes);
  const delta = displayed.used - capacity.used;
  const matching = MODEL_PURPOSES.filter(([id]) => selected?.purposes.includes(id) && !(id === "embeddings" && view.boundEmbeddingModel && selected.modelId !== view.boundEmbeddingModel)).map(([id]) => id);
  const suggestion = { ...view.suggestedChoices, ...(view.boundEmbeddingModel ? { embeddings: view.boundEmbeddingModel } : {}) };
  const hasSuggestion = Object.values(suggestion).some(Boolean) && setupCapacity(suggestion, view.catalog, view.usableBytes).fits;
  const apply = (modelId: string, purposes: string[]) => {
    if (saving) return;
    const result = assignmentProposal(choices, modelId, purposes, view);
    setPreview(null); setDropTarget(""); setRefusal(result.refusal);
    if (result.refusal) return;
    onChange(result.choices);
    setMessage(modelId ? `${purposes.length === 1 ? MODEL_PURPOSES.find(([id]) => id === purposes[0])?.[1] : `${purposes.length} tasks`} now uses ${modelId}.` : "Assignment cleared.");
  };
  const previewTask = (id: string) => {
    if (saving || !selected) return;
    const result = assignmentProposal(choices, selected.modelId, [id], view);
    setPreview(result.refusal ? null : result.choices);
  };
  return <div className="fleet-assignment-editor" aria-label="Model setup editor">
    <div className="fleet-models-heading">
      <Subhead>Model setup</Subhead>
      {hasSuggestion ? <button type="button" className="os-link fleet-assignment-suggest" disabled={saving} onClick={() => {
        onChange(suggestion); setPreview(null); setRefusal(""); setMessage("Suggested models assigned.");
      }}>Use suggested</button> : null}
    </div>
    <section className="fleet-assignment-capacity" aria-label="Model capacity">
      <div className="fleet-assignment-capacity-label"><span><strong>{displayed.known ? formatBytes(displayed.used) : "Unverified"}</strong> <small>of {view.usableBytes ? formatBytes(view.usableBytes) : "unverified capacity"}</small></span>
        <span>{preview && delta !== 0 ? `${delta > 0 ? "+" : "−"}${formatBytes(Math.abs(delta))} if assigned` : capacity.known && view.usableBytes > 0 ? capacity.used > view.usableBytes ? `${formatBytes(capacity.used - view.usableBytes)} over capacity` : `${formatBytes(view.usableBytes - capacity.used)} remaining` : "Memory estimate unavailable"}</span></div>
      <div className="fleet-models-capacity-track" role="meter" aria-label="Planned model memory" aria-valuemin={0} aria-valuemax={view.usableBytes || 1} aria-valuenow={Math.min(displayed.used, view.usableBytes)} aria-valuetext={displayed.known ? `${formatBytes(displayed.used)} estimated of ${formatBytes(view.usableBytes)}` : "Memory estimate unavailable"} data-over={!displayed.fits}>
        {displayed.selected.map(id => { const m = view.catalog.find(item => item.modelId === id); return <span key={id} title={`${id}: ${m?.memoryBytes ? formatBytes(m.memoryBytes) : "unverified"}`} style={{ width: `${view.usableBytes ? Math.min(100, (m?.memoryBytes ?? 0) / view.usableBytes * 100) : 0}%` }} />; })}
      </div>
      <div className="fleet-assignment-capacity-caption"><span>Estimated memory · shared models count once</span><span>System headroom reserved</span></div>
    </section>
    <div className="fleet-assignment-board">
      <section aria-label="Installed models">
        <Subhead meta={installed.length}>Installed models</Subhead>
        <div className="fleet-assignment-models">{installed.map(model => {
          const count = Object.values(choices).filter(id => id === model.modelId).length;
          const unavailable = model.blocked || (!model.memoryBytes ? "Memory estimate unavailable" : "");
          return <button key={model.modelId} type="button" className="fleet-assignment-model" aria-label={`Select ${model.modelId}`} aria-description={MODEL_PURPOSES.filter(([id]) => model.purposes.includes(id)).map(([, label]) => label).join(", ")} aria-pressed={selectedId === model.modelId} disabled={saving}
            draggable={!saving && !unavailable} onClick={() => { setSelectedId(model.modelId); setPreview(null); setRefusal(""); setMessage(""); }}
            onDragStart={event => {
              if (saving || unavailable) { event.preventDefault(); return; }
              dragging.current = model.modelId; setSelectedId(model.modelId); setRefusal("");
              event.dataTransfer.setData(dragType, model.modelId); event.dataTransfer.effectAllowed = "all";
            }} onDragEnd={() => { dragging.current = ""; setDropTarget(""); setPreview(null); }}>
            <GripVertical size={13} aria-hidden className="fleet-assignment-grip" />
            <span className="fleet-assignment-model-body"><strong>{model.modelId}</strong>
              <span className="fleet-assignment-model-tasks">{taskSummary(model.purposes)}</span>
              <span className="fleet-assignment-model-meta"><span>{model.memoryBytes ? formatBytes(model.memoryBytes) : "Unverified"}</span><span>{count ? `${count} ${count === 1 ? "task" : "tasks"}` : "Available"}</span></span>
              {unavailable ? <span className="fleet-assignment-unavailable">{unavailable}</span> : null}
            </span>
          </button>;
        })}</div>
        {!installed.length ? <p className="os-caption">No models installed. Browse the catalog to choose one.</p> : null}
        {selected && !selected.blocked && selected.memoryBytes > 0 && matching.length > 1 ? <button type="button" className="os-link fleet-assignment-all" disabled={saving} onClick={() => apply(selected.modelId, matching)}>Use for {matching.length} matching tasks <ArrowRight size={14} aria-hidden /></button> : null}
      </section>
      <section aria-label="Task assignments">
        <Subhead meta={selected && !selected.blocked ? `${matching.length} compatible` : undefined}>Tasks</Subhead>
        <div className="fleet-assignment-tasks">{MODEL_PURPOSES.map(([id, label]) => {
          const Icon = taskIcons[id];
          const assigned = view.catalog.find(m => m.modelId === choices[id]);
          const locked = id === "embeddings" && !!view.boundEmbeddingModel;
          const compatible = !!selected && !selected.blocked && selected.memoryBytes > 0 && matching.includes(id);
          const missing = !!choices[id] && (!assigned?.installed || !!assigned.blocked || !assigned.purposes.includes(id));
          return <div key={id} className="fleet-assignment-task">
            <button ref={element => { if (element) taskButtons.current.set(id, element); else taskButtons.current.delete(id); }} type="button" className="fleet-assignment-task-target" disabled={saving}
              aria-label={`${label}: ${choices[id] || "Not assigned"}`} aria-describedby={helpId} data-compatible={compatible} data-assigned={!!selected && choices[id] === selected.modelId} data-drop-target={dropTarget === id}
              onClick={() => selected ? apply(selected.modelId, [id]) : setMessage("Select an installed model first.")}
              onMouseEnter={() => previewTask(id)} onMouseLeave={() => setPreview(null)} onFocus={() => previewTask(id)} onBlur={() => setPreview(null)}
              onDragEnter={event => { if (dragging.current && !saving) { event.preventDefault(); event.stopPropagation(); } }}
              onDragOver={event => {
                if (!dragging.current || saving || !event.dataTransfer.types.includes(dragType)) return;
                event.preventDefault();
                event.stopPropagation();
                const result = assignmentProposal(choices, dragging.current, [id], view);
                event.dataTransfer.dropEffect = result.refusal ? "none" : "copy";
                setDropTarget(result.refusal ? "" : id); setPreview(result.refusal ? null : result.choices);
              }} onDragLeave={() => { setDropTarget(""); setPreview(null); }}
              onDrop={event => {
                event.preventDefault();
                event.stopPropagation();
                const modelId = event.dataTransfer.getData(dragType);
                if (!dragging.current || modelId !== dragging.current) return;
                apply(modelId, [id]); dragging.current = ""; taskButtons.current.get(id)?.focus(); setPreview(null);
              }}>
              <span className="fleet-assignment-task-name"><Icon size={16} aria-hidden /><span>{label}</span>{locked ? <LockKeyhole size={13} aria-label="Bound memory search model" /> : null}</span>
              <span className="fleet-assignment-task-model">{choices[id] || (installed.some(m => m.purposes.includes(id)) ? "Not assigned" : "No model installed")}{missing ? " · unavailable" : ""}</span>
            </button>
            {choices[id] && !locked ? <IconButton className="fleet-assignment-clear" label={`Clear ${label}`} disabled={saving} onClick={() => { apply("", [id]); taskButtons.current.get(id)?.focus(); setPreview(null); }}><X size={14} aria-hidden /></IconButton> : null}
          </div>;
        })}</div>
        <p id={helpId} className="os-caption fleet-assignment-hint">Select a model, then a task. Or drag it into place.</p>
        {view.boundEmbeddingModel ? <p className="os-caption">Memory search keeps {view.boundEmbeddingModel}. Switching requires a reindex.</p> : null}
        <span className="os-sr-only" role="status">{message}</span>
      </section>
    </div>
    {refusal ? <Notice tone="warn" sentence={refusal} /> : null}
    {!capacity.fits ? <Notice tone="warn" sentence="This setup needs attention." next="Clear unavailable assignments, choose smaller models or reuse one across tasks." /> : null}
    {saveError ? <Notice tone="error" sentence="The setup could not be saved." detail={saveError} /> : null}
    <div className="os-panel-actions fleet-assignment-actions">
      <button type="button" className="os-link" disabled={saving} onClick={onDiscard}>{sameModelChoices(choices, view.choices) && view.revision ? "Done" : "Discard changes"}</button>
      {capacity.fits && canSave ? <Button tone="primary" busy={saving} busyLabel="Saving setup…" onClick={onSave}>Save setup</Button> : null}
    </div>
  </div>;
}

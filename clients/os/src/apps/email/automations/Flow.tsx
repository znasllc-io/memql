import { AddButton } from "../../../kit/AddButton";
import { isPublished } from "../rows";
import { useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { ArrowDown, ArrowUp, Clock, Mail, Trash2 } from "lucide-react";
import { Field, Input, Select } from "../../../kit";
import { useWide } from "../../../kit/useWide";
import { isOperatorOrganization } from "../organizations";
import type { SenderIdentityRow, TemplateRow } from "../rows";
import type { Issue } from "./issues";
import { insertStep, moveStep, newStep, removeStep, stepSentence, updateStep, type FlowStep, type StepKind } from "./flowModel";

export interface FlowProps {
  trigger: string; triggerDetail?: ReactNode; steps: readonly FlowStep[]; accountId: string;
  templates: readonly TemplateRow[]; senders: readonly SenderIdentityRow[]; editable?: boolean; proposal?: boolean;
  issues?: readonly Issue[]; counts?: ReadonlyMap<string, string>;
  onChange?: (steps: FlowStep[]) => void;
  name?: string; description?: string; onName?: (value: string) => void; onDescription?: (value: string) => void;
  /** Event rules retain their recipient controls in the same inspector. */
  inspector?: (selected: string) => ReactNode;
  fixed?: boolean;
}
export function Flow({ trigger, triggerDetail, steps, accountId, templates, senders, editable = false, proposal = false, issues = [], counts = new Map(), onChange, name = "", description = "", onName, onDescription, inspector, fixed = false }: FlowProps) {
  const root = useRef<HTMLDivElement>(null);
  const wide = useWide(root, 760);
  const [selected, setSelected] = useState("");
  const [focused, setFocused] = useState("trigger");
  const [inserting, setInserting] = useState<number | null>(null);
  const edit = editable && !!onChange;
  const target = steps.find(s => s.id === selected);
  const tabs = ["trigger", ...steps.flatMap((s, i) => edit && !fixed ? [`insert-${i}`, s.id] : [s.id]), ...(edit && !fixed ? [`insert-${steps.length}`] : []), "end"];
  const activeTab = tabs.includes(focused) ? focused : "trigger";
  function focus(id: string) { setFocused(id); root.current?.querySelector<HTMLButtonElement>(`[data-node="${id}"]`)?.focus(); }
  function select(id: string) { if (!edit) return; setSelected(id); setInserting(null); }
  function patch(value: Partial<FlowStep>) { if (target) onChange?.(updateStep(steps, target.id, value)); }
  function move(index: number, delta: -1 | 1) { onChange?.(moveStep(steps, index, delta)); }
  function remove(index: number) { const next = steps[index + 1]?.id ?? steps[index - 1]?.id ?? "trigger"; onChange?.(removeStep(steps, index)); setSelected(next); setFocused(next); requestAnimationFrame(() => focus(next)); }
  function key(event: KeyboardEvent, id: string) {
    const index = steps.findIndex(s => s.id === id);
    if (edit && !fixed && event.altKey && (event.key === "ArrowUp" || event.key === "ArrowDown") && index >= 0) { event.preventDefault(); move(index, event.key === "ArrowUp" ? -1 : 1); return; }
    if (edit && !fixed && event.key === "Delete" && index >= 0) { event.preventDefault(); remove(index); return; }
    const at = tabs.indexOf(id);
    const next = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : event.key === "ArrowUp" ? Math.max(0, at - 1) : event.key === "ArrowDown" ? Math.min(tabs.length - 1, at + 1) : -1;
    if (next >= 0) { event.preventDefault(); focus(tabs[next]!); }
  }
  const attrs = (id: string) => ({ "data-node": id, tabIndex: activeTab === id ? 0 : -1, onFocus: () => setFocused(id), onKeyDown: (e: KeyboardEvent) => key(e, id) });
  const options = templates.filter(t => t.accountId === accountId && t.status !== "archived");
  const identities = senders.filter(s => s.accountId === accountId && s.status !== "disabled");
  const fields = inspector ? inspector(selected) : !target ? <><Field label="Name"><Input id="email-flow-name" label="Name" value={name} onChange={onName ?? (() => {})} /></Field><Field label="Description"><Input id="email-flow-description" label="Description" value={description} onChange={onDescription ?? (() => {})} /></Field></> : target.kind === "wait" ? <>
    <Field label="Days"><Select id="email-flow-days" label="Days" value={String(target.days ?? 0)} onChange={v => patch({ days: Number(v) })}>{Array.from({ length: 366 }, (_, i) => <option key={i} value={i}>{i}</option>)}</Select></Field>
    <Field label="Hours"><Select id="email-flow-hours" label="Hours" value={String(target.hours ?? 0)} onChange={v => patch({ hours: Number(v) })}>{Array.from({ length: 24 }, (_, i) => <option key={i} value={i}>{i}</option>)}</Select></Field>
  </> : <>
    <Field label="Template"><Select id="email-flow-template" label="Template" value={target.templateId ?? ""} onChange={templateId => patch({ templateId })}><option value="">Choose a template</option>{options.map(t => <option key={t.id} value={t.id}>{t.name}{isPublished(t) ? "" : " · Draft"}</option>)}</Select></Field>
    <Field label="Sender"><Select id="email-flow-sender" label="Sender" value={target.senderIdentityId ?? ""} onChange={senderIdentityId => patch({ senderIdentityId })}><option value="">{isOperatorOrganization(accountId) ? "This cluster's sender" : "Choose a sender"}</option>{identities.map(s => <option key={s.id} value={s.id}>{s.address}</option>)}</Select></Field>
    {target.kind === "confirm" ? <Field label="Wait up to"><Select id="email-flow-timeout" label="Wait up to" value={String(target.timeoutDays ?? 7)} onChange={v => patch({ timeoutDays: Number(v) })}>{Array.from({ length: 30 }, (_, i) => <option key={i + 1} value={i + 1}>{i + 1} {i ? "days" : "day"}</option>)}</Select></Field> : null}
  </>;
  const inspection = edit ? <div className="email-flow-inspector" onKeyDown={e => { if (e.key === "Escape") { e.preventDefault(); focus(selected || "trigger"); } }}>{fields}</div> : null;
  function insertion(index: number) { return edit && !fixed ? <div className="email-flow-insert" key={`insert-${index}`}><AddButton {...attrs(`insert-${index}`)} label={`Insert step ${index + 1}`} style={{ minWidth: 28, minHeight: 28 }} onClick={() => setInserting(inserting === index ? null : index)} />{inserting === index ? <div className="email-flow-add" role="group" aria-label="Add a step">{([['send', 'Send an email'], ['confirm', 'Ask to confirm'], ['wait', 'Wait']] as const).map(([kind, label]) => <button type="button" className="os-button" key={kind} onClick={() => { const added = newStep(kind as StepKind); onChange?.(insertStep(steps, index, added)); setSelected(added.id); setFocused(added.id); setInserting(null); requestAnimationFrame(() => focus(added.id)); }}>{label}</button>)}</div> : null}</div> : null; }
  return <div ref={root} className="email-flow" data-wide={wide || undefined} data-proposal={proposal || undefined}>
    <div className="email-flow-spine" role="group" aria-label={proposal ? "Proposed automation flow" : "Automation flow"}>
      {issues.filter(i => !i.stepId).map((i, n) => <p key={n} className="email-flow-issue">{i.text}</p>)}
      <div className="email-flow-node"><button type="button" {...attrs("trigger")} className="email-flow-trigger" onClick={() => select("trigger")} aria-label={`${trigger}${counts.get("trigger") ? `, ${counts.get("trigger")}` : ""}`}><strong>{trigger}</strong>{triggerDetail}{counts.get("trigger") ? <span className="os-caption">{counts.get("trigger")}</span> : null}</button>{!wide && (!selected || selected === "trigger") ? inspection : null}</div>
      {steps.map((step, index) => {
        const template = templates.find(t => t.id === step.templateId)?.name ?? "";
        const sender = senders.find(s => s.id === step.senderIdentityId)?.address ?? (isOperatorOrganization(accountId) ? "This cluster's sender" : "");
        const sentence = stepSentence(step, { template });
        return <div key={step.id}>{insertion(index)}<div className="email-flow-node" data-kind={step.kind}>
          <button type="button" {...attrs(step.id)} className="email-flow-card" data-selected={selected === step.id || undefined} onClick={() => select(step.id)} aria-label={`${sentence}${sender && step.kind !== "wait" ? `, sender ${sender}` : ""}${counts.get(step.id) ? `, ${counts.get(step.id)}` : ""}`}>
            {step.kind === "wait" ? <Clock size={16} aria-hidden /> : <Mail size={18} aria-hidden />}<span><strong>{sentence}</strong>{step.kind !== "wait" && sender ? <span className="os-caption os-mono">{sender}</span> : null}{counts.get(step.id) ? <span className="os-caption">{counts.get(step.id)}</span> : null}</span>
          </button>
          {issues.filter(i => i.stepId === step.id).map((i, n) => <p key={n} className="email-flow-issue">{i.text}</p>)}
          {edit && !fixed && selected === step.id ? <div className="email-flow-tools">{index > 0 ? <button type="button" className="os-icon-button" aria-label="Move up" title="Move up" onClick={() => move(index, -1)}><ArrowUp size={16} /></button> : null}{index < steps.length - 1 ? <button type="button" className="os-icon-button" aria-label="Move down" title="Move down" onClick={() => move(index, 1)}><ArrowDown size={16} /></button> : null}<button type="button" className="os-icon-button" aria-label="Remove" title="Remove" onClick={() => remove(index)}><Trash2 size={16} /></button></div> : null}
          {!wide && selected === step.id ? inspection : null}
        </div></div>;
      })}
      {insertion(steps.length)}<div className="email-flow-node email-flow-end"><button type="button" {...attrs("end")} className="email-flow-trigger" onClick={() => select("end")}>End{counts.get("end") ? <span className="os-caption">{counts.get("end")}</span> : null}</button>{!wide && selected === "end" ? inspection : null}</div>
    </div>{wide ? inspection : null}
  </div>;
}

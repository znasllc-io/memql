import { useId, useState } from "react";
import { ArrowDown, ArrowUp, Plus, X } from "lucide-react";
import { Button, Caption, Field, Input, Notice, Select } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { ActivityTarget } from "../../kit/SemanticActivity";
import type { TaskPolicy } from "./taskPolicies";

export interface PolicyDraft { name: string; description: string; primary: string; fallbacks: string[]; revision: number; action?: "save" | "reset"; explanation?: string; existing?: boolean }
export const SOURCE_OPTIONS = [{ value: "fleet:strongest", label: "Local model · strongest compatible" }, { value: "fleet:fastest", label: "Local model · fastest compatible" }, { value: "app:*", label: "App · any eligible app" }, { value: "federation:cheapest", label: "Federation · cheapest compatible" }, { value: "federation:strongest", label: "Federation · strongest compatible" }, { value: "embedder:active", label: "Embeddings · active binding" }];

function SourceField({ value, onChange, label, policies, disabled }: { value: string; onChange: (next: string) => void; label: string; policies: readonly TaskPolicy[]; disabled: boolean }) {
  const id = useId();
  const custom = !SOURCE_OPTIONS.some(s => s.value === value);
  return <fieldset disabled={disabled} className="fleet-source-field"><Field label={label}><Select id={`${id}-type`} label={label} value={custom ? "custom" : value} onChange={next => onChange(next === "custom" ? "" : next)}>
    {SOURCE_OPTIONS.map(source => <option key={source.value} value={source.value}>{source.label}</option>)}<option value="custom">Specific model, app or policy…</option>
  </Select></Field>{custom ? <><Input id={`${id}-value`} disabled={disabled} label={`${label} source`} value={value} placeholder="fleet:qwen3.8:27b or app:codex" onChange={onChange} /><details><summary>Source syntax</summary><p>Use fleet:modelId, app:appId, federation:providerName, a registered provider name, or policy:policyName. IDs are exact; a configured source may still be unavailable.</p>{policies.length ? <p>Policies: {policies.map(p => p.name).join(", ")}</p> : null}</details></> : null}</fieldset>;
}

/** Same editor and write contract in Fleet and in an Ask proposal. */
export function PolicyEditor({ seed, policies = [], existing = false, onSaved, onCancel }: { seed: PolicyDraft; policies?: readonly TaskPolicy[]; existing?: boolean; onSaved?: () => void; onCancel?: () => void }) {
  const connection = useOsConnection();
  const id = useId();
  const [draft, setDraft] = useState(seed);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const update = (patch: Partial<PolicyDraft>) => { setDraft(d => ({ ...d, ...patch })); setError(""); };
  const sources = [draft.primary, ...draft.fallbacks];
  const valid = (existing || !policies.some(p => p.name === draft.name)) && /^[a-z][A-Za-z0-9]*$/.test(draft.name) && sources.every(s => s.trim()) && new Set(sources.map(s => s.trim())).size === sources.length;
  function move(index: number, direction: -1 | 1) { const next = [...sources]; const destination = index + direction; [next[index], next[destination]] = [next[destination]!, next[index]!]; update({ primary: next[0]!, fallbacks: next.slice(1) }); }
  async function save() {
    if (!connection || busy) return;
    setBusy(true); setError("");
    try {
      if (draft.action === "reset") await connection.query.routingPolicyReset({ name: draft.name, expectedRevision: draft.revision });
      else await connection.query.routingPolicySave({ name: draft.name, description: draft.description, primary: draft.primary.trim(), fallbacks: draft.fallbacks.map(s => s.trim()), expectedRevision: draft.revision });
      setSaved(true); onSaved?.();
    } catch (error) { setError(error instanceof Error ? error.message : String(error)); } finally { setBusy(false); }
  }
  if (saved) return <Notice sentence={draft.action === "reset" ? `${draft.name} now uses its shipped defaults.` : `${draft.name} was saved.`} />;
  return <ActivityTarget target="fleet:policy:draft" className="fleet-policy-editor">
    <h4>{draft.action === "reset" ? `Restore ${draft.name}` : existing ? `Edit ${draft.name}` : "Create a policy"}</h4>
    <p className="fleet-scope-note"><strong>Cluster-wide change.</strong> Every rule referencing this policy uses the saved source order.</p>
    {draft.explanation ? <Caption>{draft.explanation}</Caption> : null}
    {draft.action === "reset" ? <p>This removes your customization and restores the original shipped settings.</p> : <>
      <div className="fleet-policy-fields"><Field label="Policy name"><Input id={`${id}-name`} label="Policy name" value={draft.name} disabled={existing || busy} onChange={name => update({ name })} placeholder="myLocalPolicy" /></Field><Field label="Description"><Input id={`${id}-description`} label="Policy description" value={draft.description} disabled={busy} onChange={description => update({ description })} /></Field></div>
      <ol className="fleet-source-editor" aria-label="Policy source order">{sources.map((source, index) => <li key={index}>
        <span className="fleet-source-number">{index + 1}</span><SourceField disabled={busy} label={index === 0 ? "Preferred source" : `Fallback ${index}`} value={source} policies={policies} onChange={value => { const next = [...sources]; next[index] = value; update({ primary: next[0]!, fallbacks: next.slice(1) }); }} />
        <div className="fleet-source-move"><Button ariaLabel={`Move source ${index + 1} earlier`} disabled={busy || index === 0} onClick={() => move(index, -1)}><ArrowUp size={14} aria-hidden /></Button><Button ariaLabel={`Move source ${index + 1} later`} disabled={busy || index === sources.length - 1} onClick={() => move(index, 1)}><ArrowDown size={14} aria-hidden /></Button>{index > 0 ? <Button ariaLabel={`Remove fallback ${index}`} disabled={busy} onClick={() => update({ fallbacks: draft.fallbacks.filter((_, i) => i !== index - 1) })}><X size={14} aria-hidden /></Button> : null}</div>
      </li>)}</ol>
      <Button disabled={busy || draft.fallbacks.length >= 16} onClick={() => update({ fallbacks: [...draft.fallbacks, ""] })}><Plus size={14} aria-hidden /> Add fallback</Button>
      <p className="os-caption">Sources are tried in order when compatible and available. Federation can incur vendor charges. An app entry does not grant app or operating-system permissions.</p>
    </>}
    {error ? <Notice tone="error" sentence="The policy was not saved." detail={error} next="If the revision changed, cancel and refresh before editing again." /> : null}
    <div className="os-head-actions"><Button tone="primary" busy={busy} disabled={!connection || (draft.action !== "reset" && !valid)} onClick={() => void save()}>{draft.action === "reset" ? "Restore shipped defaults" : "Save policy"}</Button>{onCancel ? <Button disabled={busy} onClick={onCancel}>Cancel</Button> : null}</div>
  </ActivityTarget>;
}

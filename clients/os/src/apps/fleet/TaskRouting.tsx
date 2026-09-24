import { AddButton } from "../../kit/AddButton";
import { PolicyEditor, type PolicyDraft } from "./PolicyEditor";
import { PolicyChain } from "./PolicyChain";
import { useState } from "react";
import { FleetTabs, RefreshButton } from "./FleetControls";
import { Button, Caption, EmptyState, Head, Notice, Select, Subhead, RecordList, RecordRow } from "../../kit";
import { InfoDetail } from "../../kit/InfoDetail";
import { ActivityTarget } from "../../kit/SemanticActivity";
import { useSession } from "../../chrome/access";
import { accessAdmits } from "../../system/registry";
import { RuleFieldsPanel } from "../settings/RuleFieldsPanel";
import { RULES_SECTION_RESOURCE } from "../settings/RulesSection";
import { ruleSentence, rulesInOrder, useRuleActions, useRules, type RuleRow } from "../settings/rulesFacts";
import { policyDescription, useTaskPolicies } from "./taskPolicies";

export function TaskRouting(_props: { onBack?: () => void } = {}) {
  const { accessEpoch } = useSession(); // Re-render when the effective grants change.
  void accessEpoch;
  const allowed = accessAdmits(RULES_SECTION_RESOURCE);
  if (!allowed) return <div className="fleet-task-routing"><Head title="Policies" /><EmptyState title="Policy access required">Ask your cluster administrator for access to routing rules to view or edit policies.</EmptyState></div>;
  return <TaskRoutingEditor />;
}

function TaskRoutingEditor() {
  const [epoch, setEpoch] = useState(0);
  const [tab, setTab] = useState<"policies" | "rules">("policies");
  const [policyName, setPolicyName] = useState("");
  const [policyDraft, setPolicyDraft] = useState<PolicyDraft | null>(null);
  const catalog = useTaskPolicies(epoch);
  const rules = useRules(true);
  const actions = useRuleActions(rules.reload);
  const [selected, select] = useState("");
  const [editing, edit] = useState<{ seed: RuleRow | null } | null>(null);
  const [task, setTask] = useState("chat");
  const [removing, remove] = useState("");
  const ordered = rulesInOrder(rules.rules);
  const selectedPolicy = catalog.policies.find(p => p.name === policyName) ?? catalog.policies.find(p => p.name === "localFirst") ?? catalog.policies[0];
  const rule = ordered.find(r => r.name === selected) ?? ordered[0];
  const policy = catalog.policies.find(p => p.name === rule?.policy);
  function create() {
    edit({ seed: { name: "", when: task === "custom" ? {} : { modality: task }, level: "", policy: catalog.policies.find(p => p.name === "localFirst")?.name ?? catalog.policies[0]?.name ?? "", precedence: Math.max(0, ...rules.rules.filter(r => !r.locked).map(r => r.precedence)) + 10, onUnavailable: "park", excludes: [], locked: false, described: "" } });
  }
  return <div className="fleet-task-routing">
    <div className="fleet-section-header">
    <Head title="Policies" meta={tab === "policies" ? !catalog.loading && !catalog.error ? catalog.policies.length : undefined : !rules.loading && !rules.error && rules.supported ? ordered.length : undefined}>{tab === "policies" && !policyDraft ? <AddButton label="Create policy" disabled={catalog.loading || !!catalog.error} onClick={() => setPolicyDraft({ name: "", description: "", primary: "fleet:strongest", fallbacks: [], revision: catalog.policies[0]?.revision ?? 0 })} /> : null}<RefreshButton label="Refresh policies" onClick={() => { setEpoch(e => e + 1); rules.reload(); }} busy={catalog.loading || rules.loading} /></Head>
    <FleetTabs label="Routing composition" value={tab} onChange={setTab} options={[["policies", "Policies"], ["rules", "Task rules"]]} />
    </div>
    <p className="fleet-task-intro">Match a kind of call to a policy’s preferred source and fallbacks.</p>
    <div className="fleet-scope-note"><strong>Cluster-wide routing</strong><span>Changes affect every matching call.</span><InfoDetail title="Routing scope and order"><p>Shipped rules with conditions run first, then custom rules by descending precedence. The conditionless shipped default runs last. A custom rule cannot override a matching shipped rule.</p><p>A policy tries its sources in order. The router still checks compatibility and availability. Your personal model ordering and app delegation policy remain separate Fleet controls.</p></InfoDetail></div>
    {catalog.error || rules.error ? <Notice tone="error" sentence="Routing could not be fully read." detail={catalog.error || rules.error} /> : null}

    <div hidden={tab !== "policies"}>{policyDraft ? <PolicyEditor key={`${policyDraft.name}:${policyDraft.action}`} seed={policyDraft} policies={catalog.policies} existing={catalog.policies.some(p => p.name === policyDraft.name)} onCancel={() => setPolicyDraft(null)} onSaved={() => { setPolicyDraft(null); setEpoch(e => e + 1); rules.reload(); }} /> : <>

      <div className="fleet-policy-workspace fleet-policy-composition"><Select id="fleet-policy-picker" label="Policy to compose" value={selectedPolicy?.name ?? ""} onChange={setPolicyName}>{catalog.policies.map(p => <option key={p.name} value={p.name}>{p.name} · {p.customized ? "Customized" : p.shipped ? "Shipped" : "Custom"}</option>)}</Select>
      {selectedPolicy ? <ActivityTarget target={`fleet:policy:${selectedPolicy.name}`} className="fleet-policy-detail"><h4>{selectedPolicy.name}</h4><p>{policyDescription(selectedPolicy)}</p><PolicyChain policy={selectedPolicy} />
      <div className="os-head-actions">{!selectedPolicy.protected ? <Button onClick={() => setPolicyDraft({ name: selectedPolicy.name, description: policyDescription(selectedPolicy), primary: selectedPolicy.primary || selectedPolicy.chain[0] || "", fallbacks: selectedPolicy.fallbacks ?? selectedPolicy.chain.slice(1), revision: selectedPolicy.revision ?? 0 })}>Edit sources</Button> : null}{selectedPolicy.customized ? <Button onClick={() => setPolicyDraft({ name: selectedPolicy.name, description: "", primary: "", fallbacks: [], revision: selectedPolicy.revision ?? 0, action: "reset" })}>Restore shipped defaults</Button> : null}</div>
      {selectedPolicy.protected ? <Caption>The active embedding binding is protected. Changing it requires an embedding migration, not a fallback edit.</Caption> : null}
      <Subhead meta={!rules.loading && !rules.error && rules.supported ? ordered.filter(r => r.policy === selectedPolicy.name).length : undefined}>Used by task rules</Subhead><RecordList as="ul" label="Rules using this policy">{ordered.filter(r => r.policy === selectedPolicy.name).map(r => <RecordRow key={r.name} name={r.name} onOpen={() => { select(r.name); setTab("rules"); }} />)}</RecordList>{!ordered.some(r => r.policy === selectedPolicy.name) ? <Caption>No task rule currently uses this policy.</Caption> : null}
      </ActivityTarget> : catalog.loading ? <Caption>Reading policies…</Caption> : !catalog.error ? <EmptyState title="No policies yet">Create a policy to choose a preferred source and fallbacks for your tasks.</EmptyState> : null}</div>
    </>}</div><div hidden={tab !== "rules"}>{editing ? <ActivityTarget target="fleet:task-rule:draft"><RuleFieldsPanel actions={actions} seed={editing.seed} existing={ordered} policies={catalog.policies} onActivated={() => { edit(null); actions.clear(); }} onCancel={() => { edit(null); actions.clear(); }} /></ActivityTarget> : <>
      <div className="fleet-task-create"><Select id="fleet-task-kind" label="Kind of task" value={task} onChange={setTask}>{[{id:"chat",label:"Chat"},{id:"streamingChat",label:"Streaming chat"},{id:"tools",label:"Tool calls"},{id:"streamingTools",label:"Streaming tool calls"},{id:"structured",label:"Structured output"},{id:"vision",label:"Vision"},{id:"speech",label:"Speech"},{id:"transcribe",label:"Transcription"}].map(kind => <option key={kind.id} value={kind.id}>{kind.label}</option>)}<option value="custom">Custom conditions</option></Select><Button tone="primary" disabled={!rules.supported || !actions.supported || catalog.loading || !!catalog.error || !!rules.error} onClick={create}>Add task rule</Button></div>
      {rules.loading ? <Caption>Reading task rules.</Caption> : <div className="fleet-policy-workspace"><div className="fleet-rule-picker" aria-label="Task rules">{ordered.map(r => <button type="button" key={r.name} aria-pressed={rule?.name === r.name} onClick={() => { select(r.name); remove(""); }}><strong>{r.name}</strong><small>{r.locked ? "Shipped" : "Custom"} · {Object.entries(r.when).map(([k, v]) => `${k}: ${v || "empty"}`).join(", ") || "Default for other calls"}</small></button>)}</div>
        {rule ? <ActivityTarget target={`fleet:task-rule:${rule.name}`} className="fleet-policy-detail"><h4>{rule.name}</h4><p>{ruleSentence(rule)}</p>{policy ? <PolicyChain policy={policy} /> : <Caption>Policy source order is unavailable.</Caption>}
          <p className="os-caption">{rule.onUnavailable === "park" ? "When all sources are unavailable, wait for a person." : "When all sources are unavailable, the router may step down a level."}</p>
          {!rule.locked ? <div className="os-head-actions"><Button onClick={() => edit({ seed: rule })}>Edit task rule</Button>{removing === rule.name ? <><span>Remove {rule.name}?</span><Button onClick={() => remove("")}>Keep rule</Button><Button tone="danger" busy={actions.state.busy} onClick={() => { void actions.retire(rule.name, rule.revision).then(ok => { if (ok) remove(""); }); }}>Confirm removal</Button></> : <Button tone="quiet" onClick={() => remove(rule.name)}>Remove rule</Button>}</div> : <Caption>This shipped rule cannot be edited.</Caption>}
        </ActivityTarget> : <EmptyState title={rules.supported ? "No task rules yet" : "Task rules unavailable"}>{rules.supported ? "Add a rule to send a kind of task to a policy." : "This cluster needs task-rule support before you can manage rules here."}</EmptyState>}</div>}
    </>}</div>
    {actions.state.message ? <Notice tone={actions.state.failed ? "error" : "info"} sentence={actions.state.message} /> : null}
    <details className="fleet-routing-limits"><summary>Supported sources and current limits</summary><p>Create policies here, edit their preferred and fallback sources, or restore a customized shipped policy. Saved changes apply throughout the cluster. A source such as app:* means an eligible app, not any installed app.</p><p>Image generation through Codex is not available here. Embeddings follow the shipped active binding; fallback must preserve the vector space. Rules are cluster-wide; there is no per-user task-rule scope in this editor.</p></details>
  </div>;
}

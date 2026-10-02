import { useCallback, useEffect, useId, useState, type ReactNode } from "react";
import { Mail } from "lucide-react";
import { useSession } from "../../chrome/access";
import { AccountPicker } from "../../apps/accounts/AccountPicker";
import { useAccountOptions } from "../../apps/accounts/tie";
import { useDefaultOrganization } from "../../apps/accounts/organization";
import { Button, EmptyState, Fact, Facts, Field, Head, Input, Notice, Panel, RecordList, RecordRow, RecordListSkeleton, Subhead, useAppReach, useNow } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import { Wizard } from "../../kit/Wizard";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { roleAdmits } from "../../system/roles";
import { ClusterAzureEmail } from "./ClusterAzureEmail";
import { AzureEmailDomainRecords } from "./AzureEmailDomainRecords";
import { InfoDetail } from "../../kit/InfoDetail";
import { useAzureEmailCall, type AzureEmailPlan, type AzureEmailReply } from "./azureEmailSetup";
import "./azureEmail.css";

type ConnectionView = "list" | "detail" | "setup" | "cluster";
const PROVISION_INTERVAL_MS = 10_000;
const MAX_PROVISION_CHECKS = 60;
/** One cluster configuration; organizations own domains and sending identities. */
export function AzureEmailConnections(props: { manageCluster?: boolean; header?: ReactNode; onBack?: () => void; backLabel?: string; children?: ReactNode }) {
  const { access, config } = useSession();
  const top = props.header ?? <Head title="Email settings" back={props.onBack ? { label: "Senders", onSelect: props.onBack } : undefined}/>;
  if (!roleAdmits(access?.role || "", { any: ["owner", "developer"] })) return <div className="os-app-stack">{top}<Notice sentence="An owner or developer can configure cluster email and client domains."/>{props.children}</div>;
  return <EmailSettings key={`${config.domain}:${access?.userId}`} {...props} header={top}/>;
}

function EmailSettings({ manageCluster = true, header, backLabel = "Email settings", children }: { manageCluster?: boolean; header?: ReactNode; backLabel?: string; children?: ReactNode }) {
  const formID = useId();
  const settings = useAppReach("settings");
  const accounts = useAccountOptions();
  const initial = useDefaultOrganization(accounts);
  const [accountId, setAccountId] = useState("");
  const [view, setView] = useState<ConnectionView>("list");
  const [cluster, setCluster] = useState<AzureEmailReply | null>(null);
  const { call, error } = useAzureEmailCall("self");
  useEffect(() => { if (!accountId && initial) setAccountId(initial); }, [accountId, initial]);
  useEffect(() => { void call("clusterStatus").then(result => { if (result) setCluster(result); }); }, [call]);
  const name = accounts.find(row => row.id === accountId)?.name || accountId;
  if (manageCluster && view === "cluster" && cluster) return <ClusterAzureEmail state={cluster} onBack={() => setView("list")} onSaved={result => { setCluster(result); setView("list"); }}/>;
  const picker = <div className="azure-email-organization"><Field label="Organization"><AccountPicker id={`${formID}-organization`} label="Organization" required accounts={accounts} value={accountId} onChange={setAccountId}/></Field></div>;
  return <div className="os-app-stack azure-email-connections">
    {view === "list" ? <>{header}<section className="os-app-stack" aria-label="Cluster email">
      {manageCluster ? <Subhead>Cluster email</Subhead> : null}
      {!cluster && !error ? <RecordListSkeleton label="Reading cluster email configuration"/> : null}
      {error ? <Notice tone="error" sentence="Cluster email configuration could not be read." detail={error}><Button onClick={() => { void call("clusterStatus").then(result => { if (result) setCluster(result); }); }}>Try again</Button></Notice> : null}
      {cluster && manageCluster ? <Panel label="Cluster email configuration"><Facts><Fact label="Microsoft Azure" value={cluster.status === "connected" ? "Connected" : cluster.status === "reauthorize" ? "Sign-in required" : "Not configured"}/>{cluster.resourceGroup ? <Fact label="Resource group" value={cluster.resourceGroup}/> : null}</Facts>
        {cluster.capture ? <p className="os-caption">Test emails are captured in the Email app. Azure setup is optional.</p> : null}
        <div className="os-panel-actions"><Button onClick={() => setView("cluster")}>{cluster.status === "connected" ? "Manage configuration" : "Set up Azure"}</Button></div>
      </Panel> : null}
      {cluster && !manageCluster && cluster.status !== "connected" ? <Notice sentence={cluster.capture ? "Test emails are captured in the Email app." : "Cluster email needs setup."} detail="Configure Azure once in Settings → Connections → Email before adding client domains.">
        {settings.canOpenWindows ? <Button onClick={() => settings.open("connections", { provider: "email" })}>Open email settings</Button> : null}
      </Notice> : null}
    </section></> : null}
    {accountId ? <OrganizationEmail key={accountId} accountId={accountId} name={name} view={view} setView={setView} picker={picker} backLabel={backLabel} cluster={cluster}/> :
      <section className="os-app-stack"><Subhead>Sending domains</Subhead>{picker}</section>}
    {view === "list" ? children : null}
  </div>;
}

function OrganizationEmail({ accountId, name, view, setView, picker, backLabel, cluster }: {
  accountId: string; name: string; view: ConnectionView; setView: (value: ConnectionView) => void;
  picker: ReactNode; backLabel: string; cluster: AzureEmailReply | null;
}) {
  const formID = useId();
  const { call, busy, error } = useAzureEmailCall(accountId);
  const [state, setState] = useState<AzureEmailReply | null>(null);
  const [domain, setDomain] = useState("");
  const [emailService, setEmailService] = useState("");
  const [communicationService, setCommunicationService] = useState("");
  const [username, setUsername] = useState("news");
  const [displayName, setDisplayName] = useState(name);
  const [replyTo, setReplyTo] = useState("");
  const [disconnect, setDisconnect] = useState(false);
  const [operations, setOperations] = useState<AzureEmailReply["operations"]>(undefined);
  const [records, setRecords] = useState<AzureEmailReply["records"]>(undefined);
  const [provisionChecks, setProvisionChecks] = useState(0);
  const [startedAt, setStartedAt] = useState(Date.now);
  const [chosen, setChosen] = useState<string | null>(null);
  const apply = useCallback((result: AzureEmailReply | null) => {
    if (!result) return;
    setState(old => ({ ...old, ...result, planId: result.planId || old?.planId }));
    if (result.plan) { setDomain(result.plan.domain); setEmailService(result.plan.emailService); setCommunicationService(result.plan.communicationService); }
    if (result.replyTo) setReplyTo(result.replyTo);
    if (result.records) setRecords(result.records);
  }, []);
  useEffect(() => { void call("status").then(apply); }, [call, apply]);
  const perform = useCallback((action: string, options: Record<string, unknown> = {}) => { void call(action, { ...options, planId: state?.planId || "" }).then(result => { apply(result); if (result?.status === "ready") setView("detail"); }); }, [call, state?.planId, apply, setView]);
  const connected = cluster?.status === "connected";
  const provisioning = state?.status === "provisioning";
  const phase = state?.status === "verified" || state?.status === "ready" ? "sender" : state?.status === "dns" ? "dns" : "domain";
  const open = chosen ?? phase;
  useEffect(() => { setChosen(null); }, [phase, view]);
  useEffect(() => {
    if (view === "setup" && open === "dns" && connected && !records) void call("domainStatus").then(apply);
  }, [view, open, connected, records, call, apply]);
  const provisionPaused = provisionChecks >= MAX_PROVISION_CHECKS;
  const advancing = view === "setup" && provisioning && connected && !error && !provisionPaused;
  const now = useNow(advancing ? 1000 : 60_000);
  const elapsed = Math.max(0, Math.floor((now.getTime() - startedAt) / 1000));
  useEffect(() => { setProvisionChecks(0); setStartedAt(Date.now()); }, [state?.planId, view, provisioning]);
  useEffect(() => {
    // Only a persisted provisioning state carries the person's prior approval.
    // Never create from a plan alone, overlap requests, or retry a refusal.
    if (view !== "setup" || !advancing || busy || !state?.planId) return;
    const timer = window.setTimeout(() => {
      setProvisionChecks(count => count + 1);
      perform("provision", { confirmed: true });
    }, PROVISION_INTERVAL_MS);
    return () => window.clearTimeout(timer);
  }, [view, advancing, busy, state?.planId, provisionChecks, perform]);
  if (view === "list") return <section className="os-app-stack" aria-label="Sending domains">
    <div className="os-head"><Subhead>Sending domains</Subhead><div className="os-head-actions">{state && ["unconfigured", "disconnected"].includes(state.status) && !error && connected ? <AddButton label="Add sending domain" onClick={() => setView("setup")}/> : null}</div></div>
    {picker}
    {!state && !error ? <RecordListSkeleton label="Reading sending domain"/> : null}
    {error ? <Notice tone="error" sentence="Sending domain could not be read." detail={error}><Button onClick={() => { void call("status").then(apply); }}>Try again</Button></Notice> : null}
    {state && !error ? ["unconfigured", "disconnected"].includes(state.status) ? <EmptyState icon={Mail} title="No sending domain">{connected ? "Add a verified domain for this organization." : "Set up cluster email before adding a domain."}</EmptyState> :
      <RecordList label="Sending domains"><RecordRow icon={<Mail size={18} aria-hidden/>} name={state.plan?.domain || name} secondary={state.sender || "Microsoft Azure"} state={state.status === "ready" ? "Ready" : "Setup incomplete"} tone={state.status === "ready" ? "accent" : "warn"} current onOpen={() => setView(state.status === "ready" ? "detail" : "setup")} label={`Manage email for ${name}`}/></RecordList> : null}
  </section>;
  if (view === "detail") return <section className="os-action-pane">
    <Head title={state?.plan?.domain || "Sending domain"} back={{ label: backLabel, onSelect: () => setView("list") }}/>
    <div className="os-action-body os-app-stack"><Panel label="Sending domain"><Facts><Fact label="Organization" value={name}/><Fact label="Sender" value={state?.sender || "Not configured"}/><Fact label="Replies to" value={state?.replyTo || "No reply mailbox configured"}/></Facts></Panel>
      {!connected ? <Notice sentence="Cluster Azure setup needs attention."/> : null}
      <section className="os-app-stack" aria-label="Azure send processing"><div className="os-head"><Subhead>Send processing</Subhead><Button busy={busy} onClick={() => { void call("operations").then(result => { if (result) setOperations(result.operations || []); }); }}>{operations ? "Refresh" : "Check sends"}</Button></div>
        <p className="os-caption">Azure processing status does not confirm inbox delivery. Delivery and bounce feedback is not connected yet.</p>
        {operations?.length === 0 ? <p className="os-caption">No Azure sends recorded for this organization.</p> : null}
        {operations?.map(operation => <Panel key={operation.operationId} label="Azure send"><Facts><Fact label="Requested" value={new Date(operation.submittedAt).toLocaleString()}/><Fact label="Processing" value={sendOperationLabel(operation.status)}/></Facts>{operation.detail ? <p className="os-caption">{operation.detail}</p> : null}</Panel>)}
      </section>
      {disconnect ? <Notice tone="warn" sentence="Disconnect this organization's sending domain?" detail="Its new sends will stop. Other organizations, Azure resources and DNS records remain."/> : null}
      {error ? <Notice tone="error" sentence={error}/> : null}
    </div>
    <ActionBar state="Ready" tone="live" acts={disconnect ? [
      { label: "Cancel", text: true, busy, onAct: () => setDisconnect(false) },
      { label: "Disconnect domain", tone: "danger", busy, onAct: () => { void call("disconnect").then(result => { if (result) { apply(result); setDisconnect(false); setView("list"); } }); } },
    ] : [{ label: "Disconnect domain", text: true, busy, onAct: () => setDisconnect(true) }, ...(connected ? [{ label: "Manage senders", busy, onAct: () => setView("setup") }] : [])]}/>
  </section>;
  const acts: Act[] = [{ label: "Leave", text: true, onAct: () => setView("list") }];
  if (open !== phase) acts.push({ label: phase === "dns" ? "Continue to verification" : "Continue to sender", onAct: () => setChosen(null) });
  else if (!busy && connected) {
    if (phase === "domain") {
      if (state?.status === "planned") acts.push({ label: "Create Azure resources", onAct: () => perform("provision", { confirmed: true }) });
      else if (provisioning) {
        if (error || provisionPaused) acts.push({ label: "Continue setup", onAct: () => { setProvisionChecks(0); setStartedAt(Date.now()); perform("provision", { confirmed: true }); } });
      } else if (domain && emailService && communicationService) acts.push({ label: "Review domain", onAct: () => { void call("prepare", { domain, emailService, communicationService }).then(apply); } });
    }
    else if (phase === "dns") acts.push({ label: "Check domain records", onAct: () => perform("verify") });
    else if (phase === "sender" && username.trim() && displayName.trim()) acts.push({ label: "Save sender", onAct: () => perform("sender", { username, displayName, replyTo }) });
  }
  const order = ["domain", "dns", "sender"];
  const plan: Partial<AzureEmailPlan> = state?.plan || {};
  const hasPlan = !!state?.plan && !["unconfigured", "disconnected"].includes(state.status);
  return <Wizard icon={<Mail size={20}/>} title="Add sending domain" lead={`Send email for ${name}.`} label="Sending domain" open={open} onOpen={setChosen} back={{ label: backLabel, onSelect: () => setView("list") }} status={{ word: busy ? "Working" : !connected ? "Cluster setup required" : error ? "Needs attention" : provisioning ? provisionPaused ? "Setup paused" : "Preparing email" : phase === "dns" ? "Waiting for DNS" : phase === "sender" ? "Choose a sender" : state?.status === "planned" ? "Review domain" : "Enter domain", tone: busy || advancing ? "busy" : "paused", meta: advancing ? `${Math.floor(elapsed / 60)}:${String(elapsed % 60).padStart(2, "0")}` : undefined }} acts={acts} notices={error ? <Notice tone="error" sentence={error}/> : !connected ? <Notice sentence="Set up the cluster's Azure configuration before continuing."/> : provisioning && provisionPaused ? <Notice sentence="Azure is taking longer than expected." detail="Continue setup to check the same resources again."/> : undefined} steps={[
    { id: "domain", name: "Domain", answer: hasPlan ? plan.domain : undefined, body: <div className="os-app-stack">{hasPlan ? <><p>{plan.domain}</p><InfoDetail title="Azure resources"><Facts><Fact label="Resource group" value={plan.resourceGroup || ""}/><Fact label="Data location" value={plan.dataLocation || ""}/></Facts></InfoDetail>{state?.status === "planned" ? <p className="os-caption">Azure email usage is billed to the cluster's selected subscription. Creating resources does not send mail.</p> : advancing ? <p className="os-caption">MemQL continues automatically while this page is open. Domain verification appears when Azure is ready.</p> : null}</> : <>
      <Field label="Email domain"><Input id={`${formID}-domain`} label="Email domain" placeholder="client.com" value={domain} onChange={value => { setDomain(value); const base = value.toLowerCase().replace(/[^a-z0-9-]/g, "-").slice(0, 45); setEmailService(`${base}-email`); setCommunicationService(`${base}-delivery`); }}/></Field>
      <details><summary>Resource names</summary><Field label="Email service"><Input id={`${formID}-email-service`} label="Email service" value={emailService} onChange={setEmailService}/></Field><Field label="Delivery service"><Input id={`${formID}-delivery-service`} label="Delivery service" value={communicationService} onChange={setCommunicationService}/></Field></details>
    </>}</div> },
    { id: "dns", name: "Verify domain", answer: phase === "sender" ? "Verified" : records?.length ? `${records.filter(record => record.status === "Verified").length} of ${records.length} verified` : undefined, body: <div className="os-app-stack"><AzureEmailDomainRecords domain={plan.domain || domain} records={records}/></div> },
    { id: "sender", name: "Sender", body: <div className="os-app-stack"><Field label="Sender address"><Input id={`${formID}-sender`} label="Sender address" value={username} onChange={setUsername}/><span className="os-caption">@{plan.domain}</span></Field><Field label="Display name"><Input id={`${formID}-name`} label="Display name" value={displayName} onChange={setDisplayName}/></Field><Field label="Reply mailbox"><Input id={`${formID}-reply`} label="Reply mailbox" placeholder={`help@${plan.domain}`} value={replyTo} onChange={setReplyTo}/></Field><p className="os-caption">Replies need a mailbox that already receives mail.</p></div> },
  ].map(step => ({ ...step, state: step.id === phase ? busy || advancing ? "current" as const : error ? "stopped" as const : "open" as const : order.indexOf(step.id) < order.indexOf(phase) ? "done" as const : "ahead" as const }))}/>;
}

export function sendOperationLabel(status: string): string {
  return ({ submitting: "Checking acceptance", unknown: "Checking acceptance", accepted: "Accepted by Azure", running: "Processing", succeeded: "Processing completed", failed: "Processing failed", canceled: "Processing canceled", rejected: "Request refused", throttled: "Waiting for Azure", unconfirmed: "Unconfirmed" } as Record<string, string>)[status] || "Unconfirmed";
}

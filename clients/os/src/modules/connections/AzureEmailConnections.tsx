import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from "react";
import { Mail } from "lucide-react";
import { useSession } from "../../chrome/access";
import { useOsConnection } from "../../live/connection";
import { AccountPicker } from "../../apps/accounts/AccountPicker";
import { useAccountOptions } from "../../apps/accounts/tie";
import { useDefaultOrganization } from "../../apps/accounts/organization";
import { Button, EmptyState, Fact, Facts, Field, Head, Input, Notice, Panel, RecordList, RecordRow, RecordListSkeleton, Select, Subhead } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import "./azureEmail.css";
import { Wizard } from "../../kit/Wizard";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { flatten } from "../../kit/rows";
import { roleAdmits } from "../../system/roles";
interface Plan {
    createResourceGroup?: boolean;
    resourceGroupLocation?: string;
    subscriptionId: string;
    resourceGroup: string;
    emailService: string;
    communicationService: string;
    domain: string;
    dataLocation: string;
}
interface Choice {
    id: string;
    name: string;
}
interface DNSRecord {
    purpose: string;
    name: string;
    type: string;
    value: string;
    status: string;
}
interface SendOperation { operationId: string; status: string; detail: string; submittedAt: string; checkedAt: string }
interface Reply {
    applicationReady?: boolean;
    operations?: SendOperation[];
    planId?: string;
    status: string;
    sessionId?: string;
    userCode?: string;
    verificationUri?: string;
    interval?: number;
    plan?: Plan;
    choices?: Choice[];
    records?: DNSRecord[];
    sender?: string;
    replyTo?: string;
}
const LOCATIONS = ["Africa", "Asia Pacific", "Australia", "Brazil", "Canada", "Europe", "France", "Germany", "India", "Japan", "Korea", "Norway", "Switzerland", "United Arab Emirates", "United Kingdom", "United States"];
/** Shared by Campaigns and Settings; this configures the organization's one
 * backend connection. Neither app keeps provider credentials or a sender copy. */
type ConnectionView = "list" | "detail" | "setup";
export function AzureEmailConnections({ header, onBack, backLabel = "Email connections", children }: {
    header?: ReactNode;
    onBack?: () => void;
    backLabel?: string;
    children?: ReactNode;
}) {
    const formID = useId();
    const accounts = useAccountOptions();
    const initial = useDefaultOrganization(accounts);
    const [accountId, setAccountId] = useState("");
    const [view, setView] = useState<ConnectionView>("list");
    const { access, config } = useSession();
    useEffect(() => { if (!accountId && initial) setAccountId(initial); }, [accountId, initial]);
    const allowed = roleAdmits(access?.role || "", { any: ["owner", "developer"] });
    const name = accounts.find(row => row.id === accountId)?.name || accountId;
    const top = header ?? <Head title="Email connections" back={onBack ? { label: "Senders", onSelect: onBack } : undefined}/>;
    const picker = <div className="azure-email-organization"><Field label="Organization"><AccountPicker id={`${formID}-email-organization`} label="Organization" required accounts={accounts} value={accountId} onChange={setAccountId}/></Field></div>;
    if (!allowed) return <div className="os-app-stack">{top}<Notice sentence="An owner or developer can connect your organization's email domain."/>{children}</div>;
    return <div className="os-app-stack azure-email-connections">
      {view === "list" ? top : null}
      {accountId ? <OrganizationEmail key={`${config.domain}:${access?.userId}:${accountId}`} accountId={accountId} name={name} view={view} setView={setView} picker={picker} backLabel={backLabel}/> :
        <section className="os-app-stack" aria-label="Email connections"><Subhead>Email connections</Subhead>{picker}<p className="os-caption">Choose an organization to connect its email domain with Microsoft Azure.</p></section>}
      {view === "list" ? children : null}
    </div>;
}
function OrganizationEmail({ accountId, name, view, setView, picker, backLabel }: {
    accountId: string;
    name: string;
    view: ConnectionView;
    setView: (value: ConnectionView) => void;
    picker: ReactNode;
    backLabel: string;
}) {
    const editing = view === "setup";
    const edit = (value: boolean) => setView(value ? "setup" : "list");
    const formID = useId();
    const connection = useOsConnection();
    const { access, config } = useSession();
    const storageKey = `memql:azure-email:${config.domain}:${access?.userId}:${accountId}`;
    const [state, setState] = useState<Reply | null>(null);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");
    const [session, setSession] = useState(() => { try {
        return sessionStorage.getItem(storageKey) || "";
    }
    catch {
        return "";
    } });
    const [grant, setGrant] = useState<Reply | null>(null);
    const [signedIn, setSignedIn] = useState(false);
    const [subscriptions, setSubscriptions] = useState<Choice[]>([]);
    const [directories, setDirectories] = useState<Choice[]>([]);
    const [directory, setDirectory] = useState("");
    const [groups, setGroups] = useState<Choice[]>([]);
    const [locations, setLocations] = useState<Choice[]>([]);
    const [plan, setPlan] = useState<Plan>({ subscriptionId: "", resourceGroup: "", emailService: "", communicationService: "", domain: "", dataLocation: "United States" });
    const [records, setRecords] = useState<DNSRecord[]>([]);
    const [username, setUsername] = useState("news");
    const [displayName, setDisplayName] = useState(name);
    const [replyTo, setReplyTo] = useState("");
    const [disconnect, setDisconnect] = useState(false);
    const [operations, setOperations] = useState<SendOperation[] | null>(null);
    const generation = useRef(0);
    const inFlight = useRef<AbortController | null>(null);
    useEffect(() => { generation.current++; return () => { generation.current++; inFlight.current?.abort(); inFlight.current = null; }; }, [connection]);
    const call = useCallback(async (action: string, options: Record<string, unknown> = {}, sessionId = session): Promise<Reply | null> => {
        if (!connection || inFlight.current)
            return null;
        const epoch = generation.current;
        const request = new AbortController();
        inFlight.current = request;
        setBusy(true);
        setError("");
        try {
            const parameters = ["provision", "verify", "sender"].includes(action) ? { ...options, planId: state?.planId || "" } : options;
            const response = await connection.query.emailAzureSetup({ accountId, action, sessionId, options: parameters }, { signal: request.signal });
            const row = response.rows()[0];
            const result = row ? flatten(row) as unknown as Reply : undefined;
            if (epoch !== generation.current)
                return null;
            if (!result || typeof result.status !== "string")
                throw new Error("The cluster did not confirm this setup step.");
            if (result.status === "expired") {
                setSession("");
                setGrant(null);
                setSignedIn(false);
                try {
                    sessionStorage.removeItem(storageKey);
                }
                catch { }
                return null;
            }
            return result;
        }
        catch (reason) {
            if (epoch === generation.current)
                setError(reason instanceof Error ? reason.message : String(reason));
            return null;
        }
        finally {
            if (inFlight.current === request)
                inFlight.current = null;
            if (epoch === generation.current)
                setBusy(false);
        }
    }, [connection, accountId, session, state?.planId]);
    useEffect(() => { if (editing) return; void call("status").then(result => { if (result) {
        setState(result);
        if (result.plan?.domain)
            setPlan(result.plan);
        if (result.replyTo)
            setReplyTo(result.replyTo);
    } }); }, [connection, accountId, editing]);
    const keepSession = (id: string) => { setSession(id); try {
        if (id)
            sessionStorage.setItem(storageKey, id);
        else
            sessionStorage.removeItem(storageKey);
    }
    catch { /* the backend still holds this session */ } };
    const loadChoices = async (sessionId = session) => {
        const result = await call("subscriptions", {}, sessionId);
        if (result?.status === "connected") {
            setSubscriptions(result.choices || []);
            const tenants = await call("directories", {}, sessionId);
            if (tenants)
                setDirectories(tenants.choices || []);
            setSignedIn(true);
            if (state?.status === "dns") {
                const domain = await call("domainStatus", {}, sessionId);
                if (domain) {
                    setState(domain);
                    setRecords(domain.records || []);
                }
            }
        }
        else if (result?.status === "expired") {
            keepSession("");
            setSignedIn(false);
            setGrant(null);
        }
    };
    useEffect(() => {
        if (!editing || !session || signedIn || error)
            return;
        const timer = window.setTimeout(() => {
            void call("poll").then(async (result) => {
                if (result?.status === "connected") {
                    setGrant(null);
                    await loadChoices();
                }
                else if (result?.status === "expired") {
                    keepSession("");
                    setGrant(null);
                }
                else if (result)
                    setGrant(previous => ({ ...previous, ...result }));
            });
        }, (grant?.interval || 5) * 1000);
        return () => window.clearTimeout(timer);
    }, [editing, session, signedIn, grant, error, call]);
    const begin = async (tenantId = directory) => {
        const result = await call("begin", tenantId ? { tenantId } : {}, "");
        if (result?.sessionId) {
            keepSession(result.sessionId);
            setGrant(result);
            setSignedIn(false);
        }
    };
    const apply = (result: Reply | null) => { if (!result)
        return; setState(result); if (result.plan)
        setPlan(result.plan); if (result.records)
        setRecords(result.records); if (result.status === "ready") {
        edit(false);
        setGrant(null);
    } };
    if (view === "list") return <section className="os-app-stack" aria-label="Email connections">
      <div className="os-head"><Subhead>Email connections</Subhead><div className="os-head-actions">
        {state && ["unconfigured", "disconnected"].includes(state.status) && !error ? <AddButton label="Connect email" onClick={() => edit(true)}/> : null}
      </div></div>
      {picker}
      {!state && !error ? <RecordListSkeleton label="Reading email connection"/> : null}
      {error ? <Notice tone="error" sentence="Email connection could not be read." detail={error}><Button onClick={() => { void call("status").then(result => { if (result) setState(result); }); }}>Try again</Button></Notice> : null}
      {state && !error ? ["unconfigured", "disconnected"].includes(state.status) ?
        <EmptyState icon={Mail} title="No email connection">Use the plus button to sign in with Microsoft and connect your sending domain.</EmptyState> :
        <RecordList label="Email connections"><RecordRow icon={<Mail size={18} aria-hidden/>} name={state.plan?.domain || name} secondary={state.sender || "Microsoft Azure"} state={state.status === "ready" ? "Connected" : "Setup incomplete"} tone={state.status === "ready" ? "accent" : "warn"} current onOpen={() => setView("detail")} label={`Manage email for ${name}`}/></RecordList> : null}
    </section>;
    if (!editing) return <section className="os-action-pane">
      <Head title={state?.plan?.domain || "Email connection"} back={{ label: backLabel, onSelect: () => setView("list") }}/>
      <div className="os-action-body os-app-stack">
        <Panel label="Email connection"><Subhead>Microsoft Azure</Subhead><Facts>
          <Fact label="Organization" value={name}/><Fact label="Sender" value={state?.sender || "Not configured"}/>
          <Fact label="Replies to" value={state?.replyTo || "No reply mailbox configured"}/>
        </Facts></Panel>
        <section className="os-app-stack" aria-label="Azure send processing">
          <div className="os-head"><Subhead>Send processing</Subhead><Button busy={busy} onClick={() => { void call("operations").then(result => { if (result) setOperations(result.operations || []); }); }}>{operations ? "Refresh" : "Check sends"}</Button></div>
          <p className="os-caption">Azure processing status does not confirm inbox delivery. Delivery and bounce feedback is not connected yet.</p>
          {operations?.length === 0 ? <p className="os-caption">No Azure sends recorded for this organization.</p> : null}
          {operations?.map(operation => <Panel key={operation.operationId} label="Azure send"><Facts>
            <Fact label="Requested" value={new Date(operation.submittedAt).toLocaleString()}/>
            <Fact label="Processing" value={sendOperationLabel(operation.status)}/>
          </Facts>{operation.detail ? <p className="os-caption">{operation.detail}</p> : null}<details><summary>Reference</summary><code>{operation.operationId}</code></details></Panel>)}
        </section>
        {disconnect ? <Notice tone="warn" sentence="Disconnect this organization's email?" detail="New sends will stop. Azure resources and DNS records will remain."/> : null}
        {error ? <Notice tone="error" sentence={error}/> : null}
      </div>
      <ActionBar state={busy ? "Working" : state?.status === "ready" ? "Connected" : "Setup incomplete"} tone={busy ? "busy" : state?.status === "ready" ? "live" : "paused"} acts={disconnect ? [
        { label: "Cancel", text: true, busy, onAct: () => setDisconnect(false) },
        { label: "Disconnect", tone: "danger", busy, onAct: () => { void call("disconnect").then(result => { if (result) { setState(result); setDisconnect(false); setView("list"); } }); } },
      ] : [
        ...(state?.status === "ready" ? [{ label: "Disconnect", text: true, onAct: () => setDisconnect(true) }] : []),
        { label: state?.status === "ready" ? "Manage senders" : "Continue setup", busy, onAct: () => edit(true) },
      ]}/>
    </section>;
    const phase = !signedIn ? "signin" : state?.status === "verified" || state?.status === "ready" ? "sender" : state?.status === "dns" ? "dns" : "resources";
    const acts: Act[] = [{ label: "Back", text: true, busy, onAct: () => edit(false) }];
    if (!busy) {
        if (phase === "signin" && !session && state?.applicationReady !== false)
            acts.push({ label: "Sign in with Microsoft", onAct: () => { void begin(); } });
        else if (phase === "signin" && error)
            acts.push({ label: "Sign in again", onAct: () => { keepSession(""); setGrant(null); void begin(); } });
        else if (phase === "resources" && (state?.status === "planned" || state?.status === "provisioning"))
            acts.push({ label: state?.status === "planned" ? "Create Azure resources" : "Check provisioning", onAct: () => { void call("provision", { confirmed: true }).then(apply); } });
        else if (phase === "resources" && plan.subscriptionId && plan.resourceGroup && plan.domain && plan.emailService && plan.communicationService && (!plan.createResourceGroup || plan.resourceGroupLocation))
            acts.push({ label: "Review resources", onAct: () => { void call("prepare", { ...plan }).then(apply); } });
        else if (phase === "dns")
            acts.push({ label: "Check domain records", onAct: () => { void call("verify").then(apply); } });
        else if (phase === "sender" && username.trim() && displayName.trim())
            acts.push({ label: "Save sender", onAct: () => { void call("sender", { username, displayName, replyTo }).then(apply); } });
    }
    const order = ["signin", "resources", "dns", "sender"];
    return <Wizard icon={<Mail size={20}/>} title={`Email · ${name}`} label="Connect email" open={phase} onOpen={() => { }} back={{ label: backLabel, onSelect: () => edit(false) }} status={{ word: busy ? "Working" : phase === "signin" && session ? "Waiting for Microsoft" : phase === "dns" ? "Waiting for DNS" : "Your turn", tone: busy ? "busy" : "paused" }} acts={acts} notices={error ? <Notice tone="error" sentence={error}/> : undefined} steps={[
            { id: "signin", name: "Microsoft", body: <div className="os-app-stack">{state?.applicationReady === false ? <Notice sentence="Microsoft sign-in needs a one-time MemQL app registration." detail="Your cluster administrator needs to finish that registration before you can connect an organization."/> : null}<p className="os-caption">Use an Azure account that can manage this organization's email resources.</p>{grant?.verificationUri && grant.userCode ? <><p className="os-mono">{grant.userCode}</p><a href={grant.verificationUri} target="_blank" rel="noopener noreferrer">Enter this code at Microsoft</a></> : null}</div> },
            { id: "resources", name: "Domain and resources", body: <div className="os-app-stack">
        {state?.status === "planned" || state?.status === "provisioning" ? <><p>{plan.domain}</p><p className="os-caption">Create or resume {plan.emailService} and {plan.communicationService} in {plan.resourceGroup}. Data location: {plan.dataLocation}.</p><p className="os-caption">Azure email usage is billed to the selected subscription. This does not send mail or change your subscription.</p></> : <>
          {directories.length > 1 ? <Select id={`${formID}-azure-directory`} label="Microsoft directory" value={directory} onChange={value => { if (inFlight.current)
                        return; setDirectory(value); setSignedIn(false); void begin(value); }}><option value="">Current directory</option>{directories.map(tenant => <option key={tenant.id} value={tenant.id}>{tenant.name || tenant.id}</option>)}</Select> : null}
          <Select id={`${formID}-azure-subscription`} label="Subscription" value={plan.subscriptionId} onChange={value => { if (inFlight.current)
                        return; setPlan(old => ({ ...old, subscriptionId: value, resourceGroup: "" })); setGroups([]); void call("resourceGroups", { subscriptionId: value }).then(async (result) => { setGroups(result?.choices || []); const regions = await call("locations", { subscriptionId: value }); setLocations(regions?.choices || []); }); }}><option value="">Choose a subscription</option>{subscriptions.map(choice => <option key={choice.id} value={choice.id}>{choice.name}</option>)}</Select>
          <Select id={`${formID}-azure-group`} label="Resource group" value={plan.createResourceGroup ? "__new__" : plan.resourceGroup} onChange={value => setPlan(old => ({ ...old, resourceGroup: value === "__new__" ? "" : value, createResourceGroup: value === "__new__", resourceGroupLocation: "" }))}><option value="">Choose a resource group</option><option value="__new__">Create a resource group</option>{groups.map(choice => <option key={choice.id} value={choice.name}>{choice.name}</option>)}</Select>
          {plan.createResourceGroup ? <><Field label="Resource group name"><Input disabled={busy} id={`${formID}-azure-new-group`} label="Resource group name" value={plan.resourceGroup} onChange={value => setPlan(old => ({ ...old, resourceGroup: value }))}/></Field><Select id={`${formID}-azure-group-region`} label="Resource group region" value={plan.resourceGroupLocation || ""} onChange={value => setPlan(old => ({ ...old, resourceGroupLocation: value }))}><option value="">Choose a region</option>{locations.map(region => <option key={region.id} value={region.id}>{region.name}</option>)}</Select></> : null}
          <Field label="Email domain"><Input disabled={busy} id={`${formID}-azure-domain`} label="Email domain" placeholder="client.com" value={plan.domain} onChange={value => { const base = value.toLowerCase().replace(/[^a-z0-9-]/g, "-").slice(0, 45); setPlan(old => ({ ...old, domain: value, emailService: `${base}-email`, communicationService: `${base}-delivery` })); }}/></Field>
          <Select id={`${formID}-azure-location`} label="Data location" value={plan.dataLocation} onChange={value => setPlan(old => ({ ...old, dataLocation: value }))}>{LOCATIONS.map(location => <option key={location}>{location}</option>)}</Select>
          <details><summary>Resource names</summary><Field label="Email service"><Input disabled={busy} id={`${formID}-azure-email-name`} label="Email service" value={plan.emailService} onChange={value => setPlan(old => ({ ...old, emailService: value }))}/></Field><Field label="Delivery service"><Input disabled={busy} id={`${formID}-azure-delivery-name`} label="Delivery service" value={plan.communicationService} onChange={value => setPlan(old => ({ ...old, communicationService: value }))}/></Field></details>
        </>}
      </div> },
            { id: "dns", name: "Verify domain", body: <div className="os-app-stack"><p className="os-caption">Add these records where {plan.domain} manages DNS. Merge SPF into its existing SPF record; do not create a second one. Keep existing mail and website records.</p><table><thead><tr><th>Type</th><th>Name</th><th>Value</th><th>Status</th></tr></thead><tbody>{records.map(record => <tr key={record.purpose}><td>{record.type}</td><td><code>{record.name}</code></td><td><code>{record.value}</code></td><td>{record.status}</td></tr>)}</tbody></table></div> },
            { id: "sender", name: "Sender", body: <div className="os-app-stack"><Field label="Sender address"><Input disabled={busy} id={`${formID}-azure-sender`} label="Sender address" value={username} onChange={setUsername}/><span className="os-caption">@{plan.domain}</span></Field><Field label="Display name"><Input disabled={busy} id={`${formID}-azure-display-name`} label="Display name" value={displayName} onChange={setDisplayName}/></Field><Field label="Reply mailbox"><Input disabled={busy} id={`${formID}-azure-reply`} label="Reply mailbox" placeholder={`help@${plan.domain}`} value={replyTo} onChange={setReplyTo}/></Field><p className="os-caption">Azure sends email; it does not create an inbox. Replies need a mailbox that already receives mail.</p></div> },
        ].map(step => ({ ...step, state: step.id === phase ? "open" as const : order.indexOf(step.id) < order.indexOf(phase) ? "done" as const : "ahead" as const }))}/>;
}

export function sendOperationLabel(status: string): string {
  return ({ submitting: "Checking acceptance", unknown: "Checking acceptance", accepted: "Accepted by Azure", running: "Processing", succeeded: "Processing completed", failed: "Processing failed", canceled: "Processing canceled", rejected: "Request refused", throttled: "Waiting for Azure", unconfirmed: "Unconfirmed" } as Record<string, string>)[status] || "Unconfirmed";
}

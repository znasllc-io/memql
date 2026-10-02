import { useEffect, useId, useState } from "react";
import { Mail } from "lucide-react";
import { useSession } from "../../chrome/access";
import { Fact, Facts, Field, Head, Input, Notice, Panel, Select, Subhead } from "../../kit";
import { Wizard } from "../../kit/Wizard";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { AZURE_EMAIL_LOCATIONS, useAzureEmailCall, type AzureEmailReply, type AzureEmailScope } from "./azureEmailSetup";

/** One configuration for the cluster, shared by every organization's domains. */
export function ClusterAzureEmail({ state, onSaved, onBack }: {
  state: AzureEmailReply; onSaved: (state: AzureEmailReply) => void; onBack: () => void;
}) {
  const formID = useId();
  const { access, config } = useSession();
  const storageKey = `memql:azure-cluster:${config.domain}:${access?.userId}`;
  const { call, busy, error } = useAzureEmailCall("self");
  const [session, setSession] = useState(() => { try { return sessionStorage.getItem(storageKey) || ""; } catch { return ""; } });
  const [grant, setGrant] = useState<AzureEmailReply | null>(null);
  const [signedIn, setSignedIn] = useState(false);
  const [directories, setDirectories] = useState<NonNullable<AzureEmailReply["choices"]>>([]);
  const [subscriptions, setSubscriptions] = useState<NonNullable<AzureEmailReply["choices"]>>([]);
  const [groups, setGroups] = useState<NonNullable<AzureEmailReply["choices"]>>([]);
  const [locations, setLocations] = useState<NonNullable<AzureEmailReply["choices"]>>([]);
  const [scope, setScope] = useState<AzureEmailScope>({ subscriptionId: state.subscriptionId || "", resourceGroup: state.resourceGroup || "", createResourceGroup: state.createResourceGroup, resourceGroupLocation: state.resourceGroupLocation, dataLocation: state.dataLocation || "United States" });
  const [disconnect, setDisconnect] = useState(false);
  const [renewing, setRenewing] = useState(false);
  const keepSession = (value: string) => { setSession(value); try { if (value) sessionStorage.setItem(storageKey, value); else sessionStorage.removeItem(storageKey); } catch { /* the server owns the session */ } };
  const readGroups = async (subscriptionId: string, id = session) => {
    const result = await call("resourceGroups", { subscriptionId }, id);
    if (result) setGroups(result.choices || []);
    const regions = await call("locations", { subscriptionId }, id);
    if (regions) setLocations(regions.choices || []);
  };
  const loadChoices = async () => {
    const result = await call("subscriptions", {}, session);
    if (!result || result.status !== "connected") return;
    setSubscriptions(result.choices || []);
    const tenants = await call("directories", {}, session);
    if (!tenants) return;
    setDirectories(tenants.choices || []);
    if (scope.subscriptionId) await readGroups(scope.subscriptionId);
    setSignedIn(true);
  };
  useEffect(() => {
    if (!session || signedIn || error || state.status === "connected") return;
    const timer = window.setTimeout(() => { void call("poll", {}, session).then(async result => {
      if (result?.status === "connected") { setGrant(null); await loadChoices(); }
      else if (result?.status === "saved") {
        keepSession(""); setGrant(null);
        const saved = await call("clusterStatus");
        if (saved) onSaved(saved);
      }
      else if (result?.status === "expired") { keepSession(""); setGrant(null); }
      else if (result) setGrant(result);
    }); }, (grant?.interval || 5) * 1000);
    return () => window.clearTimeout(timer);
  }, [session, signedIn, error, grant, call, state.status]);
  const begin = async (tenantId = "") => {
    const result = await call("begin", tenantId ? { tenantId } : {});
    if (result?.sessionId) { keepSession(result.sessionId); setGrant(result); setSignedIn(false); }
  };
  if (state.status === "connected" || state.status === "reauthorize" && !renewing) return <section className="os-action-pane">
    <Head title="Cluster email" back={{ label: "Email settings", onSelect: onBack }}/>
    <div className="os-action-body os-app-stack"><Panel label="Azure configuration"><Subhead>Microsoft Azure</Subhead><Facts>
      <Fact label="Resource group" value={state.resourceGroup || ""}/><Fact label="Data location" value={state.dataLocation || ""}/>
    </Facts><p className="os-caption">All organizations use this Azure configuration. Each keeps its own verified domain and senders.</p></Panel>
    {state.status === "reauthorize" ? <Notice sentence="Microsoft requires sign-in again before changing client domains." detail="Existing verified senders remain available."/> : null}
    {state.capture ? <Notice sentence="Test email is captured in the Email app." detail="Connecting Azure does not turn on external delivery."/> : null}
    {disconnect ? <Notice tone="warn" sentence="Disconnect Azure for this cluster?" detail="Azure sends and domain setup will stop for every organization. Resources and DNS records remain."/> : null}
    {error ? <Notice tone="error" sentence={error}/> : null}</div>
    <ActionBar state={state.status === "reauthorize" ? "Sign-in required" : "Connected"} tone={state.status === "reauthorize" ? "paused" : "live"} acts={disconnect ? [
      { label: "Cancel", text: true, busy, onAct: () => setDisconnect(false) },
      { label: "Disconnect Azure", tone: "danger", busy, onAct: () => { void call("disconnectCluster", { confirmed: true }).then(result => { if (result) { keepSession(""); onSaved({ ...result, capture: state.capture }); } }); } },
    ] : [{ label: "Disconnect", busy, onAct: () => setDisconnect(true) }, ...(state.status === "reauthorize" ? [{ label: "Sign in again", busy, onAct: () => setRenewing(true) }] : [])]}/>
  </section>;
  const phase = signedIn ? "resources" : "signin";
  const acts: Act[] = [{ label: "Leave", text: true, onAct: onBack }];
  if (!busy && !signedIn && state.applicationReady !== false && (!session || error)) acts.push({ label: session ? "Sign in again" : "Sign in with Microsoft", onAct: () => { void begin(); } });
  if (!busy && signedIn && scope.subscriptionId && scope.resourceGroup && (!scope.createResourceGroup || scope.resourceGroupLocation)) acts.push({ label: "Save configuration", onAct: () => {
    void call("saveCluster", { ...scope }, session).then(result => { if (result) { keepSession(""); onSaved({ ...result, capture: state.capture }); } });
  } });
  return <Wizard icon={<Mail size={20}/>} title="Set up cluster email" label="Cluster email" open={phase} onOpen={() => {}}
    back={{ label: "Email settings", onSelect: onBack }} status={{ word: busy ? "Working" : session && !signedIn ? "Waiting for Microsoft" : "Your turn", tone: busy ? "busy" : "paused" }} acts={acts}
    notices={error ? <Notice tone="error" sentence={error}/> : undefined} steps={[
      { id: "signin", name: "Microsoft", state: signedIn ? "done" : "open", body: <div className="os-app-stack">
        {state.applicationReady === false ? <Notice sentence="Microsoft sign-in needs a one-time MemQL app registration."/> : null}
        <p className="os-caption">Connect your Azure account once for this cluster. Client domains use this saved configuration.</p>
        {grant?.userCode && grant.verificationUri ? <><p className="os-mono">{grant.userCode}</p><a href={grant.verificationUri} target="_blank" rel="noopener noreferrer">Enter this code at Microsoft</a></> : null}
      </div> },
      { id: "resources", name: "Azure resources", state: signedIn ? "open" : "ahead", body: <div className="os-app-stack">
        {directories.length > 1 ? <Select id={`${formID}-directory`} label="Microsoft directory" value="" onChange={value => { if (!busy) { setScope(old => ({ ...old, subscriptionId: "", resourceGroup: "" })); void begin(value); } }}><option value="">Current directory</option>{directories.map(row => <option key={row.id} value={row.id}>{row.name || row.id}</option>)}</Select> : null}
        <Select id={`${formID}-subscription`} label="Subscription" value={scope.subscriptionId} onChange={value => { if (busy) return; setScope(old => ({ ...old, subscriptionId: value, resourceGroup: "", createResourceGroup: false })); setGroups([]); void readGroups(value); }}><option value="">Choose a subscription</option>{subscriptions.map(row => <option key={row.id} value={row.id}>{row.name}</option>)}</Select>
        {subscriptions.length === 0 ? <Notice sentence="No enabled Azure subscriptions are available to this account."/> : null}
        <Select id={`${formID}-group`} label="Resource group" value={scope.createResourceGroup ? "__new__" : scope.resourceGroup} onChange={value => setScope(old => ({ ...old, resourceGroup: value === "__new__" ? "" : value, createResourceGroup: value === "__new__", resourceGroupLocation: "" }))}><option value="">Choose a resource group</option><option value="__new__">Create a resource group</option>{groups.map(row => <option key={row.id} value={row.name}>{row.name}</option>)}</Select>
        {scope.createResourceGroup ? <><Field label="Resource group name"><Input id={`${formID}-group-name`} label="Resource group name" value={scope.resourceGroup} onChange={value => setScope(old => ({ ...old, resourceGroup: value }))}/></Field><Select id={`${formID}-region`} label="Resource group region" value={scope.resourceGroupLocation || ""} onChange={value => setScope(old => ({ ...old, resourceGroupLocation: value }))}><option value="">Choose a region</option>{locations.map(row => <option key={row.id} value={row.id}>{row.name}</option>)}</Select></> : null}
        <Select id={`${formID}-location`} label="Email data location" value={scope.dataLocation} onChange={value => setScope(old => ({ ...old, dataLocation: value }))}>{AZURE_EMAIL_LOCATIONS.map(location => <option key={location}>{location}</option>)}</Select>
        <p className="os-caption">Saving authorizes this cluster to set up client email in these resources. Azure services are created only after you review a domain's setup.</p>
      </div> },
    ]}/>;
}

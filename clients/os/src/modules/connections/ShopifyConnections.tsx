import { useMemo, useState, type ReactNode } from "react";
import { Store } from "lucide-react";
import { renderMemQLValue, rowString } from "@znasllc-io/memql-sdk-core/client";
import { Button, EmptyState, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { Wizard } from "../../kit/Wizard";
import { useReading } from "../../cluster/reading";
import { useOsConnection } from "../../live/connection";
import { useExternalConnections } from "./useExternalConnections";
import { useWrite } from "../../apps/deployables/packages/actions";
import { ProblemNotice } from "../../apps/deployables/packages/ReportView";
import { shopifyMessage, shopifyReply } from "./shopifyReply";
import { rememberShopifyDestination } from "./shopifyInstallation";

export function ShopifyConnections({ header, appId, result }: { header: ReactNode; appId: "settings" | "deployables"; result?: string }) {
  const feed = useExternalConnections();
  const [selectedId, select] = useState("");
  const [adding, setAdding] = useState(false);
  const [armed, setArmed] = useState(false);
  const [removed, setRemoved] = useState<string[]>([]);
  const write = useWrite();
  const stores = feed.rows.filter(row => row.provider === "shopify" && !removed.includes(row.id));
  const selected = stores.find(row => row.id === selectedId);
  const back = () => { select(""); setArmed(false); write.clear(); };
  if (adding) return <AddShopifyConnection appId={appId} onBack={() => setAdding(false)} />;
  if (selected) return <section className="os-action-pane">
    <div className="os-action-body os-app-stack">
      <Head title={selected.label} breadcrumbs={[{ label: "Settings", onSelect: back }, { label: selected.label }]} back={{ label: "Settings", onSelect: back }} />
      <Panel label="Connection"><Subhead>Shopify</Subhead><Facts><Fact label="Store" value={selected.label} /><Fact label="Connection" value="Connected" /></Facts></Panel>
      {armed ? <Notice tone="warn" sentence="Remove this store from your connections?" detail="Deployed storefronts keep their store and remain online." /> : null}
      {write.refusal ? <ProblemNotice problem={write.refusal} tone="error" /> : null}
    </div>
    <ActionBar state={write.busy ? "Disconnecting" : armed ? "Confirm disconnect" : "Connected"} tone={write.busy ? "busy" : armed ? "paused" : "live"} acts={[
      { label: armed ? "Cancel" : "Back", text: true, busy: write.busy, onAct: armed ? () => setArmed(false) : back },
      { label: "Disconnect", tone: armed ? "danger" : "quiet", busy: write.busy, onAct: () => {
        if (!armed) { setArmed(true); return; }
        void write.run(query => query.executeNamed("disconnectExternalConnection", `mutation disconnectExternalConnection(connectionId: ${renderMemQLValue(selected.id)})`)).then(done => {
          if (done) { setRemoved(ids => [...ids, selected.id]); feed.reseed(); back(); }
        });
      } },
    ]} />
  </section>;
  const loading = feed.snapshot.state === "seeding" || feed.snapshot.state === "disconnected" && !feed.snapshot.error;
  return <section className="os-settings os-settings-wide">
    {header}
    <section className="os-field-group">
      <div className="os-head"><Subhead>Shopify Stores</Subhead><div className="os-head-actions"><AddButton label="Add Shopify store" onClick={() => setAdding(true)} /></div></div>
      {result && !["connected", "reconnected"].includes(result) ? <Notice tone="warn" sentence={shopifyMessage(result)} /> : null}
      {loading && stores.length === 0 ? <RecordListSkeleton label="Reading Shopify stores" /> : <RecordList as="ul" label="Shopify stores">{stores.map(store => <RecordRow key={store.id} icon={<Store size={18} aria-hidden />} name={store.label} state="Connected" tone="accent" onOpen={() => select(store.id)} label={`Manage Shopify store ${store.label}`} />)}</RecordList>}
      {feed.snapshot.state === "live" && !stores.length ? <EmptyState icon={Store} title="No Shopify stores connected">Use the plus button to connect a store.</EmptyState> : null}
      {feed.snapshot.error ? <Notice sentence="Shopify connections could not be read."><Button onClick={feed.reseed}>Try again</Button></Notice> : null}
    </section>
  </section>;
}

function AddShopifyConnection({ appId, onBack }: { appId: "settings" | "deployables"; onBack: () => void }) {
  const connection = useOsConnection();
  const read = useMemo(() => connection ? async (signal: AbortSignal) => {
    const row = shopifyReply(await connection.query.executeNamed("shopifyConnectionProviderStatus", "builtin shopifyConnectionProviderStatus()", { signal }));
    return { configured: row.configured === true, installUrl: rowString(row, "installUrl") };
  } : null, [connection]);
  const readiness = useReading("shopify-provider", read);
  const pending = readiness.state === "reading" || readiness.state === "unread";
  const acts: Act[] = [{ label: "Cancel", text: true, onAct: onBack }];
  if (readiness.value?.configured && readiness.value.installUrl) acts.push({ label: "Continue to Shopify", onAct: () => {
    rememberShopifyDestination(appId);
    window.location.assign(readiness.value!.installUrl);
  } });
  return <Wizard icon={<Store size={20} />} title="Add Shopify store" label="Connect Shopify" open="authorize" onOpen={() => {}}
    back={{ label: "Settings", onSelect: onBack }} breadcrumbs={[{ label: "Settings", onSelect: onBack }, { label: "Add Shopify store" }]}
    status={{ word: pending ? "Checking setup" : readiness.value?.configured ? "Your turn" : "Setup needed", tone: pending ? "busy" : "paused" }} acts={acts}
    notices={<>{readiness.error ? <Notice sentence="Shopify setup could not be read."><Button onClick={readiness.reread}>Try again</Button></Notice> : null}{readiness.value?.configured === false ? <Notice tone="warn" sentence="Shopify connections are not available yet." next="Your cluster administrator needs to finish Shopify app registration." /> : null}</>}
    steps={[
      { id: "authorize", name: "Shopify", state: "open", body: pending ? <RecordListSkeleton label="Checking Shopify setup" /> : <p className="os-caption">Choose a store and approve access on Shopify. You’ll return here to finish.</p> },
    ]} />;
}

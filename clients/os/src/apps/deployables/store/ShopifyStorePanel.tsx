import { useEffect, useState } from "react";
import { Store } from "lucide-react";
import { AttentionDestination } from "../../../attention/Attention";
import { useOs } from "../../../chrome/state";
import { Button, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Wizard } from "../../../kit/Wizard";
import { useExternalConnections } from "../../../modules/connections/useExternalConnections";
import { boundStoreId } from "../rows";
import { usePaneActive } from "../paneActivity";
import type { StorePanelProps } from "./StorePanel";
import { useStore, useStoreList, useStoreWrites } from "./useStore";

export function ShopifyStorePanel(props: StorePanelProps & { result?: string; revision?: number; onWritten: () => void }) {
  const active = usePaneActive();
  return <AttentionDestination appId="deployables" sectionId="deployables" target="shopify-store" visible={active}><ShopifyStoreContent {...props} /></AttentionDestination>;
}
function ShopifyStoreContent({ site, canBind, trail, back, onWritten }: StorePanelProps & { onWritten: () => void }) {
  const { actions } = useOs();
  const connections = useExternalConnections();
  const catalog = useStoreList();
  const bound = useStore(boundStoreId(site));
  const write = useStoreWrites(onWritten);
  const [picking, setPicking] = useState(!boundStoreId(site));
  const [chosen, choose] = useState("");
  const [step, setStep] = useState("store");
  const stores = connections.rows.filter(row => row.provider === "shopify");
  const selected = stores.find(row => row.id === chosen);
  const selectedStore = catalog.stores.find(row => row.id === selected?.resourceId);
  const mode = (store: typeof selectedStore) => store?.isDevelopment ? "Sandbox" : store?.plan ? "Production store" : "Store type unavailable";
  const openSettings = () => actions.openApp("deployables", "settings", { provider: "shopify" });
  useEffect(() => {
    if (!boundStoreId(site) && connections.snapshot.state === "live" && !connections.snapshot.error && stores.length === 0) openSettings();
  }, [site.id, connections.snapshot.state, connections.snapshot.version]);
  const loading = connections.snapshot.state === "seeding" || connections.snapshot.state === "disconnected" || catalog.state === "reading" || catalog.state === "unread";
  const acts: Act[] = [{ label: "Back", text: true, busy: Boolean(write.busy), onAct: picking && boundStoreId(site) ? () => setPicking(false) : back.onSelect }];
  if (canBind && !write.busy) {
    if (!picking) acts.push({ label: "Change store", onAct: () => { setPicking(true); choose(""); setStep("store"); } });
    else if (selected && selectedStore && step === "store") acts.push({ label: "Continue", onAct: () => setStep("review") });
    else if (selected && selectedStore && step === "review") acts.push({ label: "Connect store", onAct: () => void write.bindSite(site.id, selected.resourceId).then(done => { if (done) { onWritten(); bound.reread(); setPicking(false); } }) });
  }
  const notices = <>{write.error ? <Notice tone="error" sentence={write.error} /> : null}{catalog.error ? <Notice sentence="Store details could not be read."><Button onClick={catalog.reread}>Try again</Button></Notice> : null}{connections.snapshot.error ? <Notice sentence="Your Shopify stores could not be read."><Button onClick={connections.reseed}>Try again</Button></Notice> : null}</>;
  if (picking) return <Wizard icon={<Store size={20} />} title="Connect store" label="Connect storefront" breadcrumbs={trail} back={back} open={step} onOpen={setStep}
    status={{ word: write.busy ? "Connecting" : loading ? "Loading stores" : "Choose a store", tone: write.busy || loading ? "busy" : "paused" }} acts={acts} notices={notices}
    steps={[
      { id: "store", name: "Store", state: step === "store" ? "open" : "done", answer: selected?.label, body: <>
        {loading ? <RecordListSkeleton label="Reading Shopify stores" /> : <RecordList as="ul" label="Connected Shopify stores">{stores.map(store => <RecordRow key={store.id} icon={<Store size={18} aria-hidden />} name={store.label} secondary={mode(catalog.stores.find(row => row.id === store.resourceId))} state={chosen === store.id ? "Selected" : "Connected"} tone="accent" onOpen={() => choose(store.id)} label={`Select Shopify store ${store.label}`} />)}</RecordList>}
        <button type="button" className="os-link" onClick={openSettings}>Manage Shopify connections in Settings</button>
      </> },
      { id: "review", name: "Review", state: step === "review" ? "open" : "ahead", body: <Panel label="Connection"><Facts><Fact label="Store" value={selected?.label ?? ""} /><Fact label="Type" value={mode(selectedStore)} /><Fact label="Website" value={site.hostname} />{bound.store ? <Fact label="Replaces" value={bound.store.domain} /> : null}</Facts><p className="os-caption">{selectedStore?.isDevelopment ? "Uses the sandbox catalog and test checkout." : "Uses this store’s catalog and checkout. Verify payments and products in Shopify before accepting orders."}</p></Panel> },
    ]} />;
  return <section className="os-action-pane"><div className="os-action-body os-app-stack"><Head title="Store" breadcrumbs={trail} back={back} />{notices}
    {bound.state === "reading" || bound.state === "unread" ? <RecordListSkeleton label="Reading store" /> : bound.store ? <Panel label="Store"><Subhead>Shopify</Subhead><Facts><Fact label="Store" value={bound.store.domain} /><Fact label="Type" value={mode(bound.store)} /><Fact label="Catalog" value={bound.store.storefrontTokenRef ? "Available" : "Needs attention"} /><Fact label="Connection" value={bound.store.adminTokenRef ? "Connected" : "Needs attention"} /></Facts></Panel> : <Notice sentence="The connected store could not be read." />}
    <button type="button" className="os-link" onClick={openSettings}>Manage Shopify connections in Settings</button>
  </div><ActionBar state={write.busy ? "Connecting" : bound.store?.isDevelopment ? "Sandbox" : "Connected"} tone={write.busy ? "busy" : bound.store?.isDevelopment ? "paused" : "live"} acts={acts} /></section>;
}

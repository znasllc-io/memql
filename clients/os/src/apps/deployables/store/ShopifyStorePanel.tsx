import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { useState } from "react";
import { Store } from "lucide-react";
import { AttentionDestination } from "../../../attention/Attention";
import { useOs } from "../../../chrome/state";
import { Button, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Wizard } from "../../../kit/Wizard";
import { useExternalConnections } from "../../../modules/connections/useExternalConnections";
import { boundStoreId, previewStoreId } from "../rows";
import { usePaneActive } from "../paneActivity";
import { type PartsHeld } from "../parts";
import type { DeploymentRow } from "../packages/rows";
import { RefusalNotice } from "../preview/PreviewSection";
import { usePreviewReadiness, usePreviewWrites } from "../preview/usePreview";
import type { StorePanelProps } from "./StorePanel";
import { useStore, useStoreList, useStoreWrites } from "./useStore";

type Destination = "testing" | "production";
type Props = StorePanelProps & { result?: string; revision?: number; onWritten: () => void; runs?: readonly DeploymentRow[]; can?: PartsHeld };

export function ShopifyStorePanel(props: Props) {
  const active = usePaneActive();
  return <AttentionDestination appId="deployables" sectionId="deployables" target="shopify-store" visible={active}><ShopifyStoreContent {...props} /></AttentionDestination>;
}

function ShopifyStoreContent({ site, canBind, trail, back, onWritten }: Props) {
  const { actions } = useOs();
  const connections = useExternalConnections();
  const catalog = useStoreList();
  const production = useStore(boundStoreId(site));
  const testing = useStore(previewStoreId(site));
  const readiness = usePreviewReadiness(site);
  const reread = () => { onWritten(); production.reread(); testing.reread(); readiness.reread(); };
  const write = useStoreWrites(reread);
  const previewWrite = usePreviewWrites(reread);
  const [destination, setDestination] = useState<Destination | null>(null);
  const [chosen, choose] = useState("");
  const [step, setStep] = useState("store");
  const busy = Boolean(write.busy || previewWrite.busy);
  const allStores = connections.rows.filter(row => row.provider === "shopify");
  const stores = allStores.filter(row => catalog.stores.some(store => store.id === row.resourceId));
  const selected = stores.find(row => row.id === chosen);
  const selectedStore = catalog.stores.find(row => row.id === selected?.resourceId);
  const openSettings = () => actions.openApp("deployables", "settings", { provider: "shopify" });
  const loading = connections.snapshot.state === "seeding" || connections.snapshot.state === "disconnected" || catalog.state === "reading" || catalog.state === "unread";
  const start = (value: Destination) => { setDestination(value); choose(""); setStep("store"); };
  const finish = async () => {
    if (!selected || !canBind || busy) return;
    const done = destination === "testing"
      ? await previewWrite.bindPreviewStore(site.id, selected.resourceId)
      : await write.bindSite(site.id, selected.resourceId);
    if (done) { reread(); setDestination(null); }
  };
  const acts: Act[] = [{ label: "Back", text: true, busy, onAct: destination ? () => setDestination(null) : back.onSelect }];
  if (canBind && !busy && selected && selectedStore) {
    if (step === "store") acts.push({ label: "Continue", onAct: () => setStep("review") });
    else acts.push({ label: "Connect store", onAct: () => void finish() });
  }
  const notices = <>{write.error || previewWrite.error ? <Notice tone="error" sentence={write.error || previewWrite.error} /> : null}{catalog.error ? <Notice sentence="Store details could not be read."><Button onClick={catalog.reread}>Try again</Button></Notice> : null}{connections.snapshot.error ? <Notice sentence="Your Shopify stores could not be read."><Button onClick={connections.reseed}>Try again</Button></Notice> : null}</>;
  if (destination) return <Wizard icon={<Store size={20} />} title={destination === "testing" ? "Testing store" : "Production store"} label="Connect storefront" breadcrumbs={trail} back={{ label: "Store", onSelect: () => setDestination(null) }} open={step} onOpen={setStep}
    status={{ word: busy ? "Connecting" : loading ? "" : "Choose a store", tone: busy || loading ? "busy" : "paused" }} acts={acts} notices={notices}
    steps={[
      { id: "store", name: "Store", state: step === "store" ? "open" : "done", answer: selected?.label, body: <>
        {loading ? <RecordListSkeleton label="Reading Shopify stores" /> : stores.length ? <RecordList as="ul" label="Connected Shopify stores">{stores.map(store => <RecordRow key={store.id} icon={<Store size={18} aria-hidden />} name={store.label} secondary={catalog.stores.find(row => row.id === store.resourceId)?.isDevelopment ? "Sandbox store" : "Shopify store"} state={chosen === store.id ? "Selected" : "Connected"} tone="accent" onOpen={() => choose(store.id)} label={`Select Shopify store ${store.label}`} />)}</RecordList> : <p className="os-caption">No Shopify stores connected.</p>}
        <div><button type="button" className="os-link" onClick={openSettings}>Manage Shopify connections in Settings</button></div>
      </> },
      { id: "review", name: "Review", state: step === "review" ? "open" : "ahead", body: <Panel label="Connection"><Facts><Fact label="Store" value={selected?.label ?? ""} /><Fact label="Use" value={destination === "testing" ? "Testing" : "Production"} /><Fact label="Website" value={destination === "testing" ? readiness.readiness?.testingUrl ?? "" : `https://${site.hostname}/`} /></Facts><p className="os-caption">This store supplies the catalog and checkout for {destination === "testing" ? "Testing" : "Production"}. The other website keeps its store.</p>{selectedStore?.isDevelopment ? <p className="os-caption">Sandbox store — purchases follow Shopify’s test-store rules.</p> : null}</Panel> },
    ]} />;
  const productionReady = Boolean(production.store?.adminTokenRef && production.store?.storefrontTokenRef);
  const testingReady = Boolean(testing.store?.adminTokenRef && testing.store?.storefrontTokenRef);
  const readingBindings = [production, testing].some(reading => reading.state === "reading" && !reading.store) || (Boolean(boundStoreId(site)) && production.state === "unread") || (Boolean(previewStoreId(site)) && testing.state === "unread");
  const state = readingBindings ? "" : production.store ? productionReady ? "Connected" : "Setup needed" : testing.store ? testingReady ? "Testing" : "Setup needed" : "Design preview";
  return <section className="os-action-pane"><div className="os-action-body os-app-stack"><Head title="Store" breadcrumbs={trail} back={back} />{notices}
    <Panel label="Store connections"><Subhead>Connections</Subhead>
      {readingBindings ? <RecordListSkeleton label="Reading store connections" /> : <RecordList as="ul" label="Store connections">
        <RecordRow icon={<Store size={18} aria-hidden />} name="Testing" secondary={testing.store?.domain || (previewStoreId(site) ? testing.state === "failed" || testing.state === "read" ? "Store unavailable" : <InlineSkeleton label="Loading store" /> : "")} state={testing.store ? testingReady ? "Connected" : "Needs attention" : previewStoreId(site) && testing.state !== "failed" && testing.state !== "read" ? <InlineSkeleton label="Loading store status" /> : "Design preview"} tone={testing.store ? testingReady ? "accent" : "warn" : "muted"} onOpen={canBind ? () => start("testing") : undefined} label="Configure testing store" />
        <RecordRow icon={<Store size={18} aria-hidden />} name="Production" secondary={production.store?.domain || (boundStoreId(site) ? production.state === "failed" || production.state === "read" ? "Store unavailable" : <InlineSkeleton label="Loading store" /> : "")} state={production.store ? productionReady ? "Connected" : "Needs attention" : boundStoreId(site) && production.state !== "failed" && production.state !== "read" ? <InlineSkeleton label="Loading store status" /> : "Design preview"} tone={(production.store && !productionReady) ? "warn" : production.store ? "accent" : "muted"} onOpen={canBind ? () => start("production") : undefined} label="Configure production store" />
      </RecordList>}
      <div><button type="button" className="os-link" onClick={openSettings}>Manage Shopify connections in Settings</button></div>
    </Panel>
    {readiness.readiness?.goLiveRefusal.code ? <RefusalNotice refusal={readiness.readiness.goLiveRefusal} storefront={false} onOpenStore={() => {}} compact /> : null}
    <p className="os-caption">Both websites share the same design. Connect either to any of your stores, including the same sandbox. An unconnected website shows design preview.</p>
  </div><ActionBar state={busy ? "Connecting" : state} tone={busy || readingBindings ? "busy" : productionReady ? "live" : "paused"} acts={[{ label: "Back", text: true, onAct: back.onSelect }]} /></section>;
}

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
import { StoreValues } from "./StoreValues";
import { storeConnected, storeLabel, storefrontTokenMisnamed, storefrontTokenSecretName, type StoreRow } from "./rows";
import { useStore, useStoreList, useStoreWrites } from "./useStore";

type Destination = "testing" | "production";
type Props = StorePanelProps & { result?: string; revision?: number; onWritten: () => void; runs?: readonly DeploymentRow[]; can?: PartsHeld };

export function ShopifyStorePanel(props: Props) {
  const active = usePaneActive();
  return <AttentionDestination appId="deployables" sectionId="deployables" target="shopify-store" visible={active}><ShopifyStoreContent {...props} /></AttentionDestination>;
}

function ShopifyStoreContent({ site, canBind, trail, back, onWritten, can }: Props) {
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
  // CONNECTED MEANS THE EDGE CAN SERVE IT (memql#5626): both references, and
  // the Storefront one under the store's own name -- the only one the edge
  // publishes. Both references being present was the whole test before, and a
  // store registered under any other name read Connected here while its
  // storefront could read no token at all.
  const productionReady = storeConnected(production.store);
  const testingReady = storeConnected(testing.store);
  // Each store whose token sits under another name, ONCE even if both
  // websites use it: the remedy is one reconnect, not two.
  const misnamed = [production.store, testing.store].filter(
    (store, i, all): store is StoreRow => store !== null && storefrontTokenMisnamed(store) && all.findIndex((other) => other?.id === store.id) === i,
  );
  const labelFor = (storeId: string): string => {
    const known = [production.store, testing.store, ...catalog.stores].find((store) => store !== null && store.id === storeId);
    return known ? storeLabel(known) : storeId;
  };
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
    {misnamed.map((store) => <Notice key={store.id} tone="warn" sentence={`The Storefront token for ${storeLabel(store)} is registered under a different name, so the storefront cannot read it.`} next={`Reconnect the store to seal it as ${storefrontTokenSecretName(store.id)}.`}><Button onClick={openSettings}>Reconnect in Settings</Button></Notice>)}
    {readiness.readiness?.goLiveRefusal.code ? <RefusalNotice refusal={readiness.readiness.goLiveRefusal} storefront={false} onOpenStore={() => {}} compact /> : null}
    <p className="os-caption">Both websites share the same design. Connect either to any of your stores, including the same sandbox. An unconnected website shows design preview.</p>
    {/* VALUES THAT BELONG TO ONE STORE (memql#5602), beside the stores they
        belong to. App values' own gate: these are runtime values like those,
        not a store binding, so they ask `publish` rather than `store`. */}
    <StoreValues site={site} canEdit={can?.publish ?? false} labelFor={labelFor} />
  </div><ActionBar state={busy ? "Connecting" : state} tone={busy || readingBindings ? "busy" : productionReady ? "live" : "paused"} acts={[{ label: "Back", text: true, onAct: back.onSelect }]} /></section>;
}

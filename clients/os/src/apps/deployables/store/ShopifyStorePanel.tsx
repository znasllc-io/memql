import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { useState, type ReactNode } from "react";
import { Store } from "lucide-react";
import { useOs } from "../../../chrome/state";
import { Button, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { Wizard } from "../../../kit/Wizard";
import { useExternalConnections } from "../../../modules/connections/useExternalConnections";
import { boundStoreId, previewStoreId } from "../rows";
import { type PartsHeld } from "../parts";
import type { DeploymentRow } from "../packages/rows";
import { RefusalNotice } from "../preview/PreviewSection";
import { usePreviewReadiness, usePreviewWrites } from "../preview/usePreview";
import type { StorePanelProps } from "./StorePanel";
import { StoreValues } from "./StoreValuesPanel";
import { connectionWord, storeConnection, storeLabel, type StoreConnection, type StoreRow } from "./rows";
import { useStore, useStoreList, useStoreWrites } from "./useStore";

type Destination = "testing" | "production";
type Props = StorePanelProps & { result?: string; revision?: number; onWritten: () => void; runs?: readonly DeploymentRow[]; can?: PartsHeld };

export function ShopifyStorePanel({ site, canBind, trail, back, onWritten, can }: Props) {
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
  // CONNECTED IS THE ENGINE'S ANSWER (memql#5626, #5638): the edge serves the
  // store's Storefront token -- which needs the store to name its OWN token,
  // a rule the shell keeps no copy of -- and Shopify has not uninstalled the
  // app or purged the store. Both references being present was the whole test
  // before, and a store under any other name read Connected while its
  // storefront could read no token at all.
  const facts = readiness.readiness;
  const productionState: StoreConnection | null = production.store === null ? null : storeConnection(production.store, facts === null ? null : facts.storeHasStorefrontToken);
  const testingState: StoreConnection | null = testing.store === null ? null : storeConnection(testing.store, facts === null ? null : facts.previewStoreHasStorefrontToken);
  const productionReady = productionState === "connected";
  const testingReady = testingState === "connected";
  // Each store with something to repair, ONCE even if both websites use it:
  // the remedy is one reconnect, not two.
  const repairs: [StoreRow, Repair][] = [];
  for (const [store, state] of [[production.store, productionState], [testing.store, testingState]] as const) {
    if (store === null || !isRepair(state) || repairs.some(([held]) => held.id === store.id)) continue;
    repairs.push([store, state]);
  }
  const unknownWhileReading = readiness.state !== "failed";
  const rowState = (state: StoreConnection | null): ReactNode =>
    state === "unknown" ? (unknownWhileReading ? <InlineSkeleton label="Loading store status" /> : "Status unknown") : state === null ? "" : connectionWord(state);
  const rowTone = (state: StoreConnection | null): "accent" | "warn" | "muted" =>
    state === "connected" ? "accent" : state === null || state === "unknown" ? "muted" : "warn";
  const labelFor = (storeId: string): string => {
    const known = [production.store, testing.store, ...catalog.stores].find((store) => store !== null && store.id === storeId);
    return known ? storeLabel(known) : storeId;
  };
  const readingBindings = [production, testing].some(reading => reading.state === "reading" && !reading.store) || (Boolean(boundStoreId(site)) && production.state === "unread") || (Boolean(previewStoreId(site)) && testing.state === "unread");
  const state = readingBindings ? "" : productionState !== null ? barWord(productionState, "Connected") : testingState !== null ? barWord(testingState, "Testing") : "Design preview";
  return <section className="os-action-pane"><div className="os-action-body os-app-stack"><Head title="Store" breadcrumbs={trail} back={back} />{notices}
    <Panel label="Store connections"><Subhead>Connections</Subhead>
      {readingBindings ? <RecordListSkeleton label="Reading store connections" /> : <RecordList as="ul" label="Store connections">
        <RecordRow icon={<Store size={18} aria-hidden />} name="Testing" secondary={testing.store?.domain || (previewStoreId(site) ? testing.state === "failed" || testing.state === "read" ? "Store unavailable" : <InlineSkeleton label="Loading store" /> : "")} state={testing.store ? rowState(testingState) : previewStoreId(site) && testing.state !== "failed" && testing.state !== "read" ? <InlineSkeleton label="Loading store status" /> : "Design preview"} tone={testing.store ? rowTone(testingState) : "muted"} onOpen={canBind ? () => start("testing") : undefined} label="Configure testing store" />
        <RecordRow icon={<Store size={18} aria-hidden />} name="Production" secondary={production.store?.domain || (boundStoreId(site) ? production.state === "failed" || production.state === "read" ? "Store unavailable" : <InlineSkeleton label="Loading store" /> : "")} state={production.store ? rowState(productionState) : boundStoreId(site) && production.state !== "failed" && production.state !== "read" ? <InlineSkeleton label="Loading store status" /> : "Design preview"} tone={production.store ? rowTone(productionState) : "muted"} onOpen={canBind ? () => start("production") : undefined} label="Configure production store" />
      </RecordList>}
      <div><button type="button" className="os-link" onClick={openSettings}>Manage Shopify connections in Settings</button></div>
    </Panel>
    {repairs.map(([store, repair]) => <Notice key={store.id} tone="warn" sentence={REPAIR_WORDS[repair].sentence(storeLabel(store))}>
      <p className="os-caption">{REPAIR_WORDS[repair].next}</p>
      <div><Button onClick={openSettings}>Reconnect in Settings</Button></div>
    </Notice>)}
    {readiness.state === "failed" && (production.store || testing.store) ? <Notice sentence="Whether each store's token is served could not be read."><Button onClick={readiness.reread}>Try again</Button></Notice> : null}
    {readiness.readiness?.goLiveRefusal.code ? <RefusalNotice refusal={readiness.readiness.goLiveRefusal} storefront={false} onOpenStore={() => {}} compact /> : null}
    <p className="os-caption">Both websites share the same design. Connect either to any of your stores, including the same sandbox. An unconnected website shows design preview.</p>
    {/* VALUES THAT BELONG TO ONE STORE (memql#5602), beside the stores they
        belong to. App values' own gate: these are runtime values like those,
        not a store binding, so they ask `publish` rather than `store`. */}
    <StoreValues site={site} canEdit={can?.publish ?? false} labelFor={labelFor} />
  </div><ActionBar state={busy ? "Connecting" : state} tone={busy || readingBindings || (state === "" && unknownWhileReading) ? "busy" : productionReady || (productionState === null && testingReady) ? "live" : "paused"} acts={[{ label: "Back", text: true, onAct: back.onSelect }]} /></section>;
}

/** The states a store page names a repair for, each with its own calm words. */
type Repair = Extract<StoreConnection, "uninstalled" | "redacted" | "token-misnamed">;

function isRepair(state: StoreConnection | null): state is Repair {
  return state === "uninstalled" || state === "redacted" || state === "token-misnamed";
}

/**
 * What a cut-off store says, and the one act that answers it -- reconnecting
 * the store in Settings, which Shopify's own flow verifies and which re-seals
 * the Storefront token under the store's own name (memql#5638).
 */
const REPAIR_WORDS: Readonly<Record<Repair, { sentence: (store: string) => string; next: string }>> = {
  uninstalled: { sentence: (store) => `${store} was uninstalled in Shopify.`, next: "Reconnect the store to bring its catalog back." },
  redacted: { sentence: (store) => `Shopify removed the data of ${store}.`, next: "Reconnect the store to start again." },
  "token-misnamed": {
    sentence: (store) => `The Storefront token for ${store} is registered under a different name, so the storefront cannot read it.`,
    next: "Reconnect the store to seal it under its own name.",
  },
};

/** The bar's word for the store a page leads with. "" while the engine has not answered. */
function barWord(state: StoreConnection, ready: string): string {
  switch (state) {
    case "connected":
      return ready;
    case "uninstalled":
      return "Uninstalled";
    case "redacted":
      return "Data removed";
    case "unknown":
      return "";
    default:
      return "Setup needed";
  }
}

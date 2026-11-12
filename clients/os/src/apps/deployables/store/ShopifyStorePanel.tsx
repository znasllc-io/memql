import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { useState, type ReactNode } from "react";
import { Store } from "lucide-react";
import { useOs } from "../../../chrome/state";
import { Button, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../../kit";
import { formatMoment } from "../../../kit/format";
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
import { useStore, useStoreList, useStoreVerdict, useStoreWrites, type StoreWrites } from "./useStore";
import { connectedStoreWord, type StoreVerdict, type VerdictReason } from "./verdict";

type Destination = "testing" | "production";
type Props = StorePanelProps & { result?: string; revision?: number; onWritten: () => void; runs?: readonly DeploymentRow[]; can?: PartsHeld };

export function ShopifyStorePanel({ site, canBind, trail, back, onWritten, can }: Props) {
  const { actions } = useOs();
  const connections = useExternalConnections();
  const catalog = useStoreList();
  const productionId = boundStoreId(site).trim();
  const testingId = previewStoreId(site).trim();
  const production = useStore(productionId);
  const testing = useStore(testingId);
  // ONE VERDICT PER STORE: a store bound to both destinations is read once,
  // so its reasons are drawn once.
  const productionVerdict = useStoreVerdict(productionId);
  const testingVerdict = useStoreVerdict(testingId === productionId ? "" : testingId);
  const verdictOf = (storeId: string): VerdictReading | null => storeId === "" ? null : storeId === productionId ? productionVerdict : storeId === testingId ? testingVerdict : null;
  const readiness = usePreviewReadiness(site);
  const reread = () => { onWritten(); production.reread(); testing.reread(); readiness.reread(); productionVerdict.reread(); testingVerdict.reread(); };
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
      { id: "store", name: "Store", sentence: "Which Shopify store", state: step === "store" ? "open" : "done", answer: selected?.label, body: <>
        {loading ? <RecordListSkeleton label="Reading Shopify stores" /> : stores.length ? <RecordList as="ul" label="Connected Shopify stores">{stores.map(store => <RecordRow key={store.id} icon={<Store size={18} aria-hidden />} name={store.label} secondary={catalog.stores.find(row => row.id === store.resourceId)?.isDevelopment ? "Sandbox store" : "Shopify store"} state={chosen === store.id ? "Selected" : "Connected"} tone="accent" onOpen={() => choose(store.id)} label={`Select Shopify store ${store.label}`} />)}</RecordList> : <p className="os-caption">No Shopify stores connected.</p>}
        <div><button type="button" className="os-link" onClick={openSettings}>Manage Shopify connections in Settings</button></div>
      </> },
      { id: "review", name: "Review", sentence: "Check, then connect", state: step === "review" ? "open" : "ahead", body: <Panel label="Connection"><Facts><Fact label="Store" value={selected?.label ?? ""} /><Fact label="Use" value={destination === "testing" ? "Testing" : "Production"} /><Fact label="Website" value={destination === "testing" ? readiness.readiness?.testingUrl ?? "" : `https://${site.hostname}/`} /></Facts><p className="os-caption">This store supplies the catalog and checkout for {destination === "testing" ? "Testing" : "Production"}. The other website keeps its store.</p>{selectedStore?.isDevelopment ? <p className="os-caption">Sandbox store — purchases follow Shopify’s test-store rules.</p> : null}</Panel> },
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
  // CONNECTED IS WHERE THE VERDICT SPEAKS: the connection's own words come
  // first (uninstalled, purged, a misnamed token, setup still to do), and only
  // a store the engine calls connected is then said to be recording or not.
  const rowState = (state: StoreConnection | null, verdict: VerdictReading | null): ReactNode =>
    state === "connected" ? connectedState(verdict) : state === "unknown" ? (unknownWhileReading ? <InlineSkeleton label="Loading store status" /> : "Status unknown") : state === null ? "" : connectionWord(state);
  const rowTone = (state: StoreConnection | null, verdict: VerdictReading | null): "accent" | "warn" | "muted" =>
    state === "connected" ? verdictTone(verdict) : state === null || state === "unknown" ? "muted" : "warn";
  // Each bound store once, with its connection and its verdict.
  const bound: { store: StoreRow; state: StoreConnection | null; verdict: VerdictReading | null }[] = [];
  for (const [store, state] of [[production.store, productionState], [testing.store, testingState]] as const) {
    if (store === null || bound.some(held => held.store.id === store.id)) continue;
    bound.push({ store, state, verdict: verdictOf(store.id) });
  }
  const recording = bound.filter(held => held.state === "connected");
  // A verdict asked for and not had: the read failed, or its reply named no
  // state. Either way the store is not said to be recorded, and is asked again.
  const unread = recording.filter(held => held.verdict !== null && held.verdict.verdict === null && (held.verdict.state === "failed" || held.verdict.state === "read"));
  // Two different stores: each notice says which one it is about.
  const labelOf = (store: StoreRow): string | undefined => bound.length > 1 ? storeLabel(store) : undefined;
  const lead = production.store !== null ? { store: production.store, state: productionState } : testing.store !== null ? { store: testing.store, state: testingState } : null;
  const leadReading = lead === null ? null : verdictOf(lead.store.id);
  const leadVerdict = leadReading?.verdict ?? null;
  const labelFor = (storeId: string): string => {
    const known = [production.store, testing.store, ...catalog.stores].find((store) => store !== null && store.id === storeId);
    return known ? storeLabel(known) : storeId;
  };
  const readingBindings = [production, testing].some(reading => reading.state === "reading" && !reading.store) || (Boolean(boundStoreId(site)) && production.state === "unread") || (Boolean(previewStoreId(site)) && testing.state === "unread");
  const state = readingBindings ? "" : productionState !== null ? barWord(productionState, connectedWord(productionVerdict)) : testingState !== null ? barWord(testingState, "Testing") : "Design preview";
  const leadLive = lead !== null && lead.state === "connected" && leadVerdict !== null && (leadVerdict.state === "recording" || leadVerdict.state === "starting");
  // PAUSE OR RESUME, NEVER BOTH (DESIGN.md rule 12): the act the store's state
  // allows, for somebody the engine says may act on it, and absent otherwise.
  // A store that is not connected is recorded by neither: its act is the
  // reconnect its notice offers.
  const barActs: Act[] = [{ label: "Back", text: true, onAct: back.onSelect }];
  if (lead !== null && leadVerdict !== null && leadVerdict.may.act && leadVerdict.state !== "not connected") {
    const storeId = lead.store.id;
    barActs.push(leadVerdict.state === "paused"
      ? { label: "Resume", tone: "primary", busy: write.busy === "recording", onAct: () => { if (!busy) void write.setRecording(storeId, true); } }
      : { label: "Pause recording", busy: write.busy === "recording", onAct: () => { if (!busy) void write.setRecording(storeId, false); } });
  }
  return <section className="os-action-pane"><div className="os-action-body os-app-stack"><Head title="Store" breadcrumbs={trail} back={back} />{notices}
    <Panel label="Store connections"><Subhead>Connections</Subhead>
      {readingBindings ? <RecordListSkeleton label="Reading store connections" /> : <RecordList as="ul" label="Store connections">
        <RecordRow icon={<Store size={18} aria-hidden />} name="Testing" secondary={testing.store?.domain || (previewStoreId(site) ? testing.state === "failed" || testing.state === "read" ? "Store unavailable" : <InlineSkeleton label="Loading store" /> : "")} state={testing.store ? rowState(testingState, verdictOf(testing.store.id)) : previewStoreId(site) && testing.state !== "failed" && testing.state !== "read" ? <InlineSkeleton label="Loading store status" /> : "Design preview"} tone={testing.store ? rowTone(testingState, verdictOf(testing.store.id)) : "muted"} onOpen={canBind ? () => start("testing") : undefined} label="Configure testing store" />
        <RecordRow icon={<Store size={18} aria-hidden />} name="Production" secondary={production.store?.domain || (boundStoreId(site) ? production.state === "failed" || production.state === "read" ? "Store unavailable" : <InlineSkeleton label="Loading store" /> : "")} state={production.store ? rowState(productionState, productionVerdict) : boundStoreId(site) && production.state !== "failed" && production.state !== "read" ? <InlineSkeleton label="Loading store status" /> : "Design preview"} tone={production.store ? rowTone(productionState, productionVerdict) : "muted"} onOpen={canBind ? () => start("production") : undefined} label="Configure production store" />
      </RecordList>}
      <div><button type="button" className="os-link" onClick={openSettings}>Manage Shopify connections in Settings</button></div>
    </Panel>
    {repairs.map(([store, repair]) => <Notice key={store.id} tone="warn" sentence={REPAIR_WORDS[repair].sentence(storeLabel(store))}>
      <p className="os-caption">{REPAIR_WORDS[repair].next}</p>
      <div><Button onClick={openSettings}>Reconnect in Settings</Button></div>
    </Notice>)}
    {readiness.state === "failed" && (production.store || testing.store) ? <Notice sentence="Whether each store's token is served could not be read."><Button onClick={readiness.reread}>Try again</Button></Notice> : null}
    {readiness.readiness?.goLiveRefusal.code ? <RefusalNotice refusal={readiness.readiness.goLiveRefusal} storefront={false} onOpenStore={() => {}} compact /> : null}
    {unread.map(({ store, verdict }) => <Notice key={`${store.id}:unread`} sentence="Whether the store is being recorded could not be read.">
      <StoreCaptions label={labelOf(store)} detail={verdict!.error} />
      <div><Button onClick={verdict!.reread}>Try again</Button></div>
    </Notice>)}
    {/* THE ENGINE'S REASONS, ONE NOTICE EACH, in its words and with its one
        act. A paused or disconnected store carries none. */}
    {recording.flatMap(({ store, verdict }) => verdict?.verdict?.state === "needs attention"
      ? verdict.verdict.reasons.map((reason, index) => <ReasonNotice key={`${store.id}:${reason.code}:${index}`} storeId={store.id} label={labelOf(store)} reason={reason} mayAct={verdict.verdict!.may.act} write={write} onReconnect={openSettings} />)
      : [])}
    {bound.map(({ store, verdict }) => alertFailed(verdict?.verdict ?? null) ? <Notice key={`${store.id}:alert`} tone="warn" sentence="The alert could not be sent."><StoreCaptions label={labelOf(store)} detail={verdict!.verdict!.alert.reason} /></Notice> : null)}
    {recording.map(({ store, verdict }) => verdict?.verdict ? <Recorded key={`${store.id}:recorded`} verdict={verdict.verdict} label={labelOf(store)} /> : null)}
    <p className="os-caption">Both websites share the same design. Connect either to any of your stores, including the same sandbox. An unconnected website shows design preview.</p>
    {/* VALUES THAT BELONG TO ONE STORE (memql#5602), beside the stores they
        belong to. App values' own gate: these are runtime values like those,
        not a store binding, so they ask `publish` rather than `store`. */}
    <StoreValues site={site} canEdit={can?.publish ?? false} labelFor={labelFor} />
  </div><ActionBar state={busy ? "Connecting" : state} tone={busy || readingBindings || (state === "" && (unknownWhileReading || productionState === "connected")) ? "busy" : (productionReady || (productionState === null && testingReady)) && leadLive ? "live" : "paused"} acts={barActs} /></section>;
}

type VerdictReading = ReturnType<typeof useStoreVerdict>;

/** A connected store's word (`connectedStoreWord`), a skeleton while first read. */
function connectedState(reading: VerdictReading | null): ReactNode {
  const word = connectedWord(reading);
  return word === "" ? <InlineSkeleton label="Loading store status" /> : word;
}

/** The same word as text, for the bar. "" while the verdict is first read. */
function connectedWord(reading: VerdictReading | null): string {
  return reading === null ? "Status unknown" : connectedStoreWord(reading.verdict, reading.state);
}

/** Accent only for a store the verdict says is recorded or starting. */
function verdictTone(reading: VerdictReading | null): "accent" | "warn" | "muted" {
  const state = reading?.verdict?.state;
  return state === "needs attention" ? "warn" : state === "recording" || state === "starting" ? "accent" : "muted";
}

/** Which store a notice is about (two stores only), then the engine's detail, both as captions. */
function StoreCaptions({ label, detail }: { label?: string; detail?: string }) {
  return <>{label ? <p className="os-caption">{label}</p> : null}{detail ? <p className="os-caption">{detail}</p> : null}</>;
}

/** An act's address, only when it is a web page: anything else is no link. */
function webAddress(url: string): string | null {
  try {
    const parsed = new URL(url);
    return parsed.protocol === "http:" || parsed.protocol === "https:" ? url : null;
  } catch {
    return null;
  }
}

/**
 * Whether the owner's alert failed, said only while the store is unrecorded:
 * the engine serves an alert only then, and an older one served it always.
 */
function alertFailed(verdict: StoreVerdict | null): boolean {
  return verdict !== null && verdict.alert.state === "failed" && (verdict.state === "needs attention" || verdict.state === "not connected");
}

/** One reason, in the engine's words, with its one act for somebody who may take it. */
function ReasonNotice({ storeId, label, reason, mayAct, write, onReconnect }: { storeId: string; label?: string; reason: VerdictReason; mayAct: boolean; write: StoreWrites; onReconnect: () => void }) {
  const press = (act: (id: string) => Promise<boolean>) => () => { if (write.busy === "") void act(storeId); };
  let act: ReactNode = null;
  if (mayAct && reason.actLabel !== "") {
    switch (reason.act) {
      case "approve":
      case "declare": {
        const href = webAddress(reason.actUrl);
        act = href === null ? <span>{reason.actLabel}</span> : <a className="os-link" href={href} target="_blank" rel="noreferrer">{reason.actLabel}</a>;
        break;
      }
      case "reconnect":
        act = <Button onClick={onReconnect}>{reason.actLabel}</Button>;
        break;
      case "register":
        act = <Button busy={write.busy === "register"} onClick={press(write.registerEvents)}>{reason.actLabel}</Button>;
        break;
      case "retry":
        act = <Button busy={write.busy === "retry"} onClick={press(write.retryDeliveries)}>{reason.actLabel}</Button>;
        break;
    }
  }
  return <Notice tone="warn" sentence={reason.sentence}>
    <StoreCaptions label={label} detail={reason.detail} />
    {act ? <div>{act}</div> : null}
  </Notice>;
}

/**
 * What is recorded: each kind's last change and last check, from when orders
 * are kept, and -- quietly, because neither is a fault -- the fields Shopify
 * withholds and, behind one closed line, what the connection does not reach.
 */
function Recorded({ verdict, label }: { verdict: StoreVerdict; label?: string }) {
  if (verdict.recorded.length === 0 && verdict.ordersFrom === null && verdict.withheld.length === 0 && verdict.outside.length === 0) return null;
  return <Panel label="What is recorded"><Subhead meta={label}>What is recorded</Subhead>
    {verdict.recorded.map(kind => <div key={kind.kind} role="group" aria-label={kindName(kind.kind)}>
      <Subhead>{kindName(kind.kind)}</Subhead>
      <Facts><Fact label="Last change" value={kind.lastChangeAt === null ? "" : formatMoment(kind.lastChangeAt)} /><Fact label="Last checked" value={kind.lastCheckedAt === null ? "" : formatMoment(kind.lastCheckedAt)} /></Facts>
    </div>)}
    {verdict.ordersFrom !== null ? <p className="os-caption">Orders since {formatDay(verdict.ordersFrom)}</p> : null}
    {verdict.withheld.length > 0 ? <p className="os-caption">Shopify withholds: {verdict.withheld.join(", ")}</p> : null}
    {verdict.outside.length > 0 ? <details className="os-caption">
      <summary>Not part of this connection</summary>
      <ul>{verdict.outside.map(word => <li key={word}>{word}</li>)}</ul>
    </details> : null}
  </Panel>;
}

function kindName(kind: string): string {
  return kind.charAt(0).toUpperCase() + kind.slice(1);
}

/** A YYYY-MM-DD day, read as that day wherever the browser is. */
function formatDay(day: string): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  if (match === null) return day;
  return new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3])).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
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

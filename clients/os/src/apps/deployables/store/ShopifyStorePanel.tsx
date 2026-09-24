import { AttentionDestination } from "../../../attention/Attention";
import { usePaneActive } from "../paneActivity";
import { useState } from "react";
import { Button, Caption, Fact, Facts, Field, Head, Input, Notice, Panel, RecordListSkeleton, Subhead } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { useSession } from "../../../chrome/access";
import { StorePanel as StoreDetails, type StorePanelProps } from "./StorePanel";
import { shopifyMessage, useShopifyConnect } from "./useShopifyConnect";

export function ShopifyStorePanel(props: Parameters<typeof ShopifyStoreContent>[0]) {
  const active = usePaneActive();
  return <AttentionDestination appId="deployables" sectionId="deployables" target="shopify-store" visible={active}><ShopifyStoreContent {...props} /></AttentionDestination>;
}

function ShopifyStoreContent({ site, canBind, trail, back, result, revision = 0, onWritten }: StorePanelProps & {
  result?: string; revision?: number; onWritten: () => void;
}) {
  const connect = useShopifyConnect(site.id, revision);
  const { config } = useSession();
  const [view, setView] = useState<"connection" | "app" | "token" | "details">("connection");
  const [clientId, setClientId] = useState("");
  const [secret, setSecret] = useState("");
  const [token, setToken] = useState("");
  const status = connect.value;
  const loading = connect.state === "unread" || connect.state === "reading";
  const ready = Boolean(status?.connected && status.storefrontTokenSet);
  const setup = view === "app" || Boolean(status && !status.appSaved);
  const reset = () => { if (view === "details") { connect.reread(); onWritten(); } setView("connection"); setSecret(""); setToken(""); };
  const writeDone = () => { reset(); connect.reread(); onWritten(); };
  const acts: Act[] = [{ label: "Back", text: true, onAct: view === "connection" ? back.onSelect : reset }];
  if (!loading && !connect.busy && canBind && status) {
    if (view === "token" && token.trim()) acts.push({ label: "Save token", onAct: () => void connect.setToken(token.trim()).then(ok => { setToken(""); if (ok) writeDone(); }) });
    else if (setup && clientId.trim() && secret.trim()) acts.push({ label: "Save", onAct: () => void connect.saveApp(clientId.trim(), secret.trim()).then(ok => { setSecret(""); if (ok) writeDone(); }) });
    else if (!setup && view === "connection") acts.push({ label: status.connected ? "Reconnect Shopify" : "Connect Shopify", onAct: () => void connect.connect() });
  }
  if (view === "details") return <div className="os-deploy-pane"><div className="os-deploy-scroll"><StoreDetails site={site} canBind={canBind} trail={[...trail, { label: "Details" }]} back={{ label: "Store", onSelect: reset }} /></div><ActionBar state={ready ? "Connected" : "Setup needed"} acts={[{ label: "Back", onAct: reset }]} /></div>;
  return <div className="os-deploy-pane">
    <div className="os-deploy-scroll">
      <Head title={view === "token" ? "Storefront token" : setup ? "Shopify app" : "Store"} breadcrumbs={trail} back={back} />
      {result && !["connected", "reconnected"].includes(result) ? <Notice tone="warn" sentence={shopifyMessage(result)} /> : null}
      {connect.error || connect.writeError ? <Notice tone="error" sentence={connect.error || connect.writeError} /> : null}
      {connect.state === "failed" ? <Button onClick={connect.reread}>Try again</Button> : null}
      {loading && !status ? <RecordListSkeleton label="Reading Shopify connection" rows={3} /> : status ? <>
        <Panel label="Shopify connection"><Subhead>Shopify</Subhead><Facts>
          <Fact label="Store" value={status.shopDomain} />
          <Fact label="Connection" value={status.connected ? "Connected" : "Not connected"} />
          <Fact label="Catalog" value={status.storefrontTokenSet ? "Available" : "Not connected"} />
        </Facts></Panel>
        {view === "token" ? <Panel label="Storefront API token"><Field label="Storefront API token"><Input type="password" id="shopify-storefront-token" label="Storefront API token" value={token} onChange={setToken} /></Field></Panel> : setup ? <Panel label="Shopify app credentials">
          <Subhead>App credentials</Subhead>
          <Field label="Client ID"><Input id="shopify-client-id" label="Client ID" value={clientId} onChange={setClientId} /></Field>
          <Field label="Client secret"><Input type="password" id="shopify-client-secret" label="Client secret" value={secret} onChange={setSecret} /></Field>
          <Facts><Fact label="Redirect URL" value={`https://identity.${config.domain}/auth/shopify/callback`} /></Facts>
          <details><summary>Required permissions</summary><Caption>{status.requiredScopes.join(", ")}</Caption></details>
        </Panel> : <Panel label="Connection details"><Subhead>Connection</Subhead>
          {!ready ? <Caption>The page can be live. Connect Shopify to enable its catalog and shopping features.</Caption> : null}
          {status.pendingApp ? <Caption>App changes are waiting for Shopify authorization.</Caption> : null}
          {status.connected && status.requiredScopes.some(scope => !status.grantedScopes.includes(scope)) ? <details><summary>Missing permissions</summary><Caption>{status.requiredScopes.filter(scope => !status.grantedScopes.includes(scope)).join(", ")}</Caption></details> : null}
          <div className="os-panel-actions">
            {canBind && status.connected ? <button type="button" className="os-link" onClick={() => setView("token")}>Storefront token</button> : null}
            {canBind ? <button type="button" className="os-link" onClick={() => setView("app")}>App credentials</button> : null}
          </div>
        </Panel>}
      </> : null}
      <button type="button" className="os-link" onClick={() => setView("details")}>Store details</button>
    </div>
    <ActionBar state={connect.busy ? "Connecting" : loading ? "Loading" : ready ? "Connected" : "Setup needed"} tone={connect.busy || loading ? "busy" : ready ? "live" : "paused"} acts={acts} />
  </div>;
}

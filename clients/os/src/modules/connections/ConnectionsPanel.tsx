import { useEffect, useState } from "react";
import { Head } from "../../kit";
import { LocalTabs } from "../../kit/LocalTabs";
import { useSession } from "../../chrome/access";
import type { OsAppProps } from "../../system/registry";
import { GitHubAccountsSettings } from "./GitHubAccountsSettings";
import { useSourceCredentials } from "./useSourceCredentials";
import { useSourceConnections } from "./connections";
import { credentialFromRow } from "../../apps/deployables/sources/rows";
import { githubAccountsFor } from "../../apps/deployables/sources/accountSetup";
import { returnPathFor, type ConnectReturn } from "../../apps/deployables/sources/connectReturn";
import { usePackages } from "../../apps/deployables/packages/usePackages";
import { packageFromRow } from "../../apps/deployables/packages/rows";
import { ShopifyConnections } from "./ShopifyConnections";

export type ConnectionProvider = "github" | "shopify";
const PROVIDERS = [["github", "GitHub"], ["shopify", "Shopify"]] as const;
/** Same records, controls and navigation in every app that needs connections. */
export function ConnectionsPanel({ appId, initialProvider = "github", connectResult = null, intent, consumeIntent }: {
  appId: "settings" | "deployables"; initialProvider?: ConnectionProvider; connectResult?: ConnectReturn | null;
  intent?: OsAppProps["intent"]; consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const [provider, setProvider] = useState<ConnectionProvider>(initialProvider);
  const [shopifyResult, setShopifyResult] = useState<string>();
  useEffect(() => setProvider(initialProvider), [initialProvider]);
  const [result, setResult] = useState<ConnectReturn | null>(connectResult);
  const { access } = useSession();
  const credentials = useSourceCredentials();
  const connections = useSourceConnections();
  const packages = usePackages();
  useEffect(() => setResult(connectResult), [connectResult]);
  useEffect(() => {
    if (!intent) return;
    if (intent.payload.provider === "shopify") setProvider("shopify");
    const shopify = intent.payload.shopify as { reason?: string } | undefined;
    if (shopify) { setProvider("shopify"); setShopifyResult(shopify.reason); }
    const answer = intent.payload.connect as ConnectReturn | undefined;
    if (answer && typeof answer.reason === "string") { setResult(answer); setProvider("github"); credentials.reseed(); }
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent]);
  const rows = credentials.snapshot.rows.map(credentialFromRow);
  useEffect(() => connections.observeCredentials(rows), [credentials.snapshot.version, connections.observeCredentials]);
  const header = <><Head title="Settings" /><LocalTabs label="Connections" value={provider} onChange={setProvider} options={PROVIDERS} /></>;
  const section = appId === "settings" ? "connections" : "settings";
  return provider === "github" ? <GitHubAccountsSettings
    accounts={githubAccountsFor(rows, access?.userId ?? "", connections.revokedCredentialIds)}
    packages={packages.snapshot.rows.map(packageFromRow)}
    feed={{ state: credentials.snapshot.state, error: credentials.snapshot.error, retry: credentials.reseed }}
    header={header} returnPath={returnPathFor(section, appId)} connectResult={result} />
    : <ShopifyConnections header={header} appId={appId} result={shopifyResult} />;
}

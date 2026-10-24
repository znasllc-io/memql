import { useEffect, useState, type ReactNode } from "react";
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
import { AzureEmailConnections } from "./AzureEmailConnections";
import { ShopifyConnections } from "./ShopifyConnections";

export type ConnectionProvider = "github" | "shopify" | "email";
const PROVIDERS = [["github", "GitHub"], ["shopify", "Shopify"], ["email", "Email"]] as const;
/** Same records, controls and navigation in every app that needs connections. */
export function ConnectionsPanel({ appId, initialProvider = "github", connectResult = null, intent, consumeIntent, additionalSections = [], heading = true }: {
  appId: "settings" | "deployables" | "pipelines"; heading?: boolean; initialProvider?: ConnectionProvider; connectResult?: ConnectReturn | null;
  additionalSections?: readonly { id: string; name: string; render: (active: boolean, header: ReactNode) => ReactNode }[];
  intent?: OsAppProps["intent"]; consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const [provider, setProvider] = useState<string>(initialProvider);
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
    if (intent.payload.provider === "email") setProvider("email");
    if (intent.payload.provider === "shopify") setProvider("shopify");
    const shopify = intent.payload.shopify as { reason?: string } | undefined;
    if (shopify) { setProvider("shopify"); setShopifyResult(shopify.reason); }
    const answer = intent.payload.connect as ConnectReturn | undefined;
    if (answer && typeof answer.reason === "string") { setResult(answer); setProvider("github"); credentials.reseed(); }
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent]);
  const rows = credentials.snapshot.rows.map(credentialFromRow);
  useEffect(() => connections.observeCredentials(rows), [credentials.snapshot.version, connections.observeCredentials]);
  useEffect(() => {
    if (!PROVIDERS.some(([id]) => id === provider) && !additionalSections.some(section => section.id === provider)) setProvider(initialProvider);
  }, [provider, initialProvider, additionalSections]);
  const header = <>{heading ? <Head title="Settings" /> : null}<LocalTabs label="Connections" value={provider} onChange={setProvider} options={[...(appId === "pipelines" ? PROVIDERS.filter(([id]) => id === "github") : PROVIDERS), ...additionalSections.map(section => [section.id, section.name] as const)]} /></>;
  const section = appId === "settings" ? "connections" : "settings";
  return <>{provider === "github" ? <GitHubAccountsSettings
    accounts={githubAccountsFor(rows, access?.userId ?? "", connections.revokedCredentialIds)}
    packages={packages.snapshot.rows.map(packageFromRow)}
    feed={{ state: credentials.snapshot.state, error: credentials.snapshot.error, retry: credentials.reseed }}
    header={header} returnPath={returnPathFor(section, appId)} connectResult={result} />
    : provider === "email" ? <AzureEmailConnections manageCluster={appId === "settings"} header={header} /> : provider === "shopify" ? <ShopifyConnections header={header} appId={appId === "settings" ? "settings" : "deployables"} result={shopifyResult} /> : null}
    {additionalSections.map(section => <div className="os-deploy-pane" style={provider !== section.id ? { display: "none" } : undefined} key={section.id} hidden={provider !== section.id} inert={provider !== section.id}>{section.render(provider === section.id, header)}</div>)}
  </>;
}

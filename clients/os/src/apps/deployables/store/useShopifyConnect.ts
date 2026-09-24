import { useCallback, useMemo, useRef, useState } from "react";
import { rowString, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useReading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import { flatten } from "../../../kit/rows";

export interface ShopifyStatus {
  shopDomain: string; storeId: string; appSaved: boolean; pendingApp: boolean;
  connected: boolean; storefrontTokenSet: boolean; requiredScopes: string[]; grantedScopes: string[];
}
export function shopifyReply(result: Result): Row {
  const first = result.rows()[0];
  if (!first) throw new Error("Shopify did not return a result.");
  const row = flatten(first);
  const reason = rowString(row, "reason");
  if (reason !== "ok") throw new Error(shopifyMessage(reason));
  return row;
}
export function shopifyMessage(reason: string): string {
  return ({
    connect_state_invalid: "This connection link expired. Connect Shopify again.",
    signature_invalid: "Shopify's response could not be verified. Try connecting again.",
    permission_lost: "You no longer have access to connect this store.",
    scopes_missing: "Shopify did not grant the required storefront permissions.",
    storefront_token_failed: "Shopify is connected, but catalog access needs attention. Connect again to retry.",
    exchange_failed: "Shopify could not finish connecting. Check the current connection and try again.",
    store_not_named: "This deployable's manifest must name a Shopify store.",
    store_redacted: "This store is no longer available.",
    site_not_writable: "You do not have access to change this storefront.",
    app_credentials_invalid: "Check the Shopify app's client ID and secret.",
    store_in_use: "This store is shared with deployables you cannot change.",
    storefront_token_invalid: "Shopify did not accept this Storefront API token.",
    installed: "Select Connect Shopify to finish connecting this storefront.",
  } as Record<string, string>)[reason] ?? `Shopify could not finish this step (${reason || "no result"}).`;
}
function strings(row: Row, key: string): string[] {
  let value = row[key];
  if (typeof value === "string") { try { value = JSON.parse(value); } catch { return []; } }
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}
export function shopifyStatusFrom(row: Row): ShopifyStatus {
  return { shopDomain: rowString(row, "shopDomain"), storeId: rowString(row, "storeId"),
    appSaved: row.appSaved === true, pendingApp: row.pendingApp === true, connected: row.connected === true,
    storefrontTokenSet: row.storefrontTokenSet === true, requiredScopes: strings(row, "requiredScopes"), grantedScopes: strings(row, "grantedScopes") };
}
export function useShopifyConnect(siteId: string, revision = 0) {
  const connection = useOsConnection();
  const read = useMemo(() => connection && siteId ? async (signal: AbortSignal) =>
    shopifyStatusFrom(shopifyReply(await connection.query.shopifyConnectStatus({ siteId }, { signal }))) : null, [connection, siteId]);
  const reading = useReading(`shopify-connect:${siteId}:${revision}`, read);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const pending = useRef(false);
  const run = useCallback(async (work: () => Promise<Result>) => {
    if (!connection || pending.current) return null;
    pending.current = true; setBusy(true); setError("");
    try { return shopifyReply(await work()); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); return null; }
    finally { pending.current = false; setBusy(false); reading.reread(); }
  }, [connection, reading.reread]);
  return { ...reading, busy, writeError: error,
    saveApp: async (clientId: string, clientSecret: string) => Boolean(await run(() => connection!.query.shopifyStoreAppSave({ siteId, clientId, clientSecret }))),
    setToken: async (token: string) => Boolean(await run(() => connection!.query.shopifyStorefrontTokenSet({ siteId, token }))),
    connect: async () => {
      const row = await run(() => connection!.query.shopifyConnectBegin({ siteId, returnPath: "/" }));
      if (!row) return;
      let target: URL;
      try { target = new URL(rowString(row, "authorizeUrl")); }
      catch { setError("Shopify returned an invalid connection address."); return; }
      if (target.protocol !== "https:" || !target.hostname.endsWith(".myshopify.com") || target.username || target.password) {
        setError("Shopify returned an invalid connection address."); return;
      }
      window.location.assign(target.href);
    },
  };
}

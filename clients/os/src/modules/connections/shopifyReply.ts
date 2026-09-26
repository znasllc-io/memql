import { rowString, type Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../kit/rows";

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
    shopify_provider_not_configured: "Your cluster administrator needs to finish Shopify app registration.",
    installation_failed: "Shopify could not finish authorization. Add the store again from Shopify.",
    shop_domain_invalid: "Enter a store address such as your-store.myshopify.com.",
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

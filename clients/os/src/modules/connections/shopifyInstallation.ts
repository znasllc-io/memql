import { rowString, renderMemQLValue, type QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { returnPathFor } from "../../apps/deployables/sources/connectReturn";
import { shopifyReply } from "./shopifyReply";

const LAUNCH = "memql.shopify.launch";
const DESTINATION = "memql.shopify.destination";
export type ShopifyDestination = "settings" | "deployables";
export function rememberShopifyDestination(appId: ShopifyDestination) {
  sessionStorage.setItem(DESTINATION, JSON.stringify({ appId, at: Date.now() }));
}
export function takeShopifyDestination(): ShopifyDestination {
  try {
    const raw = sessionStorage.getItem(DESTINATION);
    sessionStorage.removeItem(DESTINATION);
    const value = raw ? JSON.parse(raw) : null;
    if (value?.appId === "deployables" && Date.now() - value.at < 30 * 60_000) return "deployables";
  } catch { /* A Shopify-initiated installation opens global Connections. */ }
  return "settings";
}
// Capture before login/navigation can consume URL parameters. This is untrusted
// input: only the integration's HMAC and timestamp checks admit the selected shop.
export function captureShopifyInstallation(win: Window) {
  const params = new URLSearchParams(win.location.search);
  if (!params.has("shop") || !params.has("hmac") || !params.has("timestamp") || params.has("code") || params.has("state")) return;
  win.sessionStorage.setItem(LAUNCH, win.location.search.slice(1));
  win.history.replaceState(win.history.state, "", win.location.pathname + win.location.hash);
}
export function takeShopifyInstallation(): string | null {
  const raw = sessionStorage.getItem(LAUNCH);
  sessionStorage.removeItem(LAUNCH);
  return raw;
}
export async function authorizeShopifyInstallation(query: QueryClient, signedQuery: string, appId: ShopifyDestination): Promise<string> {
  const path = returnPathFor(appId === "settings" ? "connections" : "settings", appId);
  const row = shopifyReply(await query.executeNamed("shopifyAccountConnectBegin", `builtin shopifyAccountConnectBegin(signedQuery: ${renderMemQLValue(signedQuery)}, returnPath: ${renderMemQLValue(path)})`));
  const url = new URL(rowString(row, "authorizeUrl"));
  const shop = new URLSearchParams(signedQuery).get("shop");
  if (url.protocol !== "https:" || url.hostname !== shop || url.pathname !== "/admin/oauth/authorize" || url.username || url.password || url.port) throw new Error("Shopify returned an invalid connection address.");
  return url.href;
}

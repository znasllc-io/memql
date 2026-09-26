export interface ShopifyReturn { reason: string; siteId: string; appId?: "settings" | "deployables"; section?: string }
let parked: ShopifyReturn | null = null;
export function captureShopifyReturn(win: Window): ShopifyReturn | null {
  const params = new URLSearchParams(win.location.search);
  if (!params.has("shopify")) return null;
  const appId = params.get("connectApp") === "settings" ? "settings" : "deployables";
  const section = params.get("connect") === "settings" || appId === "settings" ? (appId === "settings" ? "connections" : "settings") : "deployables";
  parked = { reason: params.get("shopify") ?? "", siteId: params.get("site") ?? "", appId, section };
  params.delete("shopify"); params.delete("site"); params.delete("connect"); params.delete("connectApp");
  const search = params.toString();
  win.history.replaceState(win.history.state, "", `${win.location.pathname}${search ? `?${search}` : ""}${win.location.hash}`);
  return parked;
}
export function takeShopifyReturn(): ShopifyReturn | null {
  const result = parked; parked = null; return result;
}

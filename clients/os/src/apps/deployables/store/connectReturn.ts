export interface ShopifyReturn { reason: string; siteId: string }
let parked: ShopifyReturn | null = null;
export function captureShopifyReturn(win: Window): ShopifyReturn | null {
  const params = new URLSearchParams(win.location.search);
  if (!params.has("shopify")) return null;
  parked = { reason: params.get("shopify") ?? "", siteId: params.get("site") ?? "" };
  params.delete("shopify"); params.delete("site");
  const search = params.toString();
  win.history.replaceState(win.history.state, "", `${win.location.pathname}${search ? `?${search}` : ""}${win.location.hash}`);
  return parked;
}
export function takeShopifyReturn(): ShopifyReturn | null {
  const result = parked; parked = null; return result;
}

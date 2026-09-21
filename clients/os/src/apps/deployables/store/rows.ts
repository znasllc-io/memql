import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

// The v1:shopify:store row as the storefront deployable reads it.
//
// ===========================================================================
// THE ROW IS THE RECORD, AND THE SITE ONLY NAMES IT
// ===========================================================================
// `v1:platform:site.binding` used to carry {storeDomain, storefrontTokenRef}
// -- the same two values this row already held, edited in two places at two
// authorization tiers (epic memql#5530, gap G4). The binding names a store
// now, and everything about that store is read from here: the domain the
// storefront calls, the NAME of the secret holding its Storefront token, the
// scopes, the plan, the protected-data level and the status.
//
// ===========================================================================
// THREE FIELDS ENDING IN `Ref` ARE NAMES, NEVER VALUES
// ===========================================================================
// `adminTokenRef`, `storefrontTokenRef` and `webhookSecretRef` each NAME a
// v1:platform:globalSecret row. The token itself is not on this row and is
// never fetched by this app: the edge resolves the Storefront one at serve
// time into the site's runtime-config document, and that is the only place
// any of them is dereferenced. A surface that showed a value here would be a
// token on a screen.

/** A configured store, as `stores()` and `storeById()` project it. */
export interface StoreRow {
  id: string;
  domain: string;
  name: string;
  appClientId: string;
  /** The NAME of a globalSecret row. Never a token. */
  adminTokenRef: string;
  storefrontTokenRef: string;
  webhookSecretRef: string;
  apiVersion: string;
  protectedDataLevel: string;
  plan: string;
  status: string;
  /** True when this store is one a storefront is exercised against. */
  isDevelopment: boolean;
  /** The live store this one stands in for, or "" when it stands in for none. */
  developmentOfStoreId: string;
}

export function storeFromRow(row: Row): StoreRow {
  return {
    id: rowString(row, "id"),
    domain: rowString(row, "domain"),
    name: rowString(row, "name"),
    appClientId: rowString(row, "appClientId"),
    adminTokenRef: rowString(row, "adminTokenRef"),
    storefrontTokenRef: rowString(row, "storefrontTokenRef"),
    webhookSecretRef: rowString(row, "webhookSecretRef"),
    apiVersion: rowString(row, "apiVersion"),
    protectedDataLevel: rowString(row, "protectedDataLevel"),
    plan: rowString(row, "plan"),
    status: rowString(row, "status"),
    isDevelopment: row["isDevelopment"] === true,
    developmentOfStoreId: rowString(row, "developmentOfStoreId"),
  };
}

/**
 * How a store is named on screen.
 *
 * THE DOMAIN, ALWAYS, because it is the one identifier Shopify never changes
 * and the one an operator can check against the Shopify admin. A friendly
 * name is a label somebody typed and two stores may carry the same one.
 */
export function storeLabel(store: StoreRow): string {
  return store.domain === "" ? store.id : store.domain;
}

/**
 * The domain, and nothing else.
 *
 * THE NAME DOES NOT BELONG IN THE LABEL. Appending it read
 * `example-dev.myshopify.com (Example Shop (development))` -- nested
 * parentheses, from a name that already carried its own. A choice's label is
 * the identifier somebody matches against the Shopify admin; everything else
 * about it is its description, which is where `storeNote` puts it.
 */
export function storeLongLabel(store: StoreRow): string {
  return storeLabel(store);
}

/** What a store says about itself beneath its own name. */
export function storeNote(store: StoreRow, all: readonly StoreRow[]): string {
  const parts: string[] = [];
  if (store.name !== "" && store.name !== storeLabel(store)) parts.push(store.name);
  if (store.status !== "") parts.push(store.status);
  if (store.plan !== "") parts.push(store.plan);
  if (store.isDevelopment) {
    const stands = all.find((s) => s.id === store.developmentOfStoreId);
    parts.push(stands ? `development store for ${stands.domain}` : "development store");
  }
  return parts.join(" \u00b7 ");
}

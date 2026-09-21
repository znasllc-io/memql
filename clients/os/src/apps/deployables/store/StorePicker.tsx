import { useMemo, useState } from "react";

import {
  Button,
  Caption,
  Check,
  ChoiceStack,
  Field,
  Input,
  Notice,
  Panel,
  Select,
  Subhead,
  type ChoiceOption,
} from "../../../kit";
import { siteName, type SiteRow } from "../rows";
import { storeLongLabel, type StoreRow } from "./rows";
import { BLANK_STORE, useStoreList, type NewStore, type StoreWrites } from "./useStore";

// CHOOSING THE STORE A STOREFRONT FRONTS (epic memql#5530, issue memql#5539).
//
// ===========================================================================
// THE POPULATION IS THE BINDING'S POPULATION
// ===========================================================================
// The list is `stores()`, which is what THIS CALLER may read. That is not a
// convenience: the engine refuses a binding naming a store the caller cannot
// read, so a picker built from any other source would offer choices the
// server then refuses -- which reads as a broken control rather than as a
// permission.
//
// ===========================================================================
// REGISTERING A STORE IS PART OF ATTACHING ONE, NOT A SEPARATE ERRAND
// ===========================================================================
// Everything about a storefront is configured on its deployable (design D5).
// Somebody standing here with a fresh Shopify app and no store row should not
// be sent to a second surface and back; the same step registers the store and
// attaches it. It is two writes and it says so, because the first can succeed
// and the second fail.
//
// ===========================================================================
// EVERY CREDENTIAL FIELD TAKES THE NAME OF A SECRET, NEVER A TOKEN
// ===========================================================================
// `adminTokenRef`, `storefrontTokenRef` and `webhookSecretRef` each NAME a
// `v1:platform:globalSecret` row. A field here that took a token would put
// the token in a call string, which is rendered into logs on a parse error --
// and would show it on a screen. The placeholders and the caption say so
// where somebody is about to type, not in a refusal afterwards.

const NAMES_NOT_TOKENS =
  "Each of the three is the NAME of a cluster secret, not the token itself. Create the secret first, then name it here.";

export function StorePicker({
  site,
  currentStoreId,
  writes,
  onDone,
  onCancel,
}: {
  site: SiteRow;
  /** The store this storefront is bound to now, or "". */
  currentStoreId: string;
  writes: StoreWrites;
  onDone: () => void;
  /** Absent when there is nothing to go back to -- an unbound storefront. */
  onCancel?: () => void;
}) {
  const list = useStoreList();
  const [chosen, setChosen] = useState(currentStoreId);
  const [registering, setRegistering] = useState(false);
  const [draft, setDraft] = useState<NewStore>(BLANK_STORE);

  const name = siteName(site);
  const options = useMemo<ChoiceOption[]>(
    () =>
      list.stores.map((store) => ({
        value: store.id,
        label: storeLongLabel(store),
        description: describeStore(store, list.stores),
      })),
    [list.stores],
  );

  const domain = draft.domain.trim();
  const canRegister = domain !== "" && draft.storefrontTokenRef.trim() !== "";
  const canAttach = chosen !== "" && chosen !== currentStoreId;

  async function register() {
    // THE ID IS DERIVED FROM THE DOMAIN, which is the one identifier Shopify
    // never changes. A random id would make the same store attachable twice
    // under two rows, and every mirrored row is scoped by store id.
    const storeId = slugOf(domain);
    const ok = await writes.createStore({ ...draft, storeId, domain });
    if (!ok) return;
    const bound = await writes.bindSite(site.id, storeId);
    if (bound) onDone();
  }

  async function attach() {
    const ok = await writes.bindSite(site.id, chosen);
    if (ok) onDone();
  }

  return (
    <>
      {writes.error === "" ? null : (
        <Notice
          tone="error"
          sentence="That did not run."
          next="Nothing was attached. What you typed is still here."
          detail={writes.error}
        />
      )}

      <Panel label={`Choose the store ${name} fronts`}>
        <Subhead>{registering ? "Register a store" : "Choose a store"}</Subhead>

        {registering ? (
          <>
            <Caption>{NAMES_NOT_TOKENS}</Caption>
            <Field label="Store domain">
              <Input
                id="os-store-domain"
                label="The store's myshopify.com domain"
                value={draft.domain}
                onChange={(domain) => setDraft((d) => ({ ...d, domain }))}
                placeholder="acme-widgets.myshopify.com"
                code
              />
            </Field>
            <Field label="Name">
              <Input
                id="os-store-name"
                label="A name for this store"
                value={draft.name}
                onChange={(name) => setDraft((d) => ({ ...d, name }))}
                placeholder="Acme Widgets"
              />
            </Field>
            <Field label="Storefront token">
              <Input
                id="os-store-storefront-ref"
                label="The name of the secret holding the Storefront API token"
                value={draft.storefrontTokenRef}
                onChange={(storefrontTokenRef) => setDraft((d) => ({ ...d, storefrontTokenRef }))}
                placeholder="ACME_STOREFRONT_TOKEN"
                code
              />
            </Field>
            <Field label="Admin token">
              <Input
                id="os-store-admin-ref"
                label="The name of the secret holding the Admin API token"
                value={draft.adminTokenRef}
                onChange={(adminTokenRef) => setDraft((d) => ({ ...d, adminTokenRef }))}
                placeholder="ACME_ADMIN_TOKEN"
                code
              />
            </Field>
            <Field label="Webhook secret">
              <Input
                id="os-store-webhook-ref"
                label="The name of the secret holding the webhook signing secret"
                value={draft.webhookSecretRef}
                onChange={(webhookSecretRef) => setDraft((d) => ({ ...d, webhookSecretRef }))}
                placeholder="ACME_WEBHOOK_SECRET"
                code
              />
            </Field>
            <Field label="API version">
              <Input
                id="os-store-api-version"
                label="The Admin API version this store is pinned to"
                value={draft.apiVersion}
                onChange={(apiVersion) => setDraft((d) => ({ ...d, apiVersion }))}
                placeholder="Leave empty to run at the mirror's own version"
                code
              />
            </Field>
            <Field label="Protected customer data">
              <Select
                id="os-store-protected"
                label="Shopify's protected customer data approval level"
                value={draft.protectedDataLevel}
                onChange={(protectedDataLevel) => setDraft((d) => ({ ...d, protectedDataLevel }))}
              >
                <option value="">Not set</option>
                <option value="none">none</option>
                <option value="level1">level1</option>
                <option value="level2">level2</option>
              </Select>
            </Field>
            <Check
              checked={draft.isDevelopment}
              onChange={(isDevelopment) => setDraft((d) => ({ ...d, isDevelopment }))}
            >
              This is a development store
            </Check>
            {draft.isDevelopment ? (
              <Field label="Stands in for">
                <Select
                  id="os-store-development-of"
                  label="The live store this development store stands in for"
                  value={draft.developmentOfStoreId}
                  onChange={(developmentOfStoreId) => setDraft((d) => ({ ...d, developmentOfStoreId }))}
                >
                  <option value="">Not paired</option>
                  {list.stores
                    .filter((store) => !store.isDevelopment)
                    .map((store) => (
                      <option key={store.id} value={store.id}>
                        {storeLongLabel(store)}
                      </option>
                    ))}
                </Select>
              </Field>
            ) : null}
            <Caption>
              A development store is attached and mirrored like any other: orders placed against it
              are real orders in that store, under its own id, and excluded from every read scoped
              to the live one.
            </Caption>
            <div className="os-store-bandact">
              <Button tone="quiet" onClick={() => setRegistering(false)}>
                Back to the list
              </Button>
              {canRegister ? (
                <Button
                  tone="primary"
                  busy={writes.busy === "create" || writes.busy === "bind"}
                  busyLabel="Attaching"
                  onClick={() => void register()}
                >
                  Register and attach
                </Button>
              ) : null}
            </div>
            {canRegister ? (
              <Caption>
                Two writes: the store is registered first, then this deployable is attached to it.
                If the second is refused the store still exists and you can attach it from the list.
              </Caption>
            ) : (
              <Caption>
                A domain and a Storefront token name are the least a storefront needs. The rest can
                follow.
              </Caption>
            )}
          </>
        ) : (
          <>
            {list.state === "failed" ? (
              <Notice tone="error" sentence="The cluster's stores could not be read." detail={list.error} />
            ) : list.stores.length === 0 ? (
              <Caption>
                No Shopify store is registered on this cluster yet. Register the one this storefront
                fronts, and it becomes the record everything else reads.
              </Caption>
            ) : (
              <ChoiceStack
                name="os-store-choice"
                label="The store this storefront fronts"
                value={chosen}
                onChange={setChosen}
                options={options}
              />
            )}
            <div className="os-store-bandact">
              {onCancel ? (
                <Button tone="quiet" onClick={onCancel}>
                  Cancel
                </Button>
              ) : null}
              <Button tone="quiet" onClick={() => setRegistering(true)}>
                Register a store
              </Button>
              {canAttach ? (
                <Button tone="primary" busy={writes.busy === "bind"} busyLabel="Attaching" onClick={() => void attach()}>
                  Attach
                </Button>
              ) : null}
            </div>
            <Caption>
              The storefront names this store; it does not copy it. The domain the page calls and
              the Storefront token the edge serves are both read from the store row, so changing
              the store here changes what this deployable serves with no second edit anywhere.
            </Caption>
          </>
        )}
      </Panel>
    </>
  );
}

/** What a choice says about itself beyond its name. */
function describeStore(store: StoreRow, all: readonly StoreRow[]): string {
  const parts: string[] = [];
  if (store.status !== "") parts.push(store.status);
  if (store.plan !== "") parts.push(store.plan);
  if (store.isDevelopment) {
    const stands = all.find((s) => s.id === store.developmentOfStoreId);
    parts.push(stands ? `development store for ${stands.domain}` : "development store");
  }
  return parts.join(" · ");
}

/**
 * The row id a domain gets.
 *
 * DERIVED, NOT RANDOM. Every mirrored row is scoped by store id, and a random
 * id would let the same Shopify store be attached twice under two rows whose
 * mirrors then diverge. `createStore` refuses a duplicate id, so deriving it
 * turns "this store is already registered" into a refusal instead of a second
 * record of one store -- which is the failure this whole epic is about.
 */
function slugOf(domain: string): string {
  const base = domain
    .trim()
    .toLowerCase()
    .replace(/\.myshopify\.com$/, "")
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return base === "" ? "store" : base;
}

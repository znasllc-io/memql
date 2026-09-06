import { useState } from "react";
import { ArrowLeft } from "lucide-react";

import { Button, Caption, Field, Head, Input, Notice, Panel, Select, Subhead } from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { BLANK_STORE, type NewStore, type StoreWrites } from "./useStores";

// Add a store.
//
// ===========================================================================
// THE THREE CREDENTIAL FIELDS TAKE THE NAME OF A SECRET, NEVER A TOKEN
// ===========================================================================
// This is the whole reason this form is worth having its own file. A Shopify
// Admin token is the keys to a merchant's shop, and `v1:shopify:store` stores
// `adminTokenRef` / `storefrontTokenRef` / `webhookSecretRef` -- the NAMES of
// `v1:platform:globalSecret` rows -- precisely so that no read of the store
// row can return one. The mutation's own doc comment says it: "a mutation
// that took a token would put it in the call string, which is rendered into
// logs on a parse error."
//
// A form that asked for "Admin token" would put that token in this browser's
// memory, in the rendered MemQL call, and on somebody's screen -- which is
// exactly what the reference indirection exists to prevent, defeated by a
// field label. So the labels say "secret name", the help text says what a
// secret name is and where one is made, the placeholders are secret names
// rather than token-shaped strings, and there is deliberately no control here
// that accepts or displays a token value.
//
// The secret is created FIRST, at Settings -> Keys or with `memql env`, and
// named here afterwards.

export function AddStoreForm({
  writes,
  onBack,
  onAdded,
}: {
  writes: StoreWrites;
  onBack: () => void;
  /** Called once the engine has accepted the store. */
  onAdded: () => void;
}) {
  const [draft, setDraft] = useState<NewStore>(BLANK_STORE);
  const set = (key: keyof NewStore) => (value: string) => setDraft((held) => ({ ...held, [key]: value }));

  const idGiven = draft.storeId.trim() !== "";
  const domainGiven = draft.domain.trim() !== "";
  const ready = idGiven && domainGiven;
  const busy = writes.busy === "create";

  async function submit() {
    const done = await writes.createStore(draft);
    if (done) onAdded();
  }

  // ABSENT, NOT DISABLED (DESIGN.md rule 12). An incomplete form does not
  // offer an act that would be refused; the bar's detail line says what is
  // still missing, which is the thing a greyed button never tells anybody.
  const acts: Act[] = ready
    ? [{ label: "Add the store", tone: "primary", busy, onAct: () => void submit() }]
    : [];

  return (
    <div className="os-stores-pane">
      <div className="os-stores-scroll">
        <Panel label="Add a store">
          <Head title="Add a store">
            <Button tone="quiet" onClick={onBack}>
              <ArrowLeft size={13} aria-hidden /> Stores
            </Button>
          </Head>

          <Subhead>Which store</Subhead>
          <div className="os-stores-form">
            <Field label="Store id">
              <Input
                id="os-store-id"
                label="Store id, the URL segment its webhooks arrive on"
                value={draft.storeId}
                onChange={set("storeId")}
                placeholder="acme-widgets"
              />
            </Field>
            <Field label="Domain">
              <Input
                id="os-store-domain"
                label="The myshopify.com domain"
                value={draft.domain}
                onChange={set("domain")}
                placeholder="acme-widgets.myshopify.com"
              />
            </Field>
            <Field label="Name">
              <Input
                id="os-store-name"
                label="Human-readable name for this store"
                value={draft.name}
                onChange={set("name")}
                placeholder="Acme Widgets"
              />
            </Field>
            <Field label="App client id">
              <Input
                id="os-store-client-id"
                label="The custom-distribution app's client id, from the Dev Dashboard"
                value={draft.appClientId}
                onChange={set("appClientId")}
              />
            </Field>
          </div>
          <Caption>
            The store id is stable for the life of the store -- it is the segment its webhooks arrive
            on, at <span className="os-mono">/inbound/shopify-&lt;storeId&gt;</span>. The domain is
            the one identifier Shopify never changes; a storefront&rsquo;s public domain does.
          </Caption>

          {/* ---- the credentials, and the sentence that explains them ---- */}
          <Subhead>Credentials, by reference</Subhead>
          <Caption>
            These three fields take the NAME of a secret, never a token. Create the secret first --
            Settings &rarr; Keys, or <span className="os-mono">memql env</span> -- and name it here.
            The store row is read by this window, so a token stored on it would be a token on a
            screen; a reference is what keeps the secret in the cluster.
          </Caption>
          <div className="os-stores-form">
            <Field label="Admin token secret name">
              <Input
                id="os-store-admin-ref"
                label="Name of the secret holding the Admin API token. Not the token."
                value={draft.adminTokenRef}
                onChange={set("adminTokenRef")}
                placeholder="SHOPIFY_ACME_ADMIN_TOKEN"
              />
            </Field>
            <Field label="Storefront token secret name">
              <Input
                id="os-store-storefront-ref"
                label="Name of the secret holding the Headless channel's Storefront token. Not the token."
                value={draft.storefrontTokenRef}
                onChange={set("storefrontTokenRef")}
                placeholder="SHOPIFY_ACME_STOREFRONT_TOKEN"
              />
            </Field>
            <Field label="Webhook secret name">
              <Input
                id="os-store-webhook-ref"
                label="Name of the secret holding the webhook signing secret. Not the secret."
                value={draft.webhookSecretRef}
                onChange={set("webhookSecretRef")}
                placeholder="SHOPIFY_ACME_WEBHOOK_SECRET"
              />
            </Field>
          </div>

          <Subhead>How it is read</Subhead>
          <div className="os-stores-form">
            <Field label="API version">
              <Input
                id="os-store-api-version"
                label="Admin API version this store is pinned to"
                value={draft.apiVersion}
                onChange={set("apiVersion")}
                placeholder="2026-07"
              />
            </Field>
            <Field label="Protected customer data level">
              <Select
                id="os-store-protected-level"
                label="Shopify's approved protected customer data level"
                value={draft.protectedDataLevel}
                onChange={set("protectedDataLevel")}
              >
                <option value="">Not stated</option>
                <option value="none">none</option>
                <option value="level1">level1</option>
                <option value="level2">level2</option>
              </Select>
            </Field>
            <Field label="Owner">
              <Input
                id="os-store-owner"
                label="The user a customers/data_request export is filed under"
                value={draft.ownerUserId}
                onChange={set("ownerUserId")}
                placeholder="v1:identity:user:..."
              />
            </Field>
          </div>
          <Caption>
            The API version must match the one the mirror was generated from, or every call is
            refused rather than attempted. The protected data level is what Shopify approved: below
            the level a field needs, Shopify returns null and the mirror stores null, so this is the
            difference between &ldquo;the customer has no phone number&rdquo; and &ldquo;we are not
            approved to see it&rdquo;.
          </Caption>

          {writes.error === "" ? null : (
            <Notice
              tone="error"
              sentence="The store was not added."
              next="Nothing was written; what is above is still as you typed it."
              detail={writes.error}
            />
          )}
        </Panel>
      </div>

      <ActionBar
        state="Not registered"
        detail={
          ready
            ? "Nothing is written until you add it, and nothing is fetched from Shopify yet."
            : !idGiven && !domainGiven
              ? "A store id and a domain are what the engine requires; the rest can follow."
              : idGiven
                ? "A domain is still needed."
                : "A store id is still needed."
        }
        tone="none"
        acts={acts}
      >
        <Button tone="quiet" onClick={onBack}>
          Cancel
        </Button>
      </ActionBar>
    </div>
  );
}

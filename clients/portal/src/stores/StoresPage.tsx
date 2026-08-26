import { useState, type FormEvent, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import {
  Badge,
  Band,
  Button,
  Container,
  EmptyState,
  ErrorNotice,
  Field,
  PageHeader,
  Skeleton,
  TextInput,
} from "../ui";
import { STORE_CONCEPT_ID } from "./concepts";
import type { StoreHealth } from "./health";
import { StoresRefused } from "./StoresRefused";
import { storePath } from "./urls";
import { useStores, type CreateStoreInput } from "./useStores";

// The Stores LIST screen: every configured Shopify store with its live
// health, plus the add-a-store form. The per-store actions live one level
// down at /stores/:storeId, the same split Sites uses and for the same
// reason -- an operator picks a store deliberately before backfilling it.

export function StoresPage(): ReactNode {
  const navigate = useNavigate();
  const { health, healthLoading, healthError, refreshHealth, role, isOwner, accessResolved, createBusy, createError, createStore } =
    useStores();

  if (!accessResolved) {
    return <Skeleton variant="text" width="w-40" />;
  }
  if (!isOwner) {
    return <StoresRefused role={role} resolved={accessResolved} />;
  }

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={STORE_CONCEPT_ID}
          title="Stores"
          blurb="Every Shopify store this cluster mirrors. Shopify owns the data; MemQL holds a generated copy of it, kept current by webhooks and repaired by reconciliation. A store's three credentials live as references to secrets -- the row itself never carries a token."
        />

        <Band title="Add a store">
          <NewStoreForm busy={createBusy} error={createError} onCreate={createStore} />
        </Band>

        <Band
          title="Configured stores"
          meta={
            <Button size="xs" onClick={refreshHealth} disabled={healthLoading}>
              {healthLoading ? "Checking..." : "Refresh"}
            </Button>
          }
          panel
        >
          {healthError ? <ErrorNotice sentence="Could not check the stores' health." next="The list below is still what is configured." detail={healthError} /> : null}
          {healthLoading && health.length === 0 ? <Skeleton variant="text" width="w-64" /> : null}
          {!healthLoading && health.length === 0 && healthError === "" ? (
            <EmptyState statement="No store is configured. Create the custom-distribution app in the Shopify Dev Dashboard, seal its three credentials as secrets, then add the store above -- the connector runbook walks the whole sequence." />
          ) : null}
          <ul className="flex flex-col gap-2">
            {health.map((store) => (
              <li key={store.storeId}>
                <StoreCard store={store} onOpen={() => navigate(storePath(store.storeId))} />
              </li>
            ))}
          </ul>
        </Band>
      </section>
    </Container>
  );
}

function statusTone(status: string): "ok" | "warn" | "danger" | "neutral" {
  switch (status) {
    case "live":
      return "ok";
    case "backfilling":
    case "configured":
      return "warn";
    case "error":
      return "danger";
    default:
      return "neutral";
  }
}

function StoreCard({ store, onOpen }: { store: StoreHealth; onOpen: () => void }): ReactNode {
  return (
    <button
      type="button"
      onClick={onOpen}
      className="w-full rounded-lg border border-line bg-surface px-4 py-3 text-left hover:border-accent"
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{store.domain}</span>
        <Badge tone={statusTone(store.status)}>{store.status === "" ? "unknown" : store.status}</Badge>
        {store.scopesMissing.length > 0 ? (
          <Badge tone="warn">{store.scopesMissing.length} scope(s) missing</Badge>
        ) : null}
        {store.apiVersion !== "" && store.apiVersion !== store.mirrorApiVersion ? (
          <Badge tone="danger">pinned {store.apiVersion}, mirror {store.mirrorApiVersion}</Badge>
        ) : null}
      </div>
      <p className="mt-1 text-xs text-muted">
        {store.domains.length} mirrored domain(s) &middot; protected data {store.protectedDataLevel || "none"}
        {store.driftLast > 0 ? ` · ${store.driftLast} row(s) of drift on the last reconcile` : ""}
      </p>
    </button>
  );
}

// The add form takes the three credentials as REFERENCES -- the names of
// globalSecret rows -- and says so, because the alternative reading (paste
// the token here) is the one that would put a merchant's Admin token in a
// browser and in this app's memory.
function NewStoreForm({
  busy,
  error,
  onCreate,
}: {
  busy: boolean;
  error: string;
  onCreate: (input: CreateStoreInput) => void;
}): ReactNode {
  const [form, setForm] = useState<CreateStoreInput>({
    storeId: "",
    domain: "",
    name: "",
    appClientId: "",
    adminTokenRef: "",
    storefrontTokenRef: "",
    webhookSecretRef: "",
    apiVersion: "",
    protectedDataLevel: "",
    ownerUserId: "",
  });

  const set = (key: keyof CreateStoreInput) => (value: string) => setForm((f) => ({ ...f, [key]: value }));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    onCreate(form);
  };

  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Store id" hint="The URL segment its webhooks arrive on. Stable for the life of the store.">
          <TextInput value={form.storeId} onChange={set("storeId")} placeholder="acme-widgets" />
        </Field>
        <Field label="Domain" hint="The myshopify.com domain. The one identifier Shopify never changes.">
          <TextInput value={form.domain} onChange={set("domain")} placeholder="acme-widgets.myshopify.com" />
        </Field>
        <Field label="Name">
          <TextInput value={form.name} onChange={set("name")} placeholder="Acme Widgets" />
        </Field>
        <Field label="App client id" hint="From the Dev Dashboard. Recorded for the audit trail.">
          <TextInput value={form.appClientId} onChange={set("appClientId")} />
        </Field>
        <Field label="Admin token secret" hint="The NAME of a globalSecret row, not the token.">
          <TextInput value={form.adminTokenRef} onChange={set("adminTokenRef")} placeholder="SHOPIFY_ACME_ADMIN_TOKEN" />
        </Field>
        <Field label="Storefront token secret" hint="The NAME of a globalSecret row.">
          <TextInput value={form.storefrontTokenRef} onChange={set("storefrontTokenRef")} />
        </Field>
        <Field label="Webhook secret" hint="The NAME of a globalSecret row. Resolved per store by the inbound receiver.">
          <TextInput value={form.webhookSecretRef} onChange={set("webhookSecretRef")} />
        </Field>
        <Field label="API version" hint="Must match the version the mirror was generated from.">
          <TextInput value={form.apiVersion} onChange={set("apiVersion")} placeholder="2026-07" />
        </Field>
        <Field label="Protected data level" hint="none, level1 or level2, as Shopify approved.">
          <TextInput value={form.protectedDataLevel} onChange={set("protectedDataLevel")} placeholder="none" />
        </Field>
        <Field label="Owner" hint="The user a customers/data_request export is filed under.">
          <TextInput value={form.ownerUserId} onChange={set("ownerUserId")} />
        </Field>
      </div>
      {error ? <ErrorNotice sentence="The store was not added." next="Check the fields above and try again." detail={error} /> : null}
      <div>
        <Button type="submit" disabled={busy || form.storeId === "" || form.domain === ""}>
          {busy ? "Adding..." : "Add store"}
        </Button>
      </div>
    </form>
  );
}

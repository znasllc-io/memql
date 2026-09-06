import { useCallback, useState } from "react";
import { Plus, RefreshCw, Store as StoreIcon } from "lucide-react";

import { Button, Caption, Head, Notice, Panel, Row as ListRow } from "../../kit";
import { FigureValue } from "../../cluster/FigureValue";
import { isPositive } from "../../cluster/figure";
import { mirroredDomainCount, type StoreHealth } from "./health";
import { AddStoreForm } from "./AddStoreForm";
import { StorePage } from "./StorePage";
import { apiVersionMismatch, protectedDataWord, statusChipWord, statusTone } from "./words";
import { useStoreHealth, useStoreWrites } from "./useStores";

// The Stores section: the Shopify connector's operator surface.
//
// THREE SIBLING VIEWS, ONE AT A TIME, ONE HEAD EACH (DESIGN.md rule 11):
//
//     List --add a store--> Add
//       |
//       '--select a store--> Store
//
// A store's detail is tall -- four bands, a scope list and a table with one
// row per mirrored concept -- so it REPLACES the list and carries a quiet
// "<- Stores" in its own Head, rather than sitting beside it in a second
// scroller. Two Heads in one scroller is the tell that neither happened.

/** Which view the section is showing. */
type StoresView = { kind: "list" } | { kind: "add" } | { kind: "store"; storeId: string };

export function StoresSection({
  hideQuietDomains,
  onOpenSettings,
  onAsk,
}: {
  hideQuietDomains: boolean;
  onOpenSettings: () => void;
  onAsk?: (tag: string) => void;
}) {
  const [view, setView] = useState<StoresView>({ kind: "list" });
  const reading = useStoreHealth();
  // The writes re-read on completion, because nothing here is a live feed:
  // an accepted pause that left the page reading "Live" is indistinguishable
  // from a pause the engine ignored. `reread` is `useReading`'s own stable
  // callback, so it is passed through rather than wrapped -- a wrapper would
  // take a new identity on every render and rebuild every write with it.
  const reread = reading.reread;
  const writes = useStoreWrites(reread);

  const backToList = useCallback(() => setView({ kind: "list" }), []);

  if (view.kind === "add") {
    return <AddStoreForm writes={writes} onBack={backToList} onAdded={backToList} />;
  }

  if (view.kind === "store") {
    const store = reading.stores.find((s) => s.storeId === view.storeId);
    // The store left the report while this was open -- redacted under
    // shop/redact, say. The list is what the person sees, rather than a page
    // about a store that is gone.
    if (store === undefined) return renderList();
    return (
      <StorePage
        key={store.storeId}
        store={store}
        writes={writes}
        hideQuietDomains={hideQuietDomains}
        readAt={reading.at}
        onBack={backToList}
        onReread={reread}
        onOpenSettings={onOpenSettings}
        onAsk={onAsk}
      />
    );
  }

  return renderList();

  function renderList() {
    const stores = reading.stores;
    return (
      <div className="os-app-stack">
        {/* ONE PRIMARY ACTION, and nothing else standing (rule 1). There is
            no Refine: this list is the stores a cluster mirrors, which is a
            handful, and rule 2 is explicit that a section never shows filter
            chrome over no content. */}
        <Head title="Stores" meta={reading.state === "read" ? stores.length : undefined}>
          <Button tone="quiet" onClick={reread} ariaLabel="Re-read every store's health">
            <RefreshCw size={13} aria-hidden /> Re-read
          </Button>
          <Button tone="primary" ariaLabel="Add a store" onClick={() => setView({ kind: "add" })}>
            <Plus size={13} aria-hidden /> Add a store
          </Button>
        </Head>

        {writes.error === "" ? null : (
          <Notice
            tone="error"
            sentence="That did not run."
            next="Nothing changed."
            detail={writes.error}
          />
        )}
        {writes.note === "" ? null : (
          <Notice tone="info" sentence={writes.note}>
            <Button tone="quiet" onClick={writes.clearNote}>
              Got it
            </Button>
          </Notice>
        )}

        {reading.state === "failed" ? (
          <Notice
            tone="error"
            sentence="This cluster did not report its stores."
            next="Only a cluster owner may read them, and the engine decides -- this window only decides what to offer."
            detail={reading.error}
          >
            <Button onClick={reread}>Try again</Button>
          </Notice>
        ) : null}

        {/* NO PANEL AT ALL WHEN THE READ WAS REFUSED. The notice above is the
            whole answer; an empty bordered box beneath it reads as a list
            that came back with nothing in it, which is the opposite of what
            happened. */}
        {reading.state === "failed" ? null : (
          <Panel label="Configured stores">
            {reading.state === "unread" || (reading.state === "reading" && stores.length === 0) ? (
              <Caption>Reading every configured store&rsquo;s health.</Caption>
            ) : null}

            {reading.state === "read" && stores.length === 0 ? (
              <Caption>
                No store is configured. Create the custom-distribution app in the Shopify Dev
                Dashboard, seal its three credentials as secrets, then add the store above -- the
                connector runbook walks the whole sequence.
              </Caption>
            ) : null}

            {stores.map((store) => (
              <StoreLine
                key={store.storeId}
                store={store}
                onOpen={() => setView({ kind: "store", storeId: store.storeId })}
              />
            ))}
          </Panel>
        )}

        {reading.at === null ? null : (
          <Caption>
            Read at {reading.at.toLocaleTimeString()}. A store&rsquo;s health is computed from the
            connector&rsquo;s sync state and its live client rather than carried on the row, so this
            is not a live feed -- re-read to see changes made since.
          </Caption>
        )}
      </div>
    );
  }
}

/**
 * One store in the list.
 *
 * EVERY NUMBER IS A Figure. Drift is on the line when there is some AND when
 * there is no measurement at all -- an em dash carrying "nothing has reported
 * this yet" -- and it is silent only for a measured zero, which is the one
 * state that genuinely needs no words. A store that has never reconciled has
 * not reconciled cleanly, and `?? 0` is how the two become the same sentence.
 */
function StoreLine({ store, onOpen }: { store: StoreHealth; onOpen: () => void }) {
  const mirrored = mirroredDomainCount(store);
  const drift = store.driftLast;
  const mismatch = apiVersionMismatch(store);
  const missing = store.scopesMissing.length;
  return (
    <ListRow
      icon={<StoreIcon size={16} aria-hidden />}
      name={store.domain === "" ? store.storeId : store.domain}
      current={store.status === "live"}
      dim={store.status === "paused"}
      onOpen={onOpen}
      state={
        <>
          <span className="os-stores-status" data-tone={statusTone(store.status)}>
            {statusChipWord(store.status)}
          </span>
          {missing > 0 ? (
            <span
              className="os-stores-status"
              data-tone="warn"
              title="Shopify returns null for the fields a missing scope covers, so those domains are quietly incomplete rather than broken."
            >
              {missing} scope{missing === 1 ? "" : "s"} missing
            </span>
          ) : null}
          {mismatch ? (
            <span
              className="os-stores-status"
              data-tone="danger"
              title={`The mirror was generated from ${store.mirrorApiVersion}. A call is refused rather than attempted.`}
            >
              pinned {store.apiVersion}, mirror {store.mirrorApiVersion}
            </span>
          ) : null}
        </>
      }
    >
      <span className="os-stores-subline">
        <FigureValue figure={mirrored} /> mirrored domain
        {mirrored.kind === "measured" && mirrored.value === 1 ? "" : "s"}
        {" · "}
        protected data {protectedDataWord(store)}
        {/* THE DRIFT FIGURE IS THE CONNECTOR'S, NOT THIS STORE'S -- the sync
            state it is summed from is keyed by (concept, connector) with no
            store in the key. With one store configured, which is the usual
            case, they are the same number; with two they are not, and the
            title is where that is said rather than in a word on every line. */}
        <span title="Rows the last reconcile found disagreeing with the origin, across the whole shopify connector. The sync state it is summed from is not scoped per store.">
          {isPositive(drift) ? (
            <>
              {" · "}
              <FigureValue figure={drift} /> rows of drift on the last reconcile
            </>
          ) : drift.kind === "absent" ? (
            <>
              {" · "}
              drift <FigureValue figure={drift} />
            </>
          ) : null}
        </span>
      </span>
    </ListRow>
  );
}

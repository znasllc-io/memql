import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { useCallback, useMemo, useState } from "react";
import { RefreshCw, ShoppingBag } from "lucide-react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import {
  Button,
  Caption,
  Chips,
  Fact,
  Facts,
  Head,
  Measure,
  Notice,
  Panel,
  Subhead,
  absent,
  roleAdmits,
} from "../../../kit";
import { useSession } from "../../../chrome/access";
import { OpenLogsButton } from "../../../logs/OpenLogs";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { boundStoreId, siteName, type SiteRow } from "../rows";
import type { DomainState, StoreHealth } from "./health";
import { StorePicker } from "./StorePicker";
import { storeLabel, type StoreRow } from "./rows";
import {
  healthFor,
  useDevelopmentStores,
  useStore,
  useStoreHealth,
  useStoreWrites,
} from "./useStore";
import {
  apiVersionMismatch,
  apiVersionMismatchSentence,
  byDriftDescending,
  phaseTone,
  protectedDataWord,
  readingFor,
  statusForAct,
  statusTone,
} from "./words";

// THE STORE, ON THE DEPLOYABLE THAT FRONTS IT (epic memql#5530, issue
// memql#5541).
//
// ===========================================================================
// WHY THIS IS HERE AND NOT IN AN APP OF ITS OWN
// ===========================================================================
// Everything about a storefront is configured on its deployable (design D5),
// and until this landed the most important thing about one was configured
// somewhere else -- the Stores app -- and recorded twice: the site's binding
// carried a COPY of the store's domain and its Storefront token reference,
// edited at a different authorization tier from the store row that also held
// them (gap G4). The binding names the store now. This panel is where that
// one record is read and changed.
//
// ===========================================================================
// WHAT DELIBERATELY DID NOT MOVE HERE
// ===========================================================================
// Backfilling a domain, reconciling it, pausing it on its own, and retrying
// or discarding a dead letter belong to EVERY connector, not to Shopify. They
// live in Cluster > Data origins and they stay there. The mirror table below
// is read-only and says so in words, because a reader who wants to backfill
// one domain needs to be told where that lives -- not given a second copy of
// the button that would then disagree with the first the moment one of them
// learned something.
//
// ===========================================================================
// THE PANE, NOT A DIALOG
// ===========================================================================
// The deployable's other details (Traffic, App values) open in
// `DetailDialog`, which is 680px with its own header and right for a short
// read and a small form. A store is neither: it is credentials, a scope
// comparison, a subscription record, a paired development store and a table
// per mirrored concept. DESIGN.md rule 9 says real estate belongs to content
// and rule 11 says a tall detail replaces its list rather than sharing a
// scroll column, so this takes the pane on the list's own gutters -- the
// arrangement `whereItLives` already uses.

export interface StorePanelProps {
  site: SiteRow;
  /**
   * `execute app:deployables/store`, held by owner and developer. False hides
   * every control here. It attaches a store; it does not register, pause or
   * resume one, or reconcile the cluster's subscriptions, which are a cluster
   * owner's and drawn only for one.
   */
  canBind: boolean;
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
}

export function StorePanel({ site, canBind, trail, back }: StorePanelProps) {
  const storeId = boundStoreId(site);
  const bound = useStore(storeId);
  const health = useStoreHealth();
  const [picking, setPicking] = useState(false);

  // Both readings re-read after any accepted write: neither is a live feed,
  // and a paused store still reading "Live" looks exactly like a write the
  // engine ignored.
  const onWritten = useCallback(() => {
    bound.reread();
    health.reread();
  }, [bound, health]);
  const writes = useStoreWrites(onWritten);

  const store = bound.store;
  const report = healthFor(health.all, storeId);
  const name = siteName(site);
  // THE CLUSTER OWNER, and not the store part (Connect Shopify, D15).
  // Registering a store creates a v1:shopify:store row, which the engine
  // refuses below a cluster owner, and pausing or resuming one writes a store
  // row that exists, which its tier keeps a cluster owner's. Reconciling
  // subscriptions walks every ingesting store on the cluster, not the one this
  // storefront fronts. A developer holds the part and none of those, so all
  // three are absent for them rather than offered (DESIGN.md rule 12).
  const isOwner = roleAdmits(useSession().access?.role ?? "", { min: "owner" });

  // THE PICKER REPLACES THE PANEL RATHER THAN SITTING INSIDE IT. Choosing
  // what a live storefront talks to is the whole of what somebody is doing
  // while they do it; a picker beside the store it is about invites a
  // comparison that the list already makes.
  const choosing = picking || (store === null && storeId === "" && canBind);

  // ONE PRIMARY ACTION, AND NOT WHILE IT IS BEING TAKEN (DESIGN.md rule 1).
  // While the picker is open the Head carries navigation and nothing else: a
  // "Change store" control standing over the surface that changes the store
  // is an act offered a second time, and Re-read and Logs are about a store
  // the person is in the middle of replacing.
  const head = (
    <Head title="Store" breadcrumbs={trail} back={back}>
      {choosing || store === null || !canBind ? null : (
        <Button tone="quiet" onClick={() => setPicking(true)} ariaLabel={`Change the store ${name} fronts`}>
          Change store
        </Button>
      )}
      {choosing || store === null ? null : (
        <Button
          tone="quiet"
          onClick={() => {
            bound.reread();
            health.reread();
          }}
          ariaLabel={`Re-read the store ${name} fronts`}
        >
          <RefreshCw size={13} aria-hidden /> Re-read
        </Button>
      )}
      {choosing || store === null ? null : (
        <OpenLogsButton
          iconOnly
          subject={store.id}
          subjectConcept={Concepts.SHOPIFY_STORE}
          ariaLabel={`Logs for ${storeLabel(store)}`}
        />
      )}
    </Head>
  );

  if (choosing) {
    return (
      <>
        {head}
        <StorePicker
          site={site}
          currentStoreId={storeId}
          writes={writes}
          mayRegister={isOwner}
          onDone={() => {
            setPicking(false);
            bound.reread();
            health.reread();
          }}
          onCancel={storeId === "" ? undefined : () => setPicking(false)}
        />
      </>
    );
  }

  return (
    <>
      {head}

      <WriteNotices writes={writes} />

      {storeId === "" ? (
        <Panel label="No store attached">
          <Caption>
            This storefront is not attached to a Shopify store, so it serves no catalog and its
            pages can reach nothing. Only someone holding the store permission can attach one.
          </Caption>
        </Panel>
      ) : bound.state === "failed" ? (
        <Notice
          tone="error"
          sentence="The store this storefront names could not be read."
          next="The deployable still serves its files. Until the store reads back, no catalog is reachable and the page's policy names no store."
          detail={bound.error}
        />
      ) : store === null && bound.state === "read" ? (
        <Notice
          tone="error"
          sentence="This storefront names a store that is not on this cluster."
          next={
            canBind
              ? "Attach one to serve a catalog."
              : "Only someone holding the store permission can attach one."
          }
        />
      ) : store === null ? (
        <Caption>Reading the store this storefront fronts.</Caption>
      ) : (
        <>
          <Identity store={store} report={report} />
          {/* TWO COLUMNS WHERE THERE IS ROOM, and this is DESIGN.md rule 9
              rather than decoration. Every block here is text at a readable
              measure -- the app caps a caption at 84ch, which is right -- so
              in one column the pane painted 800px of dead space beside a
              600px stripe at the default window size. The measure still
              belongs to the THING; the COLUMNS are what use the window.

              `auto-fit` with a 28rem floor rather than a width query: below
              one column's worth it collapses to one, which is the same
              arrangement the narrow window wants and needs no second rule to
              say so. The mirror table stays OUTSIDE, because a table is the
              one thing here that wants the whole width. */}
          <div className="os-store-grid">
            <Connection store={store} report={report} />
            <Scopes report={report} />
            <Subscriptions report={report} writes={writes} isOwner={isOwner} />
            <DevelopmentStore store={store} />
          </div>
          <Mirror report={report} />
          <Acts store={store} report={report} writes={writes} isOwner={isOwner} />
          {health.at === null ? null : (
            <Caption>
              Read at {health.at.toLocaleTimeString()}. A store&rsquo;s health is computed from the
              connector&rsquo;s sync state and its live client rather than carried on the row, so
              this is not a live feed -- re-read to see changes made since.
            </Caption>
          )}
        </>
      )}
    </>
  );
}

function WriteNotices({ writes }: { writes: ReturnType<typeof useStoreWrites> }) {
  return (
    <>
      {writes.error === "" ? null : (
        <Notice
          tone="error"
          sentence="That did not run."
          next="Nothing changed. What is below is still the last thing read."
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
    </>
  );
}

// ---------------------------------------------------------------------------
// Identity: the one fact somebody opened this panel to check
// ---------------------------------------------------------------------------

/**
 * The domain, the state, and one line of what the store is.
 *
 * THE DOMAIN IS THE SUBJECT AND IT IS TYPESET AS ONE. It is the identifier
 * Shopify never changes, the one an operator checks against the Shopify
 * admin, and the one that appears in the served policy -- so it carries the
 * weight here and everything below it is quiet.
 *
 * THE STATE IS THE STORE'S, NOT THE DEPLOYABLE'S. A live storefront can front
 * a paused store, and the two readings must not be confusable: the window's
 * own action bar says what the DEPLOYABLE is doing, and this word says what
 * the store is.
 */
function Identity({ store, report }: { store: StoreRow; report: StoreHealth | null }) {
  const status = report?.status ?? store.status;
  const meta = [store.name, store.plan, store.isDevelopment ? "development store" : ""]
    .map((part) => part.trim())
    .filter((part) => part !== "");
  return (
    <div className="os-store-identity">
      <div className="os-store-identity-line">
        <ShoppingBag size={18} aria-hidden />
        <strong className="os-mono">{storeLabel(store)}</strong>
        <span className="os-store-status" data-tone={statusTone(status)}>
          {status === "" ? "no status reported" : status}
        </span>
      </div>
      {meta.length === 0 ? null : <p className="os-store-note">{meta.join(" · ")}</p>}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Connection: how this cluster reaches the store
// ---------------------------------------------------------------------------

/**
 * The credentials, the version and the bucket.
 *
 * EVERY `Ref` FIELD IS A NAME AND THE PANEL SAYS SO. Each one NAMES a
 * `v1:platform:globalSecret` row; the token itself is not on the store row
 * and is not fetched here. The edge resolves the Storefront one at serve time
 * into the site's runtime-config document, and that is the only place any of
 * them is dereferenced.
 */
function Connection({ store, report }: { store: StoreRow; report: StoreHealth | null }) {
  const mismatch = report !== null && apiVersionMismatch(report);
  const version =
    store.apiVersion !== ""
      ? store.apiVersion
      : report !== null && report.mirrorApiVersion !== ""
        ? `${report.mirrorApiVersion} (the mirror's)`
        : "";
  return (
    <Panel label="Connection">
      <Subhead>Connection</Subhead>
      <Facts>
        <Fact
          label="Admin token"
          value={store.adminTokenRef}
          mono
          title="The name of the cluster secret holding the token. The value is never read by this app and never reaches a served byte."
        />
        <Fact
          label="Storefront token"
          value={store.storefrontTokenRef}
          mono
          title="The name of the cluster secret holding the token. The edge resolves it at serve time into this deployable's runtime-config document, which is the only place it is dereferenced."
        />
        <Fact
          label="Webhook secret"
          value={store.webhookSecretRef}
          mono
          title="The name of the cluster secret the inbound receiver verifies this store's deliveries against."
        />
        <Fact label="API version" value={version} mono />
        <Fact label="Protected customer data" value={report === null ? store.protectedDataLevel : protectedDataWord(report)} mono />
        <Fact label="App client id" value={store.appClientId} mono />
        <Fact label="Cost bucket" value={<CostBucket report={report} />} />
      </Facts>
      {mismatch && report !== null ? (
        <Notice tone="error" sentence={apiVersionMismatchSentence(report)} />
      ) : null}
      <Caption>
        The three token fields are NAMES of cluster secrets, never tokens. The cost bucket is
        Shopify&rsquo;s leaky bucket as of the last Admin call that reported one; it is not stored,
        so a cluster that has made no call since it started has no reading -- a different answer
        from a bucket at zero.
      </Caption>
    </Panel>
  );
}

/**
 * The cost bucket, or the honest absence of one.
 *
 * NEVER "0 of 0 points, restoring 0/s". A store nothing has called reports no
 * bucket at all, and coercing that to zeroes renders a store at its rate
 * limit -- the most alarming reading this panel can show -- for a store that
 * is simply idle.
 */
function CostBucket({ report }: { report: StoreHealth | null }) {
  const bucket = report?.costBucket ?? null;
  if (bucket === null) {
    return (
      <>
        <Measure figure={absent("unmeasured", "No Admin API call has reported a cost bucket yet.")} />{" "}
        <span className="os-store-note">not observed yet</span>
      </>
    );
  }
  return (
    <>
      <Measure figure={bucket.currentlyAvailable} format={round} /> of{" "}
      <Measure figure={bucket.maximumAvailable} format={round} /> points, restoring{" "}
      <Measure figure={bucket.restoreRate} format={round} suffix="/s" />
    </>
  );
}

function round(value: number): string {
  return String(Math.round(value));
}

// ---------------------------------------------------------------------------
// Scopes
// ---------------------------------------------------------------------------

function Scopes({ report }: { report: StoreHealth | null }) {
  if (report === null) return null;
  const needed = report.scopesNeeded.length;
  return (
    <Panel label="Scopes">
      <Subhead>Scopes</Subhead>
      {report.scopesGranted.length === 0 ? (
        <Caption>
          No scopes reported yet. The store reports them when it is first reached, and until then
          nothing can be checked against what the mirror needs -- {needed} in total.
        </Caption>
      ) : report.scopesMissing.length === 0 ? (
        <Caption>All {needed} scopes the mirror needs are granted.</Caption>
      ) : (
        <>
          <p className="os-store-line">
            {report.scopesMissing.length} of the {needed} scopes the mirror needs{" "}
            {report.scopesMissing.length === 1 ? "is" : "are"} not granted. Shopify returns null for
            the fields they cover, so the domains below them are quietly INCOMPLETE rather than
            broken -- nothing errors, and the missing values look exactly like values the merchant
            never entered.
          </p>
          <Chips label="Scopes the mirror needs and does not have">
            {report.scopesMissing.map((scope) => (
              <span key={scope} className="os-store-status" data-tone="warn">
                {scope}
              </span>
            ))}
          </Chips>
        </>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

function Subscriptions({
  report,
  writes,
  isOwner,
}: {
  report: StoreHealth | null;
  writes: ReturnType<typeof useStoreWrites>;
  isOwner: boolean;
}) {
  const record = report?.subscriptions ?? null;
  return (
    <Panel label="Subscriptions">
      <Subhead>Subscriptions</Subhead>
      {record === null ? (
        <Caption>
          No subscription reconcile has been recorded for this store yet. Shopify deletes a
          subscription after eight consecutive delivery failures, so a store that has gone quiet and
          a store nobody has checked look the same until this has run.
        </Caption>
      ) : (
        <>
          <p className="os-store-line">
            <Measure figure={record.existing} /> registered of <Measure figure={record.desired} />{" "}
            the mirror wants, last checked{" "}
            {record.at === "" ? "at an unrecorded time" : new Date(record.at).toLocaleString()}.
          </p>
          {record.failed.length === 0 ? null : (
            <>
              <p className="os-store-line" data-tone="danger">
                {record.failed.length} topic{record.failed.length === 1 ? "" : "s"} could not be
                registered:
              </p>
              <Chips label="Topics that could not be registered">
                {record.failed.slice(0, 12).map((topic) => (
                  <span key={topic} className="os-store-status" data-tone="warn">
                    {topic}
                  </span>
                ))}
              </Chips>
            </>
          )}
        </>
      )}
      {/* THE ACT NAMES ITS OWN SCOPE, because it is wider than this store.
          `shopifyEnsureSubscriptions` takes NO store argument -- it walks
          every ingesting store -- so a control here reading "reconcile this
          store" would be a claim the builtin cannot keep. And for the same
          reason it is a CLUSTER OWNER'S, like pause and resume: the store
          part a developer holds attaches this storefront's store and reaches
          no other. */}
      {!isOwner ? null : (
        <div className="os-store-bandact">
          <Button
            tone="quiet"
            busy={writes.busy === "subscriptions"}
            busyLabel="Reconciling subscriptions"
            onClick={() => void writes.ensureSubscriptions()}
          >
            <RefreshCw size={13} aria-hidden /> Reconcile subscriptions
          </Button>
          <Caption>
            Reconciles every ingesting store, not only this one -- the builtin takes no store. It runs
            on its own at boot and daily at 03:15.
          </Caption>
        </div>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The development store
// ---------------------------------------------------------------------------

/**
 * The store this storefront is exercised against before it serves shoppers.
 *
 * A SECOND ROW, NOT A MODE (design D8). A development store is attached and
 * mirrored like any other store, so an order placed against it lands in the
 * mirror under its own `storeId` and is already excluded from every read
 * scoped to the live one. What pairs the two is `developmentOfStoreId`; the
 * engine has no other way to know which development store belongs to which
 * storefront, and without the pairing two rows sit in one list with nothing
 * saying which is which.
 *
 * THE PANEL DOES NOT OFFER TO CREATE ONE HERE. Attaching a store -- live or
 * development -- is the picker's job, and one place to do a thing is the
 * whole point of this epic.
 */
function DevelopmentStore({ store }: { store: StoreRow }) {
  const paired = useDevelopmentStores(store.id);
  if (store.isDevelopment) {
    return (
      <Panel label="Development store">
        <Subhead>Development store</Subhead>
        <Caption>
          This storefront is bound to a development store. Orders placed against it are real orders
          in that store, mirrored under its own id and excluded from every read scoped to the live
          one.
        </Caption>
      </Panel>
    );
  }
  return (
    <Panel label="Development store">
      <Subhead meta={paired.state === "read" ? paired.stores.length : undefined}>Development store</Subhead>
      {paired.state === "failed" ? (
        <Notice tone="error" sentence="The development stores could not be read." detail={paired.error} />
      ) : paired.stores.length === 0 ? (
        <Caption>
          No development store stands in for this one. A development store is attached like any
          other store -- a second row, marked as one, naming this store -- and it is what a
          storefront is exercised against before it goes live.
        </Caption>
      ) : (
        <RecordList as="ul" label="Development stores">
          {paired.stores.map((dev) => <RecordRow key={dev.id} name={storeLabel(dev)} state={dev.status || "no status reported"} />)}
        </RecordList>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Mirror sync state -- read-only, and it says where its acts are
// ---------------------------------------------------------------------------

function Mirror({ report }: { report: StoreHealth | null }) {
  const shown = useMemo(
    () => (report === null ? [] : [...report.domains].sort(byDriftDescending)),
    [report],
  );
  return (
    <Panel label="Mirror sync state">
      <Subhead>Mirror sync state</Subhead>
      {/* THE SCOPE IS THE CONNECTOR'S, AND SAYING SO IS NOT A DETAIL.
          `v1:platform:syncState` is keyed by (concept, connector) with no
          store in the key, and the health handler hands the same slice to
          every store in its report -- so a cluster mirroring two shops shows
          both of them the identical table. Captioned "for THIS store" it
          would be a number under the wrong subject. */}
      <Caption>
        One row per mirrored concept, for the shopify connector as a whole. These rows are keyed by
        concept and connector rather than by store, so every store this cluster mirrors reports the
        same table.
      </Caption>
      {shown.length === 0 ? (
        <Caption>
          No domain has synced yet, so there is nothing to measure. A backfill is what starts one,
          and it lives in Cluster &rarr; Data origins.
        </Caption>
      ) : (
        <div className="os-store-tablewrap">
          <table className="os-store-table">
            <thead>
              <tr>
                <th scope="col">Domain</th>
                <th scope="col">Phase</th>
                <th
                  scope="col"
                  title="Rows the last reconcile found disagreeing with the origin. Repeated drift is a webhook that is not arriving."
                >
                  Drift
                </th>
                <th
                  scope="col"
                  title="Seconds between the origin's version of the last applied write and when MemQL applied it -- the mirror's staleness."
                >
                  Lag
                </th>
                <th
                  scope="col"
                  title="Pending and failed outbox entries for this concept: whether the drain is keeping up."
                >
                  Outbox
                </th>
                <th scope="col">Last applied</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((domain) => (
                <DomainRow key={domain.concept} domain={domain} />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {/* WHERE THE ACTS ARE, IN WORDS. A reader who wants to backfill one
          domain needs to be told where that lives, not given a second copy of
          the button. */}
      <Caption>
        This table is read-only. Backfilling a domain, reconciling it, pausing it on its own, and
        retrying or discarding a dead letter belong to every connector, so they live in Cluster
        &rarr; Data origins. What is here, for a cluster owner, is the subscription reconcile
        above and the store-wide pause below.
      </Caption>
    </Panel>
  );
}

function DomainRow({ domain }: { domain: DomainState }) {
  return (
    <tr>
      <td className="os-mono">{domain.concept}</td>
      <td>
        <span className="os-store-status" data-tone={phaseTone(domain.phase)}>
          {domain.phase === "" ? "idle" : domain.phase}
        </span>
      </td>
      <td>
        <Measure figure={domain.driftLast} />
      </td>
      <td>
        <Measure figure={domain.lagSeconds} suffix="s" />
      </td>
      <td>
        <Measure figure={domain.outboxDepth} />
      </td>
      <td className="os-store-note">
        {domain.lastAppliedAt === "" ? (
          <Measure figure={absent("unmeasured", "Live delivery has applied nothing for this domain yet.")} />
        ) : (
          new Date(domain.lastAppliedAt).toLocaleString()
        )}
      </td>
    </tr>
  );
}

// ---------------------------------------------------------------------------
// The acts that are this store's alone
// ---------------------------------------------------------------------------

/**
 * Pause and resume, on one control line.
 *
 * AN ACT THAT IS NOT LEGAL IS ABSENT, NEVER DISABLED (DESIGN.md rule 12).
 * `readingFor` computes them from the state, so a paused store gets Resume, an
 * ingesting one gets Pause, and a status this shell has no copy for gets
 * NEITHER -- an act is a claim about the state it acts from, and "pause it"
 * asserts the store is not already paused.
 *
 * NO SECOND ACTION BAR. The window already draws one, for the DEPLOYABLE's
 * lifecycle, and two bars in one window is what rule 12 exists to prevent.
 * These sit inline, under the state they act from, exactly as the runtime
 * settings panel's controls do.
 *
 * A CLUSTER OWNER'S ALONE. Both write a store row that exists, which the
 * store's tier keeps a cluster owner's; the store part a developer holds
 * attaches a store and changes nothing on it.
 */
function Acts({
  store,
  report,
  writes,
  isOwner,
}: {
  store: StoreRow;
  report: StoreHealth | null;
  writes: ReturnType<typeof useStoreWrites>;
  isOwner: boolean;
}) {
  if (report === null || !isOwner) return null;
  const reading = readingFor(report);
  if (reading.acts.length === 0) {
    return (
      <Panel label="Ingestion">
        <Subhead>Ingestion</Subhead>
        <Caption>{reading.detail}</Caption>
      </Panel>
    );
  }
  return (
    <Panel label="Ingestion">
      <Subhead>Ingestion</Subhead>
      <p className="os-store-line">{reading.detail}</p>
      <div className="os-store-bandact">
        {reading.acts.map((act) => (
          <Button
            key={act.name}
            tone={act.tone}
            busy={writes.busy === "status"}
            ariaLabel={`${act.name} for ${storeLabel(store)}`}
            onClick={() => void writes.setStatus(store.id, statusForAct(act.name))}
          >
            {act.name}
          </Button>
        ))}
      </div>
    </Panel>
  );
}

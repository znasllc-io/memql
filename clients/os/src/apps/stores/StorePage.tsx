import { useMemo } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";
import { ArrowLeft, RefreshCw, Sparkles } from "lucide-react";

import { Button, Caption, Chips, Fact, Facts, Head, Notice, Panel, Subhead } from "../../kit";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { FigureValue } from "../../cluster/FigureValue";
import { absent } from "../../cluster/figure";
import { OpenLogsButton } from "../../logs/OpenLogs";
import type { DomainState, StoreHealth } from "./health";
import {
  apiVersionMismatch,
  apiVersionMismatchSentence,
  byDriftDescending,
  domainIsQuiet,
  phaseTone,
  protectedDataWord,
  readingFor,
  statusForAct,
} from "./words";
import type { StoreWrites } from "./useStores";

// ONE STORE: what it may see, what it has mirrored, how much the live path is
// missing, and the two things that are this store's to do about it.
//
// ===========================================================================
// PER-DOMAIN BACKFILL, PAUSE, RETRY AND DISCARD DO NOT LIVE HERE. THAT IS
// DELIBERATE, AND IT IS NOT AN OMISSION TO FIX.
// ===========================================================================
// They live in the CLUSTER app's Data origins section
// (`src/apps/cluster/`), because they belong to EVERY connector: backfilling
// a domain, reconciling it, pausing it, retrying or discarding a dead letter
// are the data-origins runtime's acts, and Shopify is one origin among
// however many a cluster runs. Repeating them here would put the same three
// buttons on two pages, which is the duplication design exists to avoid --
// and the two copies would then disagree the first time one of them learned
// something.
//
// What is left here is what is THIS STORE's, and Shopify's alone:
//
//   * the STORE-WIDE pause, which is a different switch from pausing a
//     domain -- it stops ingestion for one merchant while their deliveries
//     keep being staged;
//   * re-registering webhook SUBSCRIPTIONS, because nothing generic can know
//     how often a given origin forgets its subscribers (Shopify deletes one
//     after eight consecutive delivery failures).
//
// The per-domain table below is READ-ONLY on purpose, and it says in words
// where its acts are. Do not add the buttons.

export function StorePage({
  store,
  writes,
  hideQuietDomains,
  readAt,
  onBack,
  onReread,
  onOpenSettings,
  onAsk,
}: {
  store: StoreHealth;
  writes: StoreWrites;
  /** The app setting (DESIGN.md rule 4), not in-surface chrome. */
  hideQuietDomains: boolean;
  /** When the health report landed, or null before one has. */
  readAt: Date | null;
  onBack: () => void;
  onReread: () => void;
  /** Takes the reader to the preference the domain table's empty state names. */
  onOpenSettings: () => void;
  onAsk?: (tag: string) => void;
}) {
  const reading = readingFor(store);
  const mismatch = apiVersionMismatch(store);
  const name = store.domain === "" ? store.storeId : store.domain;

  const acts: Act[] = reading.acts.map((act) => ({
    label: act.name,
    tone: act.tone,
    busy: writes.busy === "status",
    ariaLabel: `${act.name} for ${name}`,
    onAct: () => void writes.setStatus(store.storeId, statusForAct(act.name)),
  }));

  return (
    <div className="os-stores-pane">
      <div className="os-stores-scroll">
        <Panel label={`Store ${name}`}>
          {/* ONE HEAD, and no primary action in it: every act that changes
              this store's state is on the bar (rule 12). What stays is the
              way back, the way to look again, and the lines. */}
          <Head title={name}>
            <Button tone="quiet" onClick={onBack}>
              <ArrowLeft size={13} aria-hidden /> Stores
            </Button>
            <Button tone="quiet" onClick={onReread} ariaLabel={`Re-read ${name}`}>
              <RefreshCw size={13} aria-hidden /> Re-read
            </Button>
            <OpenLogsButton
              subject={store.storeId}
              subjectConcept={Concepts.SHOPIFY_STORE}
              ariaLabel={`Logs for ${name}`}
            />
            {onAsk ? (
              <Button
                tone="quiet"
                onClick={() => onAsk(`app:stores store:${store.storeId}`)}
                ariaLabel={`Ask about ${name}`}
              >
                <Sparkles size={13} aria-hidden /> Ask
              </Button>
            ) : null}
          </Head>

          <Caption>
            Shopify owns this store&rsquo;s data and MemQL holds a generated mirror of it. Webhooks
            keep the mirror current; reconciliation repairs what they lose, and the drift below is
            the measurement of how much that is.
          </Caption>

          {writes.error === "" ? null : (
            <Notice
              tone="error"
              sentence="That did not run."
              next="Nothing changed. The report below is still the last one read."
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
        </Panel>

        <ConfigurationBand store={store} mismatch={mismatch} />
        <ScopesBand store={store} />
        <SubscriptionsBand store={store} writes={writes} />
        <DomainsBand store={store} hideQuiet={hideQuietDomains} onOpenSettings={onOpenSettings} />

        {readAt === null ? null : (
          <Caption>
            Read at {readAt.toLocaleTimeString()}. A store&rsquo;s health is computed from the
            connector&rsquo;s sync state and its live client rather than carried on the row, so this
            is not a live feed -- re-read to see changes made since.
          </Caption>
        )}
      </div>

      <ActionBar state={reading.state} detail={reading.detail} tone={reading.tone} acts={acts} />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

function ConfigurationBand({ store, mismatch }: { store: StoreHealth; mismatch: boolean }) {
  return (
    <Panel label="Configuration">
      <Subhead>Configuration</Subhead>
      <Facts>
        <Fact label="Store id" value={store.storeId} mono />
        <Fact label="Status" value={store.status === "" ? "" : store.status} mono />
        <Fact label="Protected customer data" value={protectedDataWord(store)} mono />
        <Fact
          label="API version"
          value={store.apiVersion === "" ? `${store.mirrorApiVersion} (the mirror's)` : store.apiVersion}
          mono
        />
        <Fact label="Cost bucket" value={<CostBucketValue store={store} />} />
      </Facts>
      {mismatch ? <Notice tone="error" sentence={apiVersionMismatchSentence(store)} /> : null}
      <Caption>
        The cost bucket is Shopify&rsquo;s leaky bucket, as of the last Admin call that reported
        one. It is not stored, so a cluster that has made no call since it started has no reading --
        which is a different answer from a bucket at zero.
      </Caption>
    </Panel>
  );
}

/**
 * The cost bucket, or the honest absence of one.
 *
 * NEVER "0 of 0 points, restoring 0/s". A store nothing has called reports no
 * bucket at all, and coercing that to zeroes renders a store at its rate
 * limit -- the single most alarming reading this page can show -- for a store
 * that is simply idle.
 */
function CostBucketValue({ store }: { store: StoreHealth }) {
  const bucket = store.costBucket;
  if (bucket === null) {
    return (
      <>
        <FigureValue figure={absent("unmeasured", "No Admin API call has reported a cost bucket yet.")} />{" "}
        <span className="os-stores-note">not observed yet</span>
      </>
    );
  }
  return (
    <>
      <FigureValue figure={bucket.currentlyAvailable} format={round} /> of{" "}
      <FigureValue figure={bucket.maximumAvailable} format={round} /> points, restoring{" "}
      <FigureValue figure={bucket.restoreRate} format={round} suffix="/s" />
    </>
  );
}

function round(value: number): string {
  return String(Math.round(value));
}

// ---------------------------------------------------------------------------
// Scopes
// ---------------------------------------------------------------------------

function ScopesBand({ store }: { store: StoreHealth }) {
  const needed = store.scopesNeeded.length;
  return (
    <Panel label="Scopes">
      <Subhead>Scopes</Subhead>
      {store.scopesGranted.length === 0 ? (
        <Caption>
          The installation has not reported its granted scopes yet, so nothing can be checked
          against what the mirror needs. The connector needs {needed} in total.
        </Caption>
      ) : store.scopesMissing.length === 0 ? (
        <Caption>All {needed} scopes the mirror needs are granted.</Caption>
      ) : (
        <>
          <p className="os-stores-line">
            {store.scopesMissing.length} of the {needed} scopes the mirror needs are not granted.
            Shopify returns null for the fields they cover, so the domains below them are quietly
            INCOMPLETE rather than broken -- nothing errors, and the missing values look exactly
            like values the merchant never entered.
          </p>
          <Chips label="Scopes the mirror needs and does not have">
            {store.scopesMissing.map((scope) => (
              <span key={scope} className="os-stores-status" data-tone="warn">
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

function SubscriptionsBand({ store, writes }: { store: StoreHealth; writes: StoreWrites }) {
  const record = store.subscriptions;
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
          <p className="os-stores-line">
            <FigureValue figure={record.existing} /> registered of{" "}
            <FigureValue figure={record.desired} /> the mirror wants, last checked{" "}
            {record.at === "" ? "at an unrecorded time" : new Date(record.at).toLocaleString()}.
          </p>
          {record.failed.length === 0 ? null : (
            <>
              <p className="os-stores-line" data-tone="danger">
                {record.failed.length} topic{record.failed.length === 1 ? "" : "s"} could not be
                registered:
              </p>
              <Chips label="Topics that could not be registered">
                {record.failed.slice(0, 12).map((topic) => (
                  <span key={topic} className="os-stores-status" data-tone="warn">
                    {topic}
                  </span>
                ))}
              </Chips>
            </>
          )}
        </>
      )}
      {/* THE ACT NAMES ITS OWN SCOPE, because it is wider than this page.
          `shopifyEnsureSubscriptions` takes NO store argument -- it walks
          every ingesting store -- so a button here that read as "reconcile
          this store" would be a claim the builtin cannot keep. It stays on
          this band rather than on the bar because it is not a lifecycle act:
          it changes no store's status, and the bar's three slots belong to
          the acts that do. */}
      <div className="os-stores-bandact">
        <Button
          tone="quiet"
          busy={writes.busy === "subscriptions"}
          busyLabel="Reconciling"
          onClick={() => void writes.ensureSubscriptions()}
        >
          <RefreshCw size={13} aria-hidden /> Reconcile now
        </Button>
        <Caption>
          Reconciles every ingesting store, not only this one -- the builtin takes no store. It runs
          on its own at boot and daily at 03:15.
        </Caption>
      </div>
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Per-domain sync state
// ---------------------------------------------------------------------------

function DomainsBand({
  store,
  hideQuiet,
  onOpenSettings,
}: {
  store: StoreHealth;
  hideQuiet: boolean;
  onOpenSettings: () => void;
}) {
  const shown = useMemo(() => {
    const kept = hideQuiet ? store.domains.filter((d) => !domainIsQuiet(d)) : store.domains;
    return [...kept].sort(byDriftDescending);
  }, [store.domains, hideQuiet]);

  return (
    <Panel label="Mirror sync state">
      <Subhead>Mirror sync state</Subhead>
      {/* THE SCOPE IS THE CONNECTOR'S, AND SAYING SO IS NOT A DETAIL.
          `v1:platform:syncState` is keyed by (concept, connector) with no
          store in the key, and the health handler hands the same slice to
          every store in its report -- so a cluster mirroring two shops shows
          both of them the identical table. Captioned "per-domain sync state
          for THIS store", it would be a number under the wrong subject. */}
      <Caption>
        One row per mirrored concept, for the shopify connector as a whole. These rows are keyed by
        concept and connector rather than by store, so every store this cluster mirrors reports the
        same table.
      </Caption>
      {store.domains.length === 0 ? (
        <Caption>
          No domain has synced yet, so there is nothing to measure. A backfill is what starts one,
          and it lives in Cluster &rarr; Data origins.
        </Caption>
      ) : shown.length === 0 ? (
        <Caption>
          Every mirrored domain is quiet -- idle, no drift and nothing waiting in the outbox -- and
          quiet domains are hidden by{" "}
          <button type="button" className="os-sort" onClick={onOpenSettings}>
            a setting in this app
          </button>
          .
        </Caption>
      ) : (
        <div className="os-stores-tablewrap">
          <table className="os-stores-table">
            <thead>
              {/* EVERY COLUMN IS NAMED FOR WHAT IT CARRIES, and for two of
                  them that used to require disagreeing with the report. It
                  sent `staleWrites` carrying syncState's `lagSeconds` and
                  `tombstoned` carrying its `outboxDepth`, so drawing the keys
                  would have put a latency and a queue depth under two names
                  meaning something else. Epic memql#5009 repaired the names
                  in the Go handler instead -- this surface was the only
                  consumer -- so the columns and the wire agree now, and this
                  note is here so nobody "restores" the old spellings. */}
              <tr>
                <th scope="col">Domain</th>
                <th scope="col">Phase</th>
                <th scope="col" title="Rows the last reconcile found disagreeing with the origin. Repeated drift is a webhook that is not arriving.">
                  Drift
                </th>
                <th scope="col" title="Seconds between the origin's version of the last applied write and when MemQL applied it -- the mirror's staleness.">
                  Lag
                </th>
                <th scope="col" title="Pending and failed outbox entries for this concept: whether the drain is keeping up.">
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
        &rarr; Data origins. What is here is the store-wide pause on the bar below, and the
        subscription reconcile above.
      </Caption>
    </Panel>
  );
}

function DomainRow({ domain }: { domain: DomainState }) {
  return (
    <tr>
      <td className="os-mono">{domain.concept}</td>
      <td>
        <span className="os-stores-status" data-tone={phaseTone(domain.phase)}>
          {domain.phase === "" ? "idle" : domain.phase}
        </span>
      </td>
      <td>
        <FigureValue figure={domain.driftLast} />
      </td>
      <td>
        <FigureValue figure={domain.lagSeconds} suffix="s" />
      </td>
      <td>
        <FigureValue figure={domain.outboxDepth} />
      </td>
      <td className="os-stores-note">
        {domain.lastAppliedAt === "" ? (
          <FigureValue figure={absent("unmeasured", "Live delivery has applied nothing for this domain yet.")} />
        ) : (
          new Date(domain.lastAppliedAt).toLocaleString()
        )}
      </td>
    </tr>
  );
}

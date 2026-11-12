import { useEffect, useState } from "react";

import { AutoRefresh, Caption, Head, Notice, useNow } from "../../../kit";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { formatFreshness } from "../../../kit/format";
import { bucketLabel } from "../../../kit/statistics/period";
import { useOsConnection } from "../../../live/connection";
import type { SiteRow } from "../rows";
import { useDeployablesSettings } from "../settingsContext";
import { StatisticsPeriodControl } from "../statistics/SiteStatisticsView";
import { statisticsRefreshMs } from "../statistics/useSiteStatistics";
import { DestinationControl, type DestinationChoice } from "./DestinationControl";
import { destinationsOf, type StoreStatistics } from "./read";
import { useStoreStatistics } from "./useStoreStatistics";

// A STOREFRONT'S STORE STATISTICS (the 2026-11-11 store statistics design):
// the store's sales, beside the website's Statistics. It takes the pane the
// way Reviews does, under the deployable's trail, and draws in Statistics'
// manner: the Head with its quiet controls at the right, then the store's
// context in captions. The figures, the chart and the lists come after it.
//
// THE PAGE OPENS ON PRODUCTION when Production has a store, else on Testing,
// and the choice is not remembered: whichever store a person looked at last
// is not a reason to show it next time. The Destination control is drawn only
// when the two destinations name two stores; the engine says when they name
// one ("both"), because only it compares the two forms of an id.
//
// THE CONTEXT SAYS ONLY WHAT THE ANSWER HOLDS. A store the walk holds no row
// for yet has no currency, no zone and no period: nothing of those is said,
// and no figure is drawn as zero. Days are the store's when the answer names
// its zone, and UTC's otherwise, because that is how the engine cut them.

/** The line about orders placed past what the app may read (record A22). */
export const OLDER_ORDERS_LINE = "Refunds, edits and cancellations of orders placed more than 60 days ago are not recorded.";
export const DEVELOPMENT_LINE = "This is a development store: every order on it is a test order, and they are counted.";
export const TAX_LINE = "Prices on this store include tax.";
/** A store with no row yet: its orders are still being read in. */
export const BROUGHT_IN_LINE = "The store's orders are still being brought in.";

export interface StoreStatisticsSectionProps {
  site: SiteRow;
  /** The deployable's name, as its page's trail says it. */
  name: string;
  /** The trail down to the deployable: Store statistics is added after it. */
  trail: readonly Breadcrumb[];
  /** Back out of Store statistics, to the deployable. */
  back: { label: string; onSelect: () => void };
}

export function StoreStatisticsSection({ site, name, trail, back }: StoreStatisticsSectionProps) {
  const connection = useOsConnection();
  const { settings } = useDeployablesSettings();
  const period = settings.statisticsPeriod;
  const destinations = destinationsOf(site);
  const [chosen, setDestination] = useState<DestinationChoice>(destinations.production !== "" ? "production" : "testing");
  // A destination whose store was unbound while the page stood reads the other.
  const destination: DestinationChoice = destinations[chosen] !== "" ? chosen : chosen === "production" ? "testing" : "production";
  const storeId = destinations[destination];
  const reading = useStoreStatistics({ siteId: site.id, storeId, period, enabled: true });
  const stats = reading.value;

  // WHETHER THE TWO DESTINATIONS NAME ONE STORE is the last answer's word,
  // held across a change of destination or period so the control does not
  // leave the Head while the next read is out.
  const [oneStore, setOneStore] = useState<boolean | null>(null);
  useEffect(() => {
    if (stats !== null && stats.destination !== null) setOneStore(stats.destination === "both");
  }, [stats]);
  const offerDestination = destinations.production !== "" && destinations.testing !== "" && oneStore === false;

  const pending = stats === null && (reading.state === "idle" || reading.state === "loading");

  return (
    <section
      className="os-action-pane"
      aria-label={`Store statistics for ${site.hostname}`}
      data-os-dividers="none"
      data-os-page-context={JSON.stringify({ page: "Store statistics", siteId: site.id, hostname: site.hostname, name, destination, storeId, period })}
    >
      <div className="os-action-body os-app-stack">
        <Head navigationKey={`storeStatistics:${site.id}`} title="Store statistics" breadcrumbs={[...trail, { label: "Store statistics" }]} back={back}>
          {offerDestination ? <DestinationControl value={destination} onChange={setDestination} /> : null}
          <StatisticsPeriodControl />
        </Head>
        <AutoRefresh onRefresh={reading.reload} busy={reading.state === "loading"} enabled={connection !== null} intervalMs={statisticsRefreshMs(period)} />
        {reading.state === "error" ? (
          // VERBATIM: the engine's own sentence, kept while the read is tried again.
          <Notice tone="error" sentence={reading.error || "Store statistics could not be loaded."} next="Retrying automatically." />
        ) : null}
        {pending ? <Caption><InlineSkeleton label="Loading the store" /></Caption> : stats !== null ? <StoreContext stats={stats} /> : null}
      </div>
    </section>
  );
}

/** Block 1: which store, in what currency and days, since when, how fresh; then each line that applies. */
function StoreContext({ stats }: { stats: StoreStatistics }) {
  const now = useNow(60_000);
  const zone = stats.timezone ?? undefined;
  const line = [
    stats.storeName ?? stats.storeDomain,
    stats.destination === "both" ? "Production and Testing" : null,
    stats.currency !== null ? `Prices in ${stats.currency}` : null,
    // No period was cut for a store with no row, so no days are named.
    stats.start !== null ? `Days in ${stats.timezone ?? "UTC"}` : null,
    stats.historySince !== null ? `Orders since ${bucketLabel(stats.historySince, "day", zone)}` : null,
    stats.lastChangeAt !== null ? `Last order change recorded ${formatFreshness(stats.lastChangeAt, now)}` : null,
  ].filter((part): part is string => part !== null && part !== "");
  return (
    <>
      {stats.start === null ? <Notice sentence={BROUGHT_IN_LINE} /> : null}
      {line.length > 0 ? <Caption>{line.join(" · ")}</Caption> : null}
      {stats.development === true ? <Caption>{DEVELOPMENT_LINE}</Caption> : null}
      {stats.taxesIncluded === true ? <Caption>{TAX_LINE}</Caption> : null}
      {stats.olderOrderChangesNotRecorded === true ? <Caption>{OLDER_ORDERS_LINE}</Caption> : null}
    </>
  );
}

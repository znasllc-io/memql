import { Caption } from "../../kit";
import {
  formatFigure,
  formatRate,
  rateOf,
  sendBreakdown,
  sendBreakdownLabel,
  type CampaignRow,
  type CampaignStats,
} from "./rows";

// THE SEND BAR: one audience, divided by what happened to it.
//
// ===========================================================================
// WHY THIS IS A BAND AND NOT A ROW OF STAT CARDS
// ===========================================================================
// Four numbers in four boxes is the templated answer, and it makes a person do
// arithmetic to learn the one thing they came for: how far through is this,
// and did it go well. A band partitioned proportionally IS that answer. It
// also makes the SKIPPED slice visible -- the compliance-relevant figure
// nobody goes looking for, because nobody thinks to ask "how many of my list
// can I no longer mail" until it is most of them.
//
// It reads as one object because it IS one object: the audience, divided. The
// segments always sum to the whole (pending is derived, never read), so the
// band can never show a gap that looks like a rendering fault.
//
// PLAIN CSS, NO CHART LIBRARY. This is four proportions in a row; a charting
// dependency would be the largest thing in the app in exchange for a flex
// container. The Deployables map made the same call for the same reason.
//
// IT IS READABLE WITHOUT EYES. `role="img"` plus an `aria-label` stating the
// figures in words -- a bar a screen reader cannot read is a bar that excluded
// somebody, and the picture's whole content is proportion, which the legend
// beneath does not convey on its own.

/** How thin a slice may get before it stops being drawn to scale.
 *
 *  A single failure in a list of ten thousand is a real fact and a
 *  zero-width sliver is not a way to say it, so a non-empty segment is floored
 *  at a hairline. It costs the band a fraction of a percent of accuracy and
 *  buys the difference between "none" and "one", which is the difference
 *  somebody is actually looking for. */
const MIN_VISIBLE_SHARE = 0.012;

export function SendBar({
  campaign,
  stats,
}: {
  campaign: CampaignRow;
  /** The server-computed breakdown, when it has been read. Null is normal --
   *  the band is drawn from the campaign row alone, which is what lets it fill
   *  live during a send. */
  stats: CampaignStats | null;
}) {
  const breakdown = sendBreakdown(campaign);

  if (breakdown.empty) {
    return (
      <div className="os-send">
        <div className="os-send-bar" data-empty role="img" aria-label={sendBreakdownLabel(breakdown)} />
        <Caption>
          Nothing has been sent yet. The bar fills in as the send works through the audience.
        </Caption>
      </div>
    );
  }

  return (
    <div className="os-send">
      {/* THE BAND. `flex-grow` on each segment rather than a percentage width,
          so rounding cannot leave a one-pixel gap at the end -- the slices
          divide the container rather than each claiming a share of it. */}
      <div className="os-send-bar" role="img" aria-label={sendBreakdownLabel(breakdown)}>
        {breakdown.segments
          .filter((segment) => segment.count > 0)
          .map((segment) => (
            <span
              key={segment.key}
              className="os-send-seg"
              data-outcome={segment.key}
              style={{ flexGrow: Math.max(segment.share, MIN_VISIBLE_SHARE) }}
            />
          ))}
      </div>

      {/* THE LEGEND CARRIES THE EXACT FIGURES, because a proportion is not a
          number and an operator reporting on a send needs both. Every slice
          appears, including the ones at zero: "no failures" is a reading
          somebody wants, and an omitted row is silence about it. */}
      <ul className="os-send-legend" aria-label="What happened to this audience">
        {breakdown.segments.map((segment) => (
          <li key={segment.key} className="os-send-legend-item">
            <span className="os-send-swatch" data-outcome={segment.key} aria-hidden />
            <span className="os-send-legend-label">{segment.label}</span>
            <span className="os-send-legend-count">{segment.count}</span>
          </li>
        ))}
      </ul>

      <Engagement campaign={campaign} stats={stats} sent={sentCount(breakdown)} />
    </div>
  );
}

function sentCount(breakdown: ReturnType<typeof sendBreakdown>): number {
  return breakdown.segments.find((s) => s.key === "sent")?.count ?? 0;
}

/**
 * Opens and clicks, BELOW the bar and smaller than it.
 *
 * ENGAGEMENT IS A DIFFERENT QUESTION, about the sent slice only, so it is not
 * a fifth segment. A fifth segment would compete with the send outcome for the
 * same width while measuring a different denominator -- opens are a share of
 * what was DELIVERED, not of the audience -- and a band whose slices mean two
 * different things is a band that cannot be read at all.
 *
 * The rate leads because it is the comparable figure; the raw unique-of-total
 * pair follows because a rate with no counts behind it cannot be checked.
 *
 * ABSENT IS AN EM DASH WITH A REASON, NEVER A ZERO. `campaignStats` reports a
 * unique count as unmeasured when the fold hit its bound, and reports no soft
 * bounce figure at all. A zero there would be this window inventing a fact --
 * and a zero open rate is a thing operators act on.
 */
function Engagement({
  campaign,
  stats,
  sent,
}: {
  campaign: CampaignRow;
  stats: CampaignStats | null;
  sent: number;
}) {
  // TRACKING OFF IS NOT ZERO ENGAGEMENT. A campaign that never asked to be
  // measured has no figure, and saying so is the only honest reading -- "0%
  // opened" on an untracked send is a lie about the recipients.
  if (!campaign.trackOpens && !campaign.trackClicks) {
    return <Caption>Opens and clicks were not tracked for this campaign.</Caption>;
  }

  if (stats === null) {
    return <Caption>Opens and clicks arrive with the full breakdown below.</Caption>;
  }

  return (
    <div className="os-send-engagement">
      {campaign.trackOpens ? (
        <EngagementFigure
          label="Opened"
          rate={formatRate(rateOf(stats.opensUnique, sent))}
          unique={formatFigure(stats.opensUnique)}
          total={formatFigure(stats.opensTotal)}
          note={stats.opensUnique.absentBecause}
        />
      ) : null}
      {campaign.trackClicks ? (
        <EngagementFigure
          label="Clicked"
          rate={formatRate(rateOf(stats.clicksUnique, sent))}
          unique={formatFigure(stats.clicksUnique)}
          total={formatFigure(stats.clicksTotal)}
          note={stats.clicksUnique.absentBecause}
        />
      ) : null}
    </div>
  );
}

function EngagementFigure({
  label,
  rate,
  unique,
  total,
  note,
}: {
  label: string;
  rate: string;
  unique: string;
  total: string;
  note: string;
}) {
  return (
    <div className="os-send-engagement-item">
      <span className="os-send-engagement-rate">{rate}</span>
      <span className="os-send-engagement-label">{label}</span>
      <span className="os-caption">
        {unique} of the people who got it, {total} times in all
      </span>
      {note === "" ? null : <span className="os-caption os-send-engagement-note">{note}</span>}
    </div>
  );
}

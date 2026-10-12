import { Caption } from "../../../kit";
import { sendBreakdown, sendBreakdownLabel, type CampaignRow } from "../rows";

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
//
// IT IS FED BY THE CAMPAIGN'S OWN ROW, whose counters arrive live, so it fills
// under somebody watching a send without anything here reading again. What the
// people who got it did with it -- opens and clicks -- is a different question
// about a different denominator, and is drawn beneath it (`Results.tsx`).

/** How thin a slice may get before it stops being drawn to scale.
 *
 *  A single failure in a list of ten thousand is a real fact and a
 *  zero-width sliver is not a way to say it, so a non-empty segment is floored
 *  at a hairline. It costs the band a fraction of a percent of accuracy and
 *  buys the difference between "none" and "one", which is the difference
 *  somebody is actually looking for. */
const MIN_VISIBLE_SHARE = 0.012;

export function SendBar({ campaign }: { campaign: CampaignRow }) {
  const breakdown = sendBreakdown(campaign);

  if (breakdown.empty) {
    return (
      <div className="os-send">
        <div className="os-send-bar" data-empty role="img" aria-label={sendBreakdownLabel(breakdown)} />
        <Caption>Nothing has been sent yet.</Caption>
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
            <span className="os-send-legend-count">{segment.count.toLocaleString("en-US")}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

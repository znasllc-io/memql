import { Caption } from "../../kit";
import { kindBreakdownLabel, type KindBreakdown } from "./rows";

// THE KIND BAND: one run's steps, divided by what each one cost.
//
// ===========================================================================
// WHY A BAND AND NOT SIX FIGURES
// ===========================================================================
// The one thing a person opens a run to learn is how much of it had to think.
// Six numbers in six boxes makes them work that out; a band partitioned
// proportionally IS the answer, and it is legible before a word of it is read.
// That argument is the campaigns send bar's, and this is the second use of it
// rather than a second invention -- the mechanism, the minimum-share floor and
// the spoken label are all that file's, applied to a different partition.
//
// It also makes two slices visible that nobody goes looking for: the HUMAN
// steps, which are the ones that will park the run and wait for somebody, and
// the UNCLASSIFIED ones, which epic A1 leaves empty for `function` steps and
// which this build cannot vouch for either way.
//
// ===========================================================================
// THE CONTRAST IS DRAWN IN INK, NOT IN HUE
// ===========================================================================
// The obvious move is a colour per kind. It is the wrong one here twice over.
// Amber is `warn` and red is `error` everywhere in this shell and the accent
// is "live / primary / yes-here" -- so a six-colour legend would put status
// hues on a partition that has nothing to do with status, and a reasoning step
// drawn in accent would read as "this step is fine".
//
// So the axis is WEIGHT: reasoning is solid ink, deterministic is the rail
// tint, and the rest sit between them. That survives greyscale, it survives
// every theme pack (a pack carries colour, and this partition does not use
// colour to mean anything), and it says the true thing -- most of a run is
// cheap machine motion and a little of it is expensive thought.
//
// PLAIN CSS, NO CHART LIBRARY. Six proportions in a row; a charting dependency
// would be the largest thing in the app in exchange for a flex container.

/**
 * How thin a slice may get before it stops being drawn to scale.
 *
 * One reasoning step in a run of two hundred is the most important fact on
 * the page and a zero-width sliver is not a way to say it, so a non-empty
 * segment is floored at a hairline. It costs the band a fraction of a percent
 * of accuracy and buys the difference between "none" and "one".
 */
const MIN_VISIBLE_SHARE = 0.012;

export function KindBand({ breakdown }: { breakdown: KindBreakdown }) {
  if (breakdown.empty) {
    return (
      <div className="os-nexus-band">
        <div className="os-nexus-band-bar" data-empty role="img" aria-label={kindBreakdownLabel(breakdown)} />
        <Caption>
          No steps yet. The band fills in as the run works out what it has to do.
        </Caption>
      </div>
    );
  }

  return (
    <div className="os-nexus-band">
      {/* `flex-grow` per segment rather than a percentage width, so rounding
          cannot leave a one-pixel gap at the end: the slices divide the
          container rather than each claiming a share of it. */}
      <div className="os-nexus-band-bar" role="img" aria-label={kindBreakdownLabel(breakdown)}>
        {breakdown.segments
          .filter((segment) => segment.count > 0)
          .map((segment) => (
            <span
              key={segment.kind === "" ? "unclassified" : segment.kind}
              className="os-nexus-band-seg"
              data-kind={segment.kind === "" ? "unclassified" : segment.kind}
              style={{ flexGrow: Math.max(segment.share, MIN_VISIBLE_SHARE) }}
            />
          ))}
      </div>

      {/* THE LEGEND CARRIES THE EXACT FIGURES, because a proportion is not a
          number. Every slice appears, including the ones at zero: "no steps
          are waiting on a person" is a reading somebody wants, and an omitted
          row is silence about it. */}
      <ul className="os-nexus-band-legend" aria-label="What this run's steps are made of">
        {breakdown.segments.map((segment) => (
          <li key={segment.kind === "" ? "unclassified" : segment.kind} className="os-nexus-band-item">
            <span
              className="os-nexus-band-swatch"
              data-kind={segment.kind === "" ? "unclassified" : segment.kind}
              aria-hidden
            />
            <span className="os-nexus-band-label">{segment.label}</span>
            <span className="os-nexus-band-count">{segment.count}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

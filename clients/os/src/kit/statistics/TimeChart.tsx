import { useId, useRef, useState, type KeyboardEvent, type PointerEvent } from "react";

import { useWidth } from "../useWide";
import { bucketLabel, bucketMoment, type Bucket } from "./period";
import { niceMax, niceMin, timeTickIndexes, valueTicks } from "./scale";
import "./statistics.css";

// ONE TIME-SERIES CHART FOR EVERY STATISTICS PAGE (spec 8).
//
// The kit had no chart, so Traffic's strip and Email's send band each drew
// bars of their own, in two looks. This is the one drawing both pages, and the
// Overviews, are built from, so what it promises it promises everywhere:
//
//   - A BUCKET NOTHING MEASURED IS A GAP, NEVER A ZERO. A null ends the line
//     and the next measured bucket starts a new one (its own `M`), and the
//     soft fill under the line breaks with it. A website whose visitors were
//     not counted before this shipped reads as "no line yet", not as a month
//     of nobody (kit/measure says why the two must never look alike).
//   - THE PREVIOUS PERIOD IS A FAINT DASHED LINE at the same offsets, under
//     the current one and on the same value axis, so a larger previous period
//     still fits. Twelve months has none (spec D4), and then nothing is drawn
//     and the legend that would explain a dashed line is gone too.
//   - ONE READOUT. Hover, a tap, focus and the arrow keys move it: the
//     bucket, its value (or "Not measured"), and the previous period's value
//     beside it. It sits ABOVE the plot rather than floating over it, so it
//     never covers the data it describes, and it keeps its line while empty
//     so nothing moves when it fills. It names an hour with its day
//     (`bucketMoment`), as the summary does; the axis says "15:00" because its
//     neighbours say which day, and a readout has no neighbours.
//   - THE WHOLE SERIES IN ONE SENTENCE for a screen reader (the figcaption),
//     and the arrow keys for reading it point by point. The plot is an
//     `application` the way kit/MapCanvas is, so the arrow keys reach it with
//     a screen reader running instead of moving the reader's own cursor.
//
// STRETCHED, NOT SCALED. The SVG is drawn in a fixed 600-unit-wide
// coordinate space and stretched to the plot's width
// (`preserveAspectRatio="none"`); every stroke opts out of the stretch
// (`vector-effect: non-scaling-stroke`), so a line is 2px on a phone and on a
// wall display alike. What a stretch would distort -- text and round dots --
// is HTML laid over the drawing, positioned in percentages of it.
//
// BELOW ZERO, BELOW THE ZERO RULE. A figure that can be below zero -- a net
// figure a day of refunds outweighed -- scales from a round floor (`niceMin`)
// to the round top; its fill runs to where zero falls rather than to the
// plot's bottom, so a day below zero hangs beneath the zero rule, and that
// rule is marked (`data-zero`). With nothing below zero the floor is zero and
// the drawing is the one it always was.
//
// A STORE'S OWN DAYS. A page that reads a store passes its `timeZone`, and
// every bucket -- the axis, the readout and the summary -- is named as the
// store's clock showed it (`bucketLabel`). Without one, UTC.
//
// Colours are theme tokens (statistics.css), so every built-in theme in light
// and dark draws it without a rule of its own, and nothing moves under
// reduced motion.

/** One bucket of a series. A null value is a bucket nothing measured: a gap, never a zero. */
export interface ChartPoint {
  at: string;
  value: number | null;
}

/** The drawing's own coordinate width, stretched to the plot's real width. */
const VIEW_WIDTH = 600;
const DEFAULT_HEIGHT = 160;
/** The CSS pixels a time-axis label is owed: how many labels fit is the plot's width over this. */
const TIME_LABEL_ROOM = 72;

/** A run of measured buckets, first to last inclusive: one stroke of the line. */
interface Run {
  from: number;
  to: number;
}

/** A point's value when it is a number that can be drawn; anything else is a gap. */
function valueOf(point: ChartPoint | undefined): number | null {
  const value = point?.value;
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function runsOf(values: readonly (number | null)[]): Run[] {
  const runs: Run[] = [];
  let from = -1;
  values.forEach((value, index) => {
    if (value === null) {
      if (from >= 0) runs.push({ from, to: index - 1 });
      from = -1;
    } else if (from < 0) {
      from = index;
    }
  });
  if (from >= 0) runs.push({ from, to: values.length - 1 });
  return runs;
}

/** Two decimals is far finer than a pixel at any width, and keeps `d` short. */
function coordinate(value: number): number {
  return Math.round(value * 100) / 100;
}

/**
 * The line: one `M` per run of two or more buckets, then an `L` per bucket
 * after the first. A lone bucket has no stroke to draw; it is a dot (below).
 */
function linePath(values: readonly (number | null)[], runs: readonly Run[], x: (index: number) => number, y: (value: number) => number): string {
  return runs
    .filter((run) => run.to > run.from)
    .map((run) => {
      const steps: string[] = [];
      for (let index = run.from; index <= run.to; index += 1) {
        steps.push(`${index === run.from ? "M" : "L"}${coordinate(x(index))} ${coordinate(y(values[index] ?? 0))}`);
      }
      return steps.join(" ");
    })
    .join(" ");
}

/** The fill under each run of two or more buckets, down to the baseline; a lone bucket has no area. */
function areaPath(values: readonly (number | null)[], runs: readonly Run[], x: (index: number) => number, y: (value: number) => number, baseline: number): string {
  return runs
    .filter((run) => run.to > run.from)
    .map((run) => {
      const steps = [`M${coordinate(x(run.from))} ${baseline}`];
      for (let index = run.from; index <= run.to; index += 1) {
        steps.push(`L${coordinate(x(index))} ${coordinate(y(values[index] ?? 0))}`);
      }
      steps.push(`L${coordinate(x(run.to))} ${baseline}`, "Z");
      return steps.join(" ");
    })
    .join(" ");
}

/**
 * The series in one sentence: "Visitors: 30 points from Oct 10 to Nov 8;
 * highest 1,204 on Nov 6; 3 not measured." The highest is left out when
 * nothing was measured, and the unmeasured when everything was. An hour is
 * named with its day ("from Nov 7, 15:00 to Nov 8, 15:00").
 */
function summaryOf(label: string, bucket: Bucket, points: readonly ChartPoint[], values: readonly (number | null)[], format: (value: number) => string, timeZone: string | undefined): string {
  const count = points.length;
  if (count === 0) return `${label}: no points.`;
  const first = bucketMoment(points[0]?.at ?? "", bucket, timeZone);
  const last = bucketMoment(points[count - 1]?.at ?? "", bucket, timeZone);
  const parts = [count === 1 ? `${label}: 1 point on ${first}` : `${label}: ${count} points from ${first} to ${last}`];
  let highest: number | null = null;
  let highestAt = -1;
  let unmeasured = 0;
  for (let index = 0; index < values.length; index += 1) {
    const value = values[index] ?? null;
    if (value === null) unmeasured += 1;
    else if (highest === null || value > highest) {
      highest = value;
      highestAt = index;
    }
  }
  if (highest !== null) parts.push(`highest ${format(highest)} on ${bucketMoment(points[highestAt]?.at ?? "", bucket, timeZone)}`);
  if (unmeasured > 0) parts.push(`${unmeasured} not measured`);
  return `${parts.join("; ")}.`;
}

/** A series' key: a short stroke in the line's own style, never a box. */
function Swatch({ series }: { series: "current" | "previous" }) {
  return <span className="os-time-chart-swatch" data-series={series} aria-hidden="true" />;
}

/**
 * The chart's place while its read is on its way: the same figure, head,
 * plot and time axis a TimeChart draws, with a quiet shape where the line
 * will be, so nothing below moves when the chart lands. Loading is shapes,
 * never words: the label is announced, not painted.
 */
export function TimeChartSkeleton({ label, height = DEFAULT_HEIGHT }: {
  /** What is on its way ("Loading visitors over time"), for assistive technology. */
  label: string;
  /** The plot's height, as the TimeChart it stands for will be given. */
  height?: number;
}) {
  return (
    <figure className="os-time-chart" role="status" aria-busy="true">
      <span className="os-sr-only">{label}</span>
      <div className="os-time-chart-head" aria-hidden="true" />
      <div className="os-time-chart-body" aria-hidden="true">
        <div className="os-time-chart-values" style={{ height }} />
        <div className="os-time-chart-plot" style={{ height }}>
          <span className="os-skeleton-block os-time-chart-skeleton" />
        </div>
        <div className="os-time-chart-times" />
      </div>
    </figure>
  );
}

export function TimeChart({ label, bucket, points, previous, format, timeZone, height = DEFAULT_HEIGHT }: {
  /** What is drawn ("Visitors"). Names the plot, and starts its sentence. */
  label: string;
  bucket: Bucket;
  points: readonly ChartPoint[];
  /** The previous period, bucket for bucket. Absent when there is none (twelve months). */
  previous?: readonly ChartPoint[];
  /** How a value is written, on the value axis and in the readout. */
  format: (value: number) => string;
  /** The store's time zone ("Europe/Madrid"), its buckets are named in. Absent, UTC. */
  timeZone?: string;
  /** The plot's height in CSS pixels, and the drawing's. */
  height?: number;
}) {
  const plotRef = useRef<HTMLDivElement | null>(null);
  const width = useWidth(plotRef);
  const [active, setActive] = useState<number | null>(null);
  const focused = useRef(false);
  const captionId = useId();

  const count = points.length;
  const current = points.map(valueOf);
  // The previous period is drawn at the SAME OFFSETS: its first bucket under
  // this period's first. Buckets past this period's last have nowhere to go.
  const prior = previous === undefined ? null : Array.from({ length: count }, (_, index) => valueOf(previous[index]));
  const measured = [...current, ...(prior ?? [])].filter((value): value is number => value !== null);
  const largest = Math.max(0, ...measured);
  const smallest = Math.min(0, ...measured);
  const top = niceMax(largest);
  const floor = niceMin(smallest);
  const span = top - floor;

  const x = (index: number) => (count <= 1 ? VIEW_WIDTH / 2 : (index / (count - 1)) * VIEW_WIDTH);
  const y = (value: number) => height - ((value - floor) / span) * height;
  const leftOf = (index: number) => `${count <= 1 ? 50 : (index / (count - 1)) * 100}%`;
  const topOf = (value: number) => `${(1 - (value - floor) / span) * 100}%`;
  // Where zero falls: the plot's bottom, unless something is below zero.
  const zero = coordinate(y(0));

  const runs = runsOf(current);
  const line = linePath(current, runs, x, y);
  const area = areaPath(current, runs, x, y, zero);
  const priorRuns = prior === null ? [] : runsOf(prior);
  const priorLine = prior === null ? "" : linePath(prior, priorRuns, x, y);
  // A measured bucket with a gap either side is a run of one, which a line
  // cannot show: it gets a dot of its own, in either period.
  const lone = runs.filter((run) => run.from === run.to).map((run) => run.from);
  const priorLone = priorRuns.filter((run) => run.from === run.to).map((run) => run.from);
  const priorDrawn = priorLine !== "" || priorLone.length > 0;
  const timeTicks = timeTickIndexes(count, Math.floor((width ?? VIEW_WIDTH) / TIME_LABEL_ROOM));

  // THE VALUE AXIS'S STEPS, labelled in the gutter and ruled across the plot.
  // No scale is offered for nothing (0, 0.5 and 1 over an empty plot would be
  // a claim about values nobody measured), so then only the baseline is
  // ruled. A series of whole numbers drops a fractional middle step
  // (`valueTicks`). And while the current period says "Not measured" in the
  // middle of the plot, the middle step gives way, so no rule crosses it.
  // Below zero the floor is a step too, under zero.
  const steps = measured.length === 0 ? [] : valueTicks(largest, measured.every(Number.isInteger), smallest);
  const ticks = runs.length === 0 ? steps.filter((tick) => tick === floor || tick === 0 || tick === top) : steps;
  const rules = ticks.length === 0 ? [{ at: height, zero: false }] : ticks.map((tick) => ({ at: coordinate(y(tick)), zero: floor < 0 && tick === 0 }));

  // Clamped at render: a shorter series arriving under a held index reads its
  // last bucket rather than nothing.
  const at = active === null || count === 0 ? null : Math.min(active, count - 1);
  const now = at === null ? null : (current[at] ?? null);
  const then = at === null || prior === null ? null : (prior[at] ?? null);

  function indexAt(clientX: number): number | null {
    const plot = plotRef.current;
    if (plot === null || count === 0) return null;
    const rect = plot.getBoundingClientRect();
    if (!(rect.width > 0)) return null;
    const index = Math.round(((clientX - rect.left) / rect.width) * (count - 1));
    return Math.min(count - 1, Math.max(0, index));
  }

  function onPointer(event: PointerEvent<HTMLDivElement>): void {
    const index = indexAt(event.clientX);
    if (index !== null) setActive(index);
  }

  function onKeyDown(event: KeyboardEvent<HTMLDivElement>): void {
    if (count === 0) return;
    const from = at ?? count - 1;
    const next =
      event.key === "ArrowLeft" ? Math.max(0, from - 1)
      : event.key === "ArrowRight" ? Math.min(count - 1, from + 1)
      : event.key === "Home" ? 0
      : event.key === "End" ? count - 1
      : null;
    if (next === null) return;
    event.preventDefault();
    setActive(next);
  }

  return (
    <figure className="os-time-chart">
      <div className="os-time-chart-head">
        <p className="os-time-chart-readout" role="status">
          {at === null ? null : (
            <>
              <span className="os-time-chart-when">{bucketMoment(points[at]?.at ?? "", bucket, timeZone)}</span>
              <span className="os-sr-only">: </span>
              <span className="os-time-chart-now">
                <Swatch series="current" />
                {now === null ? "Not measured" : <strong>{format(now)}</strong>}
              </span>
              {prior === null ? null : (
                <>
                  <span className="os-sr-only">; </span>
                  <span className="os-time-chart-then">
                    <Swatch series="previous" />
                    Previous {then === null ? "not measured" : <strong>{format(then)}</strong>}
                  </span>
                </>
              )}
            </>
          )}
        </p>
        {/* Two series need saying once which is which; the readout carries the
            same keys while it is showing a bucket, so the legend gives way.
            A previous period with nothing drawn has nothing to explain. */}
        {priorDrawn && at === null ? (
          <p className="os-time-chart-legend" aria-hidden="true">
            <span><Swatch series="current" />{label}</span>
            <span><Swatch series="previous" />Previous period</span>
          </p>
        ) : null}
      </div>
      <div className="os-time-chart-body">
        {/* Steps from zero up are evenly spaced, so the gutter spaces its
            labels evenly. A floor below zero need not be (-200 under 0, 500
            and 1,000), so then each label is set on its own rule. */}
        <div className="os-time-chart-values" data-below-zero={floor < 0 ? "true" : undefined} aria-hidden="true" style={{ height }}>
          {[...ticks].reverse().map((tick) => <span key={tick} style={floor < 0 ? { top: topOf(tick) } : undefined}>{format(tick)}</span>)}
        </div>
        <div
          ref={plotRef}
          className="os-time-chart-plot"
          role="application"
          aria-roledescription="chart"
          aria-label={label}
          aria-describedby={captionId}
          tabIndex={0}
          style={{ height }}
          onPointerMove={onPointer}
          onPointerDown={onPointer}
          onPointerLeave={() => {
            if (!focused.current) setActive(null);
          }}
          onFocus={() => {
            focused.current = true;
            setActive((held) => held ?? (count > 0 ? count - 1 : null));
          }}
          onBlur={() => {
            focused.current = false;
            setActive(null);
          }}
          onKeyDown={onKeyDown}
        >
          <svg viewBox={`0 0 ${VIEW_WIDTH} ${height}`} preserveAspectRatio="none" aria-hidden="true">
            {rules.map((rule) => (
              <line key={rule.at} className="os-time-chart-grid" data-zero={rule.zero ? "true" : undefined} x1={0} x2={VIEW_WIDTH} y1={rule.at} y2={rule.at} vectorEffect="non-scaling-stroke" shapeRendering="crispEdges" />
            ))}
            {area === "" ? null : <path className="os-time-chart-area" data-area="current" d={area} />}
            {priorLine === "" ? null : <path className="os-time-chart-previous" data-series="previous" d={priorLine} strokeDasharray="4 4" vectorEffect="non-scaling-stroke" />}
            {line === "" ? null : <path className="os-time-chart-line" data-series="current" d={line} vectorEffect="non-scaling-stroke" />}
          </svg>
          {priorLone.map((index) => (
            <span key={`previous:${index}`} className="os-time-chart-point" data-series="previous" aria-hidden="true" style={{ left: leftOf(index), top: topOf(prior?.[index] ?? 0) }} />
          ))}
          {lone.map((index) => (
            <span key={`current:${index}`} className="os-time-chart-point" data-series="current" aria-hidden="true" style={{ left: leftOf(index), top: topOf(current[index] ?? 0) }} />
          ))}
          {runs.length === 0 ? <span className="os-time-chart-empty" aria-hidden="true">Not measured</span> : null}
          {at === null ? null : (
            <>
              <span className="os-time-chart-guide" aria-hidden="true" style={{ left: leftOf(at) }} />
              {then === null ? null : <span className="os-time-chart-dot" data-series="previous" aria-hidden="true" style={{ left: leftOf(at), top: topOf(then) }} />}
              {now === null ? null : <span className="os-time-chart-dot" data-series="current" aria-hidden="true" style={{ left: leftOf(at), top: topOf(now) }} />}
            </>
          )}
        </div>
        <div className="os-time-chart-times" aria-hidden="true">
          {timeTicks.map((index) => (
            <span
              key={index}
              data-edge={count > 1 && index === 0 ? "start" : count > 1 && index === count - 1 ? "end" : undefined}
              style={{ left: leftOf(index) }}
            >
              {bucketLabel(points[index]?.at ?? "", bucket, timeZone)}
            </span>
          ))}
        </div>
      </div>
      <figcaption id={captionId} className="os-sr-only">{summaryOf(label, bucket, points, current, format, timeZone)}</figcaption>
    </figure>
  );
}

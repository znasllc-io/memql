// Charts -- bar, line and pie, as SVG in the VNode tree.
//
// Built against v1:observability:codeMetric: windowStart / bucket /
// callCount / errorCount / errorRate / p50-p95-p99DurationNs. That concept is
// where the slot list comes from -- a time column, a categorical bucket, and
// several numeric measures of very different magnitudes -- and it is why the
// `y` slot auto-binds exactly ONE measure (see below).
//
// ONE VISUAL SYSTEM, NOT THREE RENDERINGS
//
// Bar, line and pie share this file on purpose. They use the same categorical
// palette in the same fixed slot order, the same axis + gridline frame, the
// same tick stepping, the same number formatting, the same legend, and the
// same 2px surface gap between adjacent fills. Three files would be three
// palettes and three tick algorithms within a release.
//
//   Form        Job                      Colour
//   bar         magnitude by category    ONE hue (slot 1) for every bar --
//                                        the bar length already encodes the
//                                        value, so spending the identity
//                                        channel on it says nothing new
//   line        change over time         one hue per SERIES, in slot order
//   pie         parts of a whole         one hue per SLICE, in slot order
//   proportion  parts of a whole, in a   one hue per SEGMENT, in slot order
//               single line of height
//
// PIE AND PROPORTION ANSWER THE SAME QUESTION IN DIFFERENT ROOM. Both show
// share-of-whole and both declare the same requirements, so a picker offers
// whichever the surface has space for. A pie needs a 320x240 block; the
// proportion rail is one line tall, which is what lets a page header carry
// "how does this population divide" above the population itself instead of
// spending a third of the fold on it. That is a LAYOUT difference, and layout
// is exactly the axis a form is allowed to differ on.
//
// The palette is the validated categorical default: hues in a fixed order
// (the order is the colour-vision-deficiency safety mechanism, not
// decoration), stepped separately for the light and the dark surface. Worst
// adjacent CVD separation 9.1 light / 8.4 dark, worst normal-vision 19.6 /
// 19.3, all against the >=8 / >=15 gates. Three light-mode slots sit below
// 3:1 contrast on a light surface, which obliges the relief rule: every chart
// here ships VISIBLE DIRECT LABELS (values on bars and slices, a labelled
// last point on each line) and a legend whenever there are two or more
// series, so identity and magnitude are never carried by colour alone.
//
// Past six series the palette stops: a seventh slice folds into "Other" in a
// neutral tone rather than inventing a hue.
//
// INTERACTIVITY WITHOUT SCRIPT
//
// view-kit emits no handlers (vnode.ts). Every mark therefore carries an SVG
// <title>, which every browser renders as a hover tooltip with no JavaScript
// at all, plus data-vk-category / data-vk-value for a host that wants a
// richer layer. The <svg> itself is role="img" with a summarising aria-label.

import { h, text, type VNode } from "./vnode.js";
import { formatDate, formatDateTime, formatNumber, instantValue, numberValue, scalarText } from "./format.js";
import { statusText } from "./displayCard.js";
import {
  CATEGORICAL_MAX_DISTINCT,
  boundField,
  boundFields,
  fitElement,
  profileConcept,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
} from "./fitness.js";
import type { ConceptLike, RowLike } from "./types.js";

// The number of categorical slots the palette defines. A seventh series folds
// into "Other".
export const CHART_SERIES_SLOTS = 6;


// Cartesian geometry, in viewBox units. The SVG scales to its container; these
// are proportions, not pixels.
const W = 480;
const H = 240;
const PAD_LEFT = 52;
const PAD_RIGHT = 12;
const PAD_TOP = 12;
const PAD_BOTTOM = 30;
const PLOT_W = W - PAD_LEFT - PAD_RIGHT;
const PLOT_H = H - PAD_TOP - PAD_BOTTOM;
// The surface gap that keeps two adjacent fills from reading as one shape.
const GAP = 2;
const BAR_RADIUS = 4;

// ---------------------------------------------------------------------------
// Shared scale + formatting
// ---------------------------------------------------------------------------

// niceMax rounds a data maximum up to a 1/2/2.5/5 x 10^k step boundary so the
// axis lands on readable numbers. Shared by every cartesian form -- two forms
// with different tick logic would put the same data on two different grids.
function niceStep(rough: number): number {
  if (rough <= 0) return 1;
  const magnitude = Math.pow(10, Math.floor(Math.log10(rough)));
  const scaled = rough / magnitude;
  const step = scaled <= 1 ? 1 : scaled <= 2 ? 2 : scaled <= 2.5 ? 2.5 : scaled <= 5 ? 5 : 10;
  return step * magnitude;
}

export interface AxisScale {
  readonly max: number;
  readonly ticks: readonly number[];
}

export function axisScale(dataMax: number, tickCount = 4): AxisScale {
  if (!Number.isFinite(dataMax) || dataMax <= 0) {
    return { max: 1, ticks: [0, 1] };
  }
  const step = niceStep(dataMax / tickCount);
  const max = Math.ceil(dataMax / step) * step;
  const ticks: number[] = [];
  for (let v = 0; v <= max + step / 2; v += step) {
    ticks.push(Math.round(v * 1e6) / 1e6);
  }
  return { max, ticks };
}

// formatCompact keeps an axis tick short. Full precision belongs in the
// tooltip and the direct label, not on the axis.
export function formatCompact(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1e9) return `${trim(value / 1e9)}B`;
  if (abs >= 1e6) return `${trim(value / 1e6)}M`;
  if (abs >= 1e3) return `${trim(value / 1e3)}k`;
  return formatNumber(value);
}

function trim(value: number): string {
  const rounded = Math.round(value * 10) / 10;
  return String(rounded);
}

function truncate(value: string, max = 16): string {
  return value.length <= max ? value : `${value.slice(0, max - 1)}...`;
}

// Written out rather than composed as `vk-chart-s${n}`: the stylesheet
// anti-drift guard reads class names out of this source, and a name that only
// exists at runtime is a name it cannot check.
export const CHART_SERIES_CLASSES: readonly string[] = [
  "vk-chart-s1",
  "vk-chart-s2",
  "vk-chart-s3",
  "vk-chart-s4",
  "vk-chart-s5",
  "vk-chart-s6",
];

function seriesClass(index: number): string {
  return CHART_SERIES_CLASSES[index] ?? "vk-chart-other";
}

// ---------------------------------------------------------------------------
// Shared chrome
// ---------------------------------------------------------------------------

function svgRoot(ariaLabel: string, children: VNode[]): VNode {
  return h(
    "svg",
    {
      class: "vk-chart",
      viewBox: `0 0 ${W} ${H}`,
      role: "img",
      "aria-label": ariaLabel,
    },
    children,
  );
}

// gridAndAxis draws the y gridlines, their tick labels and the baseline.
// Recessive by design: the marks are the content, the frame is scaffolding.
function gridAndAxis(scale: AxisScale): VNode[] {
  const out: VNode[] = [];
  for (const value of scale.ticks) {
    const y = PAD_TOP + PLOT_H - (value / scale.max) * PLOT_H;
    out.push(
      h("line", {
        class: "vk-chart-grid",
        x1: String(PAD_LEFT),
        y1: y.toFixed(1),
        x2: String(PAD_LEFT + PLOT_W),
        y2: y.toFixed(1),
      }),
    );
    out.push(
      h(
        "text",
        {
          class: "vk-chart-tick",
          x: String(PAD_LEFT - 6),
          y: (y + 3).toFixed(1),
          "text-anchor": "end",
        },
        [text(formatCompact(value))],
      ),
    );
  }
  out.push(
    h("line", {
      class: "vk-chart-axis",
      x1: String(PAD_LEFT),
      y1: String(PAD_TOP + PLOT_H),
      x2: String(PAD_LEFT + PLOT_W),
      y2: String(PAD_TOP + PLOT_H),
    }),
  );
  return out;
}

// legend is emitted whenever there are two or more series, so identity never
// depends on colour alone.
function legend(entries: readonly { label: string; index: number }[]): VNode {
  return h(
    "ul",
    { class: "vk-chart-legend" },
    entries.map((entry) =>
      h("li", { class: "vk-chart-legend-item" }, [
        h("span", { class: `vk-chart-swatch ${seriesClass(entry.index)}` }, []),
        h("span", { class: "vk-chart-legend-label" }, [text(entry.label)]),
      ]),
    ),
  );
}

// The figure is where the series palette is DEFINED (see styles.ts), so it is
// also where an explicit theme is stamped: the legend swatches sit outside the
// <svg> and must resolve the same tokens as the marks inside it.
function figure(children: VNode[], theme?: "light" | "dark"): VNode {
  const attrs: Record<string, string> = { class: "vk-chart-figure" };
  if (theme) attrs["data-vk-theme"] = theme;
  return h("div", attrs, children);
}

function emptyChart(entity: string, what: string): VNode {
  return h("div", { class: "vk-empty" }, [text(`No ${what} to chart for ${entity}.`)]);
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

interface Bucket {
  readonly label: string;
  value: number;
  // True for the synthetic tail bucket. It takes the neutral tone rather than
  // a palette slot: "Other" is not an identity and must not look like one.
  other?: boolean;
}

// bucketize sums a numeric field per category, or counts rows when no numeric
// field is bound. First-seen category order, so the chart follows the query's
// order rather than an alphabetisation nobody asked for.
function bucketize(
  rows: readonly RowLike[],
  categoryField: string,
  valueField: string | undefined,
): Bucket[] {
  const out: Bucket[] = [];
  const index = new Map<string, Bucket>();
  for (const row of rows) {
    const raw = row[categoryField];
    // A boolean category goes through statusText for the reason memql#3303
    // gave the status badge: a boolean is not a label. Its field name is the
    // label and the value only decides whether it is asserted, so an
    // active/inactive split reads "active" / "not active" rather than
    // "true" / "false", which is the value of a field whose name the reader
    // of a legend cannot see. Everything else keeps scalarText's formatting.
    const label =
      typeof raw === "boolean" ? statusText(categoryField, raw) : scalarText(row, categoryField);
    if (label === "") continue;
    let bucket = index.get(label);
    if (!bucket) {
      bucket = { label, value: 0 };
      index.set(label, bucket);
      out.push(bucket);
    }
    bucket.value += valueField === undefined ? 1 : (numberValue(row, valueField) ?? 0);
  }
  return out;
}

// foldToPalette orders buckets largest-first and folds everything past the
// palette into one neutral "Other". Shared by the two share-of-whole forms so
// the same row set cannot fold differently depending on which one is on
// screen -- a pie showing five categories and a rail showing six, over
// identical data, would each be reporting a different population.
function foldToPalette(buckets: readonly Bucket[]): Bucket[] {
  const sorted = [...buckets].sort((a, b) => b.value - a.value);
  if (sorted.length <= CHART_SERIES_SLOTS) return sorted;
  const head = sorted.slice(0, CHART_SERIES_SLOTS - 1);
  const tail = sorted.slice(CHART_SERIES_SLOTS - 1);
  head.push({
    label: "Other",
    value: tail.reduce((sum, b) => sum + b.value, 0),
    other: true,
  });
  return head;
}

// shareOf is the percentage a bucket takes of the whole, to one decimal. One
// implementation so the pie's slice label, the rail's legend and both
// tooltips cannot round differently.
function shareOf(value: number, total: number): number {
  return total === 0 ? 0 : Math.round((value / total) * 1000) / 10;
}

// ---------------------------------------------------------------------------
// Bar
// ---------------------------------------------------------------------------

export const BAR_CHART_ELEMENT: ElementSpec = {
  id: "chart.bar",
  title: "Bar chart",
  summary: "One bar per category, sized by a measure or by row count.",
  requires: [
    {
      slot: "category",
      description: "the category each bar stands for",
      kinds: ["text"],
      min: 1,
      distinctMax: CATEGORICAL_MAX_DISTINCT,
      preferFewestDistinct: true,
      prefer: ["status"],
    },
    {
      slot: "value",
      description: "the measure each bar's length shows",
      kinds: ["number"],
      min: 0,
      autoMax: 1,
      degraded: "bars show how many rows fall in each category",
    },
  ],
  render: (input) => drawBar(input),
};

function drawBar({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const categoryField = boundField(fit, "category");
  const valueField = boundField(fit, "value");
  if (!categoryField) return emptyChart(concept.entity, "categories");

  const buckets = bucketize(rows, categoryField, valueField);
  if (buckets.length === 0) return emptyChart(concept.entity, "categories");

  const measure = valueField ?? "rows";
  const scale = axisScale(Math.max(...buckets.map((b) => b.value)));
  const slot = PLOT_W / buckets.length;
  const barWidth = Math.max(2, slot - GAP);

  const marks: VNode[] = [];
  buckets.forEach((bucket, i) => {
    const x = PAD_LEFT + i * slot + GAP / 2;
    const height = (bucket.value / scale.max) * PLOT_H;
    const y = PAD_TOP + PLOT_H - height;
    marks.push(
      h(
        "path",
        {
          // Every bar is slot 1: the length is the magnitude, so colour has
          // no second job to do here.
          class: "vk-chart-bar vk-chart-s1",
          d: barPath(x, y, barWidth, height, BAR_RADIUS),
          "data-vk-category": bucket.label,
          "data-vk-value": String(bucket.value),
        },
        [h("title", {}, [text(`${bucket.label}: ${formatNumber(bucket.value)} ${measure}`)])],
      ),
    );
    // The direct value label. Required, not decorative: it is the relief that
    // makes a sub-3:1 fill legible in light mode.
    marks.push(
      h(
        "text",
        {
          class: "vk-chart-value",
          x: (x + barWidth / 2).toFixed(1),
          y: (y - 4).toFixed(1),
          "text-anchor": "middle",
        },
        [text(formatCompact(bucket.value))],
      ),
    );
    marks.push(
      h(
        "text",
        {
          class: "vk-chart-tick",
          x: (x + barWidth / 2).toFixed(1),
          y: String(PAD_TOP + PLOT_H + 14),
          "text-anchor": "middle",
        },
        [text(truncate(bucket.label, Math.max(6, Math.floor(slot / 6))))],
      ),
    );
  });

  const summary =
    `Bar chart of ${measure} by ${categoryField} for ${concept.entity}: ` +
    buckets.map((b) => `${b.label} ${formatNumber(b.value)}`).join(", ");

  return figure([svgRoot(summary, [...gridAndAxis(scale), ...marks])], options?.theme);
}

// barPath rounds only the data end and anchors the other end flat on the
// baseline -- a bar rounded at both ends floats off its own axis.
function barPath(x: number, y: number, w: number, hgt: number, radius: number): string {
  const r = Math.max(0, Math.min(radius, hgt, w / 2));
  const bottom = y + hgt;
  return [
    `M${x.toFixed(1)},${bottom.toFixed(1)}`,
    `L${x.toFixed(1)},${(y + r).toFixed(1)}`,
    `Q${x.toFixed(1)},${y.toFixed(1)} ${(x + r).toFixed(1)},${y.toFixed(1)}`,
    `L${(x + w - r).toFixed(1)},${y.toFixed(1)}`,
    `Q${(x + w).toFixed(1)},${y.toFixed(1)} ${(x + w).toFixed(1)},${(y + r).toFixed(1)}`,
    `L${(x + w).toFixed(1)},${bottom.toFixed(1)}`,
    "Z",
  ].join(" ");
}

// ---------------------------------------------------------------------------
// Line
// ---------------------------------------------------------------------------

export const LINE_CHART_ELEMENT: ElementSpec = {
  id: "chart.line",
  title: "Line chart",
  summary: "A measure over time, one line per series.",
  requires: [
    {
      slot: "x",
      description: "the time axis",
      kinds: ["datetime"],
      min: 1,
      preferNames: ["windowStart", "occurredAt", "startsAt", "timestamp", "createdAt"],
    },
    {
      slot: "y",
      description: "the measure plotted against time",
      kinds: ["number"],
      min: 1,
      max: CHART_SERIES_SLOTS,
      // One series unless a view author names more: see `autoMax`. Two
      // measures of different magnitude on one axis is the dual-axis mistake
      // wearing a disguise.
      autoMax: 1,
    },
  ],
  render: (input) => drawLine(input),
};

function drawLine({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const xField = boundField(fit, "x");
  const yFields = boundFields(fit, "y");
  if (!xField || yFields.length === 0) return emptyChart(concept.entity, "series");

  const points = rows
    .map((row) => ({ at: instantValue(row, xField), row }))
    .filter((p): p is { at: Date; row: RowLike } => p.at !== undefined)
    .sort((a, b) => a.at.getTime() - b.at.getTime());
  if (points.length === 0) return emptyChart(concept.entity, "timestamps");

  const firstMs = points[0].at.getTime();
  const lastMs = points[points.length - 1].at.getTime();
  const span = lastMs - firstMs;
  const xOf = (at: Date): number =>
    span === 0 ? PAD_LEFT + PLOT_W / 2 : PAD_LEFT + ((at.getTime() - firstMs) / span) * PLOT_W;

  let dataMax = 0;
  for (const { row } of points) {
    for (const field of yFields) {
      const v = numberValue(row, field);
      if (v !== undefined && v > dataMax) dataMax = v;
    }
  }
  const scale = axisScale(dataMax);
  const yOf = (v: number): number => PAD_TOP + PLOT_H - (v / scale.max) * PLOT_H;

  const marks: VNode[] = [];
  yFields.forEach((field, index) => {
    const series = points
      .map((p) => ({ at: p.at, value: numberValue(p.row, field) }))
      .filter((p): p is { at: Date; value: number } => p.value !== undefined);
    if (series.length === 0) return;

    const cls = seriesClass(index);
    if (series.length > 1) {
      marks.push(
        h("polyline", {
          class: `vk-chart-line ${cls}`,
          points: series.map((p) => `${xOf(p.at).toFixed(1)},${yOf(p.value).toFixed(1)}`).join(" "),
        }),
      );
    }
    for (const p of series) {
      marks.push(
        h(
          "circle",
          {
            class: `vk-chart-dot ${cls}`,
            cx: xOf(p.at).toFixed(1),
            cy: yOf(p.value).toFixed(1),
            r: "4",
            "data-vk-category": formatDateTime(p.at),
            "data-vk-value": String(p.value),
          },
          [
            h("title", {}, [
              text(`${field} ${formatNumber(p.value)} at ${formatDateTime(p.at)}`),
            ]),
          ],
        ),
      );
    }
    // Direct-label the last point of each series: the relief rule again, and
    // it means a reader never has to trace a line back to a legend.
    const last = series[series.length - 1];
    marks.push(
      h(
        "text",
        {
          class: "vk-chart-value",
          x: (xOf(last.at) - 4).toFixed(1),
          y: (yOf(last.value) - 8).toFixed(1),
          "text-anchor": "end",
        },
        [text(`${truncate(field, 14)} ${formatCompact(last.value)}`)],
      ),
    );
  });

  marks.push(
    h(
      "text",
      {
        class: "vk-chart-tick",
        x: String(PAD_LEFT),
        y: String(PAD_TOP + PLOT_H + 14),
        "text-anchor": "start",
      },
      [text(formatDate(points[0].at))],
    ),
  );
  marks.push(
    h(
      "text",
      {
        class: "vk-chart-tick",
        x: String(PAD_LEFT + PLOT_W),
        y: String(PAD_TOP + PLOT_H + 14),
        "text-anchor": "end",
      },
      [text(formatDate(points[points.length - 1].at))],
    ),
  );

  const summary =
    `Line chart of ${yFields.join(", ")} over ${xField} for ${concept.entity}, ` +
    `${points.length} points from ${formatDate(points[0].at)} to ${formatDate(points[points.length - 1].at)}.`;

  const body: VNode[] = [svgRoot(summary, [...gridAndAxis(scale), ...marks])];
  if (yFields.length > 1) {
    body.push(legend(yFields.map((label, index) => ({ label, index }))));
  }
  return figure(body, options?.theme);
}

// ---------------------------------------------------------------------------
// Pie
// ---------------------------------------------------------------------------

export const PIE_CHART_ELEMENT: ElementSpec = {
  id: "chart.pie",
  title: "Pie chart",
  summary: "Each category's share of the whole.",
  requires: [
    {
      slot: "category",
      description: "the category each slice stands for",
      kinds: ["text"],
      min: 1,
      distinctMax: CHART_SERIES_SLOTS + 2,
      preferFewestDistinct: true,
      prefer: ["status"],
    },
    {
      slot: "value",
      description: "the measure each slice's size shows",
      kinds: ["number"],
      min: 0,
      autoMax: 1,
      degraded: "slices show how many rows fall in each category",
    },
  ],
  render: (input) => drawPie(input),
};

// Wider than it is tall on purpose: the share labels sit OUTSIDE the arc, and
// an SVG root clips its own overflow -- a square viewBox would cut "17.8%" off
// at the east and west edges.
const PIE_W = 320;
const PIE_H = 240;
const PIE_R = 80;
const PIE_LABEL_GAP = 12;

function drawPie({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const categoryField = boundField(fit, "category");
  const valueField = boundField(fit, "value");
  if (!categoryField) return emptyChart(concept.entity, "categories");

  // Largest first, then anything past the palette folds into one neutral
  // "Other" slice. Inventing a seventh hue is how a palette stops being safe.
  const buckets = foldToPalette(
    bucketize(rows, categoryField, valueField).filter((b) => b.value > 0),
  );
  if (buckets.length === 0) return emptyChart(concept.entity, "categories");

  const total = buckets.reduce((sum, b) => sum + b.value, 0);
  const cx = PIE_W / 2;
  const cy = PIE_H / 2;
  const measure = valueField ?? "rows";

  const marks: VNode[] = [];
  let angle = -Math.PI / 2;
  buckets.forEach((bucket, i) => {
    const sweep = (bucket.value / total) * Math.PI * 2;
    const end = angle + sweep;
    const share = shareOf(bucket.value, total);
    marks.push(
      h(
        "path",
        {
          // The 2px stroke is painted in the surface colour: it is the gap
          // between adjacent fills, not an outline.
          class: `vk-chart-slice ${bucket.other ? "vk-chart-other" : seriesClass(i)}`,
          d: slicePath(cx, cy, PIE_R, angle, end, buckets.length === 1),
          "data-vk-category": bucket.label,
          "data-vk-value": String(bucket.value),
        },
        [
          h("title", {}, [
            text(`${bucket.label}: ${formatNumber(bucket.value)} ${measure} (${share}%)`),
          ]),
        ],
      ),
    );
    // Direct share labels, outside the arc: the light-mode relief rule, and
    // a pie without them makes the reader estimate angles.
    const mid = angle + sweep / 2;
    if (share >= 4) {
      marks.push(
        h(
          "text",
          {
            class: "vk-chart-value",
            x: (cx + Math.cos(mid) * (PIE_R + PIE_LABEL_GAP)).toFixed(1),
            y: (cy + Math.sin(mid) * (PIE_R + PIE_LABEL_GAP) + 3).toFixed(1),
            "text-anchor": Math.cos(mid) < -0.1 ? "end" : Math.cos(mid) > 0.1 ? "start" : "middle",
          },
          [text(`${share}%`)],
        ),
      );
    }
    angle = end;
  });

  const summary =
    `Pie chart of ${measure} by ${categoryField} for ${concept.entity}: ` +
    buckets
      .map((b) => `${b.label} ${Math.round((b.value / total) * 100)}%`)
      .join(", ");

  const svg = h(
    "svg",
    {
      class: "vk-chart",
      viewBox: `0 0 ${PIE_W} ${PIE_H}`,
      role: "img",
      "aria-label": summary,
    },
    marks,
  );

  return figure(
    [
      svg,
      legend(
        buckets.map((bucket, index) => ({
          label: bucket.label,
          index: bucket.other ? CHART_SERIES_SLOTS : index,
        })),
      ),
    ],
    options?.theme,
  );
}

function slicePath(
  cx: number,
  cy: number,
  r: number,
  start: number,
  end: number,
  whole: boolean,
): string {
  if (whole) {
    // A single 360-degree slice as an arc collapses to a point: the start and
    // end coordinates are identical, so SVG draws nothing. Two half-arcs.
    return [
      `M${(cx - r).toFixed(1)},${cy.toFixed(1)}`,
      `A${r},${r} 0 1 1 ${(cx + r).toFixed(1)},${cy.toFixed(1)}`,
      `A${r},${r} 0 1 1 ${(cx - r).toFixed(1)},${cy.toFixed(1)}`,
      "Z",
    ].join(" ");
  }
  const x1 = cx + Math.cos(start) * r;
  const y1 = cy + Math.sin(start) * r;
  const x2 = cx + Math.cos(end) * r;
  const y2 = cy + Math.sin(end) * r;
  const largeArc = end - start > Math.PI ? 1 : 0;
  return [
    `M${cx.toFixed(1)},${cy.toFixed(1)}`,
    `L${x1.toFixed(1)},${y1.toFixed(1)}`,
    `A${r},${r} 0 ${largeArc} 1 ${x2.toFixed(1)},${y2.toFixed(1)}`,
    "Z",
  ].join(" ");
}

// ---------------------------------------------------------------------------
// Proportion rail
// ---------------------------------------------------------------------------

// The same question as the pie -- what share does each category hold -- in one
// line of height instead of a block. Added for the predefined operator views
// (memql#3319), which all open with "here is a population, here is how it
// divides, here it is enumerated": the divide step has to sit ABOVE the
// population without pushing it off the fold, and no existing form does that.
// A bar chart shows magnitude by category, not share of whole, and needs a
// vertical axis; a pie shows share and needs a square.
//
// COLOUR IS NOT THE ONLY CARRIER, per the palette's relief rule. A segment is
// too short to hold a direct label, so the legend beneath carries the full
// reading for every segment -- "developer 12 (34.3%)" -- and is emitted
// ALWAYS, including for a single category, rather than only past two series
// as the cartesian forms do.
export const PROPORTION_BAR_ELEMENT: ElementSpec = {
  id: "chart.proportion",
  title: "Proportion rail",
  summary: "Each category's share of the whole, on a single line.",
  requires: [
    {
      slot: "category",
      description: "the category each segment stands for",
      kinds: ["text", "boolean"],
      min: 1,
      distinctMax: CHART_SERIES_SLOTS + 2,
      preferFewestDistinct: true,
      prefer: ["status"],
    },
    {
      slot: "value",
      description: "the measure each segment's width shows",
      kinds: ["number"],
      min: 0,
      autoMax: 1,
      degraded: "segments show how many rows fall in each category",
    },
  ],
  render: (input) => drawProportion(input),
};

const RAIL_W = 480;
const RAIL_H = 14;
const RAIL_R = 3;

function drawProportion({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const categoryField = boundField(fit, "category");
  const valueField = boundField(fit, "value");
  if (!categoryField) return emptyChart(concept.entity, "categories");

  const buckets = foldToPalette(
    bucketize(rows, categoryField, valueField).filter((b) => b.value > 0),
  );
  if (buckets.length === 0) return emptyChart(concept.entity, "categories");

  const total = buckets.reduce((sum, b) => sum + b.value, 0);
  const measure = valueField ?? "rows";

  const marks: VNode[] = [];
  let x = 0;
  buckets.forEach((bucket, i) => {
    const full = (bucket.value / total) * RAIL_W;
    // The GAP is taken out of the segment rather than drawn over it, so the
    // rail's total width still reads as the whole population. A share small
    // enough to vanish keeps 1 unit: a category that exists must be visible,
    // and the legend says how small it is.
    const width = Math.max(1, full - (i === buckets.length - 1 ? 0 : GAP));
    marks.push(
      h(
        "rect",
        {
          class: `vk-chart-rail-seg ${bucket.other ? "vk-chart-other" : seriesClass(i)}`,
          x: x.toFixed(1),
          y: "0",
          width: width.toFixed(1),
          height: String(RAIL_H),
          rx: String(RAIL_R),
          "data-vk-category": bucket.label,
          "data-vk-value": String(bucket.value),
        },
        [
          h("title", {}, [
            text(
              `${bucket.label}: ${formatNumber(bucket.value)} ${measure} ` +
                `(${shareOf(bucket.value, total)}%)`,
            ),
          ]),
        ],
      ),
    );
    x += full;
  });

  const summary =
    `Proportion of ${measure} by ${categoryField} for ${concept.entity}: ` +
    buckets.map((b) => `${b.label} ${shareOf(b.value, total)}%`).join(", ");

  return figure(
    [
      h(
        "svg",
        {
          class: "vk-chart vk-chart-rail",
          viewBox: `0 0 ${RAIL_W} ${RAIL_H}`,
          // A rail is a proportion, not a picture with an aspect ratio worth
          // keeping: it should fill whatever width it is given at a fixed
          // height, so the slice is stretched rather than letterboxed.
          preserveAspectRatio: "none",
          role: "img",
          "aria-label": summary,
        },
        marks,
      ),
      legend(
        buckets.map((bucket, index) => ({
          label:
            `${bucket.label} ${formatNumber(bucket.value)} ` +
            `(${shareOf(bucket.value, total)}%)`,
          index: bucket.other ? CHART_SERIES_SLOTS : index,
        })),
      ),
    ],
    options?.theme,
  );
}

// ---------------------------------------------------------------------------
// Convenience entry points
// ---------------------------------------------------------------------------

export function renderBarChart(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(BAR_CHART_ELEMENT, profileConcept(concept, rows), options);
  return drawBar({ rows, concept, fit, options });
}

export function renderLineChart(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(LINE_CHART_ELEMENT, profileConcept(concept, rows), options);
  return drawLine({ rows, concept, fit, options });
}

export function renderPieChart(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(PIE_CHART_ELEMENT, profileConcept(concept, rows), options);
  return drawPie({ rows, concept, fit, options });
}

export function renderProportionBar(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(PROPORTION_BAR_ELEMENT, profileConcept(concept, rows), options);
  return drawProportion({ rows, concept, fit, options });
}

// Stat tiles -- the headline numbers over a row set.
//
// Built against no one concept, because the aggregate IS the element: the
// count of rows is available for every concept in the tree, and any numeric
// field the rows carry adds a tile. That makes this the one element that fits
// everything, including an empty result -- "0 nodes" is a true and useful
// rendering, which is why minRows is 0 here and 1 everywhere else.
//
// A number is not always a chart. A single magnitude with no comparison to
// make reads better as a large figure than as a one-bar bar chart, and the
// tile is where that lands.

import { h, text, type VNode } from "./vnode.js";
import { formatNumber, numberValue } from "./format.js";
import {
  boundFields,
  fitElement,
  profileConcept,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
} from "./fitness.js";
import type { ConceptLike, RowLike } from "./types.js";

// Beyond this many measures the strip stops being a headline.
const MAX_METRICS = 3;

export const STAT_TILE_ELEMENT: ElementSpec = {
  id: "statTile",
  title: "Stat tiles",
  summary: "The row count plus a headline figure for each numeric field.",
  // The opening reading: the numbers ARE the answer to "how many".
  band: "reading",
  minRows: 0,
  requires: [
    {
      slot: "metric",
      description: "the measures to total",
      kinds: ["number"],
      min: 0,
      max: MAX_METRICS,
      autoMax: MAX_METRICS,
      degraded: "only the row count is shown",
    },
  ],
  render: (input) => draw(input),
};

function tile(value: string, label: string, sub?: string, metric?: string): VNode {
  const attrs: Record<string, string> = { class: "vk-stat" };
  if (metric) attrs["data-vk-metric"] = metric;
  const children: VNode[] = [
    h("div", { class: "vk-stat-value" }, [text(value)]),
    h("div", { class: "vk-stat-label" }, [text(label)]),
  ];
  if (sub) children.push(h("div", { class: "vk-stat-sub" }, [text(sub)]));
  return h("div", attrs, children);
}

function draw({ rows, concept, fit }: ElementRenderInput): VNode {
  const tiles: VNode[] = [
    tile(formatNumber(rows.length), rows.length === 1 ? concept.entity : `${concept.entity} rows`),
  ];

  for (const field of boundFields(fit, "metric")) {
    let sum = 0;
    let count = 0;
    for (const row of rows) {
      const v = numberValue(row, field);
      if (v === undefined) continue;
      sum += v;
      count += 1;
    }
    if (count === 0) continue;
    tiles.push(
      tile(
        formatNumber(sum),
        `${field} total`,
        // The average is the sub-line rather than a tile of its own: it is
        // the same measure, and two tiles for one field would double-count
        // the reader's attention.
        `avg ${formatNumber(sum / count)} over ${formatNumber(count)}`,
        field,
      ),
    );
  }

  return h("div", { class: "vk-stats" }, tiles);
}

export function renderStatTiles(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(STAT_TILE_ELEMENT, profileConcept(concept, rows), options);
  return draw({ rows, concept, fit, options });
}

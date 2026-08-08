// Map -- rows plotted on an equirectangular world projection.
//
// NO CONCEPT IN THE TREE CARRIES COORDINATES TODAY. That is a deliberate
// state, not an oversight, and it is the clearest demonstration of what the
// fitness contract buys: this element ships with its requirements declared,
// reports `unfit` for every concept that currently exists, and therefore is
// never offered by a view picker -- with the reason spelled out in words
// ("Map cannot render calendarEvent. It needs the north-south coordinate, and
// there is no field the concept points at for it, and none named latitude,
// lat"). The day a concept declares a lat/lon pair, the map appears with no
// change to this file. test/elements.test.ts pins that "nothing fits yet"
// state against the real DSL tree, so the assertion fails -- loudly, with
// instructions -- when such a concept lands.
//
// Both coordinate slots are `explicitOnly`: latitude and longitude are
// semantically loaded in a way "some number field" is not, and a map that
// confidently plots callCount against errorCount is worse than a map that
// says it cannot render this concept.
//
// The projection is equirectangular (x = lon, y = -lat, linearly). It is the
// only projection that needs no data: a real basemap would mean shipping
// country geometry, and view-kit has no dependencies and no network. The
// graticule is drawn so a reader can see the projection rather than guess it.

import { h, text, type VNode } from "./vnode.js";
import { rowDisplayId } from "./rowList.js";
import { numberValue, scalarText } from "./format.js";
import {
  boundField,
  fitElement,
  profileConcept,
  type ElementOptions,
  type ElementRenderInput,
  type ElementSpec,
} from "./fitness.js";
import type { ConceptLike, RowLike } from "./types.js";

export const MAP_ELEMENT: ElementSpec = {
  id: "map",
  title: "Map",
  summary: "Rows as points on a world projection.",
  requires: [
    {
      slot: "latitude",
      description: "the north-south coordinate",
      kinds: ["number"],
      min: 1,
      explicitOnly: true,
      preferNames: ["latitude", "lat"],
    },
    {
      slot: "longitude",
      description: "the east-west coordinate",
      kinds: ["number"],
      min: 1,
      explicitOnly: true,
      preferNames: ["longitude", "lon", "lng", "long"],
    },
    {
      slot: "label",
      description: "what to call each point",
      kinds: ["text"],
      min: 0,
      prefer: ["primary"],
      explicitOnly: true,
      degraded: "points are labelled by their id",
    },
  ],
  render: (input) => draw(input),
};

const MAP_W = 360;
const MAP_H = 180;
const GRATICULE_DEGREES = 30;

function draw({ rows, concept, fit, options }: ElementRenderInput): VNode {
  const latField = boundField(fit, "latitude");
  const lonField = boundField(fit, "longitude");
  const labelField = boundField(fit, "label");
  if (!latField || !lonField) {
    return h("div", { class: "vk-empty" }, [
      text(`No coordinates on ${concept.entity}.`),
    ]);
  }

  const marks: VNode[] = [];
  for (let deg = -180 + GRATICULE_DEGREES; deg < 180; deg += GRATICULE_DEGREES) {
    const x = deg + 180;
    marks.push(
      h("line", {
        class: "vk-map-graticule",
        x1: String(x),
        y1: "0",
        x2: String(x),
        y2: String(MAP_H),
      }),
    );
  }
  for (let deg = -90 + GRATICULE_DEGREES; deg < 90; deg += GRATICULE_DEGREES) {
    const y = 90 - deg;
    marks.push(
      h("line", {
        class: "vk-map-graticule",
        x1: "0",
        y1: String(y),
        x2: String(MAP_W),
        y2: String(y),
      }),
    );
  }
  // The equator and the prime meridian, drawn heavier than the rest of the
  // graticule so the projection has an origin a reader can find.
  marks.push(
    h("line", {
      class: "vk-map-axis",
      x1: "0",
      y1: String(MAP_H / 2),
      x2: String(MAP_W),
      y2: String(MAP_H / 2),
    }),
  );
  marks.push(
    h("line", {
      class: "vk-map-axis",
      x1: String(MAP_W / 2),
      y1: "0",
      x2: String(MAP_W / 2),
      y2: String(MAP_H),
    }),
  );

  let plotted = 0;
  for (const row of rows) {
    const lat = numberValue(row, latField);
    const lon = numberValue(row, lonField);
    if (lat === undefined || lon === undefined) continue;
    if (lat < -90 || lat > 90 || lon < -180 || lon > 180) continue;
    plotted += 1;
    const id = rowDisplayId(row);
    const label = scalarText(row, labelField) || id;
    const attrs: Record<string, string> = {
      // A single-series plot: every point is slot 1, because there is no
      // second identity to encode.
      class: "vk-map-point vk-chart-s1",
      cx: String(lon + 180),
      cy: String(90 - lat),
      r: "3",
      "data-row-id": id,
    };
    if (options?.selectedRowId !== undefined && id === options.selectedRowId) {
      attrs["data-selected"] = "true";
    }
    marks.push(h("circle", attrs, [h("title", {}, [text(`${label} (${lat}, ${lon})`)])]));
  }

  if (plotted === 0) {
    return h("div", { class: "vk-empty" }, [
      text(`No plottable coordinates on ${concept.entity}.`),
    ]);
  }

  const svgAttrs: Record<string, string> = {
    class: "vk-map",
    viewBox: `0 0 ${MAP_W} ${MAP_H}`,
    role: "img",
    "aria-label": `Map of ${plotted} ${concept.entity} rows by ${latField} and ${lonField}.`,
  };
  // Same figure wrapper as the charts: that is where the series palette is
  // defined, and the map's points use its slot-1 hue.
  const figureAttrs: Record<string, string> = { class: "vk-chart-figure" };
  if (options?.theme) figureAttrs["data-vk-theme"] = options.theme;
  return h("div", figureAttrs, [h("svg", svgAttrs, marks)]);
}

export function renderMap(
  rows: readonly RowLike[],
  concept: ConceptLike,
  options: ElementOptions = {},
): VNode {
  const fit = fitElement(MAP_ELEMENT, profileConcept(concept, rows), options);
  return draw({ rows, concept, fit, options });
}

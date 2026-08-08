// Every element, against the concept it was built for.
//
// Each block below reads the REAL concept out of dsl/<domain>/concepts.memql,
// asserts its sample rows only use fields that concept declares (with values
// of the declared type), and then checks two things: which concrete fields
// the element bound, and what it rendered. A renamed field in the DSL breaks
// these tests rather than silently leaving an element pointed at a name that
// no longer exists.

import test from "node:test";
import assert from "node:assert/strict";

import {
  assertRowsMatchConcept,
  conceptLike,
  dslConcept,
  loadAllDslConcepts,
  syntheticRow,
} from "./support/dslConcepts.js";
import { renderToHtml } from "../src/vnode.js";
import {
  boundField,
  boundFields,
  explainFit,
  fitElement,
  fitElements,
  profileConcept,
  renderElement,
} from "../src/fitness.js";
import { VIEW_KIT_ELEMENTS } from "../src/elements.js";
import { CALENDAR_ELEMENT, renderCalendar } from "../src/calendar.js";
import { CHECKLIST_ELEMENT, renderChecklist } from "../src/checklist.js";
import { TABLE_ELEMENT, renderTable } from "../src/table.js";
import { TIMELINE_ELEMENT, renderTimeline } from "../src/timeline.js";
import { KANBAN_ELEMENT, renderKanban } from "../src/kanban.js";
import { STAT_TILE_ELEMENT, renderStatTiles } from "../src/statTile.js";
import { MAP_ELEMENT, renderMap } from "../src/map.js";
import {
  BAR_CHART_ELEMENT,
  LINE_CHART_ELEMENT,
  PIE_CHART_ELEMENT,
  renderBarChart,
  renderLineChart,
  renderPieChart,
} from "../src/chart.js";

// ---------------------------------------------------------------------------
// Calendar -- v1:calendar:calendarEvent
// ---------------------------------------------------------------------------

const CALENDAR_EVENT = dslConcept("v1:calendar:calendarEvent");
const EVENTS = [
  {
    id: "calendarEvent:e1",
    ownerUserId: "user:u1",
    title: "Karate class",
    startsAt: "2026-08-04T17:00:00Z",
    endsAt: "2026-08-04T18:00:00Z",
    allDay: false,
    location: "Dojo on 5th",
    source: "native",
    deleted: false,
  },
  {
    id: "calendarEvent:e2",
    ownerUserId: "user:u1",
    title: "Out of office",
    startsAt: "2026-08-12T00:00:00Z",
    endsAt: "2026-08-14T00:00:00Z",
    allDay: true,
    source: "native",
    deleted: false,
  },
  {
    id: "calendarEvent:e3",
    ownerUserId: "user:u1",
    title: "Dentist",
    startsAt: "2026-09-01T09:30:00Z",
    source: "native",
    deleted: false,
  },
];

test("the calendar's sample rows really are calendarEvent rows", () => {
  assertRowsMatchConcept(CALENDAR_EVENT, EVENTS, assert);
});

test("calendar binds calendarEvent's own fields", () => {
  const fit = fitElement(
    CALENDAR_ELEMENT,
    profileConcept(conceptLike(CALENDAR_EVENT), EVENTS),
  );
  assert.equal(fit.verdict, "full");
  assert.equal(boundField(fit, "start"), "startsAt");
  assert.equal(boundField(fit, "end"), "endsAt");
  assert.equal(boundField(fit, "label"), "title");
  assert.equal(boundField(fit, "allDay"), "allDay");
  assert.equal(boundField(fit, "detail"), "location");
});

test("calendar renders the month of the earliest event", () => {
  const html = renderToHtml(renderCalendar(EVENTS, conceptLike(CALENDAR_EVENT)));
  assert.match(html, /data-vk-month="2026-08"/);
  assert.match(html, /August 2026/);
  // The 4th carries the timed event with its clock time.
  assert.match(html, /data-vk-date="2026-08-04"[\s\S]*?17:00[\s\S]*?Karate class/);
  // An all-day row shows no clock time.
  assert.doesNotMatch(html, /00:00/);
  // A multi-day span says where it ends rather than pretending to be one day.
  assert.match(html, /to 2026-08-14/);
  // September's event is counted, not silently dropped.
  assert.match(html, /1 more outside August 2026/);
});

test("calendar renders a different month on request", () => {
  const html = renderToHtml(
    renderCalendar(EVENTS, conceptLike(CALENDAR_EVENT), { month: "2026-09" }),
  );
  assert.match(html, /September 2026/);
  assert.match(html, /Dentist/);
  assert.match(html, /2 more outside September 2026/);
});

// ---------------------------------------------------------------------------
// Checklist -- v1:todos:todo
// ---------------------------------------------------------------------------

const TODO = dslConcept("v1:todos:todo");
const TODOS = [
  {
    id: "todo:t1",
    ownerUserId: "user:u1",
    title: "Ship the element library",
    done: false,
    dueAt: "2026-08-10T00:00:00Z",
    priority: "high",
  },
  {
    id: "todo:t2",
    ownerUserId: "user:u1",
    title: "Write the fitness doc",
    done: true,
    priority: "high",
  },
  {
    id: "todo:t3",
    ownerUserId: "user:u1",
    title: "Re-validate the palette",
    done: false,
    priority: "low",
  },
];

test("the checklist's sample rows really are todo rows", () => {
  assertRowsMatchConcept(TODO, TODOS, assert);
});

test("checklist binds todo's own fields", () => {
  const fit = fitElement(CHECKLIST_ELEMENT, profileConcept(conceptLike(TODO), TODOS));
  assert.equal(fit.verdict, "full");
  // `done` is found through the concept's declared status slot, not by name.
  assert.equal(boundField(fit, "done"), "done");
  assert.equal(boundField(fit, "label"), "title");
  assert.equal(boundField(fit, "due"), "dueAt");
  assert.equal(boundField(fit, "group"), "priority");
});

test("checklist renders open items before done ones, per group", () => {
  const html = renderToHtml(renderChecklist(TODOS, conceptLike(TODO)));
  assert.match(html, /1 of 3 done/);
  assert.match(html, /vk-checklist-group">high</);
  const openFirst = html.indexOf("Ship the element library");
  const doneAfter = html.indexOf("Write the fitness doc");
  assert.ok(openFirst < doneAfter, "an open item must precede a done one in its group");
  assert.match(html, /aria-checked="true"/);
  assert.match(html, /aria-checked="false"/);
  assert.match(html, /data-vk-done="true"/);
  assert.match(html, /2026-08-10/);
});

test("calendar also plots todos -- partially, and it says which part is missing", () => {
  const profile = profileConcept(conceptLike(TODO), TODOS);
  const fit = fitElement(CALENDAR_ELEMENT, profile);
  assert.equal(fit.verdict, "partial");
  assert.equal(boundField(fit, "start"), "dueAt");
  assert.equal(boundField(fit, "label"), "title");
  assert.equal(boundField(fit, "end"), undefined);
  const words = explainFit(CALENDAR_ELEMENT, fit, profile);
  assert.match(words, /Calendar fits todo, with limits\./);
  assert.match(words, /uses dueAt for the date each row sits on/);
  assert.match(words, /single moment rather than a span/);
});

// ---------------------------------------------------------------------------
// Table + board -- v1:cluster:node
// ---------------------------------------------------------------------------

const NODE = dslConcept("v1:cluster:node");
const NODES = [
  {
    id: "bff-local-0",
    nodeType: "bff",
    address: "bff:50051",
    health: "healthy",
    environment: "development",
    region: "local",
    lastSeen: "2026-08-08T10:00:00Z",
    capabilities: ["integration.email.sendEmail"],
  },
  {
    id: "agent-local-0",
    nodeType: "agent",
    address: "agent:50051",
    health: "degraded",
    environment: "development",
    region: "local",
    lastSeen: "2026-08-08T09:58:00Z",
    capabilities: [],
  },
  {
    id: "agent-local-1",
    nodeType: "agent",
    address: "agent:50052",
    health: "healthy",
    environment: "development",
    region: "local",
    lastSeen: "2026-08-08T10:01:00Z",
    capabilities: [],
  },
];

test("the table's sample rows really are cluster node rows", () => {
  assertRowsMatchConcept(NODE, NODES, assert);
});

test("table leads with the display-card columns and sorts on request", () => {
  const fit = fitElement(TABLE_ELEMENT, profileConcept(conceptLike(NODE), NODES));
  assert.equal(fit.verdict, "full");
  assert.deepEqual(boundFields(fit, "column").slice(0, 4), [
    "id",
    "nodeType",
    "address",
    "health",
  ]);
  // capabilities is a list, so it is not a column.
  assert.ok(!boundFields(fit, "column").includes("capabilities"));

  const html = renderToHtml(
    renderTable(NODES, conceptLike(NODE), {
      sort: { field: "address", direction: "desc" },
    }),
  );
  assert.match(html, /data-vk-sort-field="address" aria-sort="descending"/);
  const first = html.indexOf("agent:50052");
  const second = html.indexOf("agent:50051");
  const third = html.indexOf("bff:50051");
  assert.ok(third < first && first < second, "descending sort must reverse the order");
});

test("table sorts numerically, not lexically", () => {
  const rows = [{ id: "a", callCount: 9 }, { id: "b", callCount: 100 }];
  const html = renderToHtml(
    renderTable(rows, { id: "x", entity: "x" }, {
      sort: { field: "callCount", direction: "asc" },
    }),
  );
  assert.ok(html.indexOf(">9<") < html.indexOf(">100<"), "9 sorts before 100");
});

test("board groups nodes by the concept's declared status field", () => {
  const fit = fitElement(KANBAN_ELEMENT, profileConcept(conceptLike(NODE), NODES));
  assert.equal(fit.verdict, "full");
  assert.equal(boundField(fit, "group"), "health");
  assert.equal(boundField(fit, "label"), "id");
  assert.equal(boundField(fit, "detail"), "nodeType");

  const html = renderToHtml(renderKanban(NODES, conceptLike(NODE)));
  assert.match(html, /data-vk-group-field="health"/);
  assert.match(html, /data-status="healthy"/);
  assert.match(html, /data-status="degraded"/);
  assert.match(html, /vk-board-count">2</);
});

// ---------------------------------------------------------------------------
// Table -- v1:identity:user
// ---------------------------------------------------------------------------

const USER = dslConcept("v1:identity:user");
const USERS = [
  {
    id: "user:u1",
    displayName: "Ada Lovelace",
    primaryEmail: "ada@example.com",
    role: "owner",
    active: true,
    lastSeenAt: "2026-08-08T08:00:00Z",
  },
  {
    id: "user:u2",
    displayName: "Grace Hopper",
    primaryEmail: "grace@example.com",
    role: "admin",
    active: false,
    lastSeenAt: "2026-07-30T08:00:00Z",
  },
];

test("the table's user rows really are identity user rows", () => {
  assertRowsMatchConcept(USER, USERS, assert);
});

test("table renders users through their own display card", () => {
  const fit = fitElement(TABLE_ELEMENT, profileConcept(conceptLike(USER), USERS));
  assert.deepEqual(boundFields(fit, "column").slice(0, 4), [
    "displayName",
    "role",
    "primaryEmail",
    "active",
  ]);
  const html = renderToHtml(renderTable(USERS, conceptLike(USER)));
  assert.match(html, /<th class="vk-table-head" data-vk-sort-field="displayName"/);
  assert.match(html, /Ada Lovelace/);
});

// ---------------------------------------------------------------------------
// Timeline -- v1:deployment:deployment and v1:identity:auditEvent
// ---------------------------------------------------------------------------

const DEPLOYMENT = dslConcept("v1:deployment:deployment");
const DEPLOYMENTS = [
  {
    id: "deployment:d1",
    deploymentId: "d1",
    status: "succeeded",
    version: "2026.8.1",
    environment: "staging",
    updatedAt: "2026-08-08T10:15:00Z",
  },
  {
    id: "deployment:d2",
    deploymentId: "d2",
    status: "in_progress",
    version: "2026.8.2",
    environment: "staging",
    updatedAt: "2026-08-08T11:40:00Z",
  },
];

const AUDIT_EVENT = dslConcept("v1:identity:auditEvent");
const AUDIT_EVENTS = [
  {
    id: "auditEvent:a1",
    occurredAt: "2026-08-07T22:03:00Z",
    category: "auth",
    action: "login_succeeded",
    actorEmail: "ada@example.com",
    outcome: "success",
  },
  {
    id: "auditEvent:a2",
    occurredAt: "2026-08-08T06:11:00Z",
    category: "authorization",
    action: "role_changed",
    actorEmail: "ada@example.com",
    outcome: "success",
  },
];

test("the timeline's sample rows really are deployment and auditEvent rows", () => {
  assertRowsMatchConcept(DEPLOYMENT, DEPLOYMENTS, assert);
  assertRowsMatchConcept(AUDIT_EVENT, AUDIT_EVENTS, assert);
});

test("timeline finds each concept's own time field", () => {
  const deploymentFit = fitElement(
    TIMELINE_ELEMENT,
    profileConcept(conceptLike(DEPLOYMENT), DEPLOYMENTS),
  );
  assert.equal(deploymentFit.verdict, "full");
  assert.equal(boundField(deploymentFit, "at"), "updatedAt");
  assert.equal(boundField(deploymentFit, "label"), "version");
  assert.equal(boundField(deploymentFit, "status"), "status");

  const auditFit = fitElement(
    TIMELINE_ELEMENT,
    profileConcept(conceptLike(AUDIT_EVENT), AUDIT_EVENTS),
  );
  assert.equal(auditFit.verdict, "full");
  assert.equal(boundField(auditFit, "at"), "occurredAt");
  assert.equal(boundField(auditFit, "label"), "action");
  assert.equal(boundField(auditFit, "detail"), "category");
  assert.equal(boundField(auditFit, "status"), "outcome");
});

test("timeline renders newest first, grouped by date", () => {
  const html = renderToHtml(renderTimeline(DEPLOYMENTS, conceptLike(DEPLOYMENT)));
  assert.ok(
    html.indexOf("2026.8.2") < html.indexOf("2026.8.1"),
    "the later deployment must come first",
  );
  assert.match(html, /vk-timeline-date">2026-08-08</);
  assert.match(html, /11:40/);
  assert.match(html, /data-status="in_progress"/);
});

// ---------------------------------------------------------------------------
// Charts + stat tiles -- v1:observability:codeMetric
// ---------------------------------------------------------------------------

const CODE_METRIC = dslConcept("v1:observability:codeMetric");
const METRICS = [
  {
    id: "codeMetric:m1",
    codeReference: "method:component/auth.(*Handler).Login",
    windowStart: "2026-08-08T10:00:00Z",
    windowEnd: "2026-08-08T11:00:00Z",
    bucket: "1h",
    callCount: 1240,
    errorCount: 12,
    errorRate: 0.0097,
    p50DurationNs: 410000,
    p95DurationNs: 2400000,
  },
  {
    id: "codeMetric:m2",
    codeReference: "method:component/auth.(*Handler).Login",
    windowStart: "2026-08-08T11:00:00Z",
    windowEnd: "2026-08-08T12:00:00Z",
    bucket: "1h",
    callCount: 980,
    errorCount: 3,
    errorRate: 0.0031,
    p50DurationNs: 380000,
    p95DurationNs: 1900000,
  },
  {
    id: "codeMetric:m3",
    codeReference: "method:component/auth.(*Handler).Login",
    windowStart: "2026-08-08T12:00:00Z",
    windowEnd: "2026-08-08T12:01:00Z",
    bucket: "1m",
    callCount: 21,
    errorCount: 0,
    errorRate: 0,
    p50DurationNs: 350000,
    p95DurationNs: 1700000,
  },
];

test("the chart's sample rows really are codeMetric rows", () => {
  assertRowsMatchConcept(CODE_METRIC, METRICS, assert);
});

test("line chart plots one measure against the concept's time column", () => {
  const fit = fitElement(
    LINE_CHART_ELEMENT,
    profileConcept(conceptLike(CODE_METRIC), METRICS),
  );
  assert.equal(fit.verdict, "full");
  assert.equal(boundField(fit, "x"), "windowStart");
  // One series automatically: plotting callCount and errorRate on one axis
  // would be the dual-axis mistake in disguise.
  assert.deepEqual(boundFields(fit, "y"), ["callCount"]);

  const html = renderToHtml(renderLineChart(METRICS, conceptLike(CODE_METRIC)));
  assert.match(html, /<svg class="vk-chart"/);
  assert.match(html, /role="img" aria-label="Line chart of callCount over windowStart/);
  assert.match(html, /class="vk-chart-line vk-chart-s1"/);
  assert.match(html, /<title>callCount 1,240 at 2026-08-08 10:00<\/title>/);
  // The direct label -- the relief that keeps a sub-3:1 light-mode hue legible.
  assert.match(html, /class="vk-chart-value"[^>]*>callCount 21</);
});

test("a multi-series line is an explicit choice, and gets a legend", () => {
  const html = renderToHtml(
    renderLineChart(METRICS, conceptLike(CODE_METRIC), {
      bindings: { y: ["callCount", "errorCount"] },
    }),
  );
  assert.match(html, /vk-chart-s1/);
  assert.match(html, /vk-chart-s2/);
  assert.match(html, /class="vk-chart-legend"/);
  assert.match(html, /vk-chart-legend-label">errorCount</);
});

test("bar chart buckets by the concept's status slot and labels every bar", () => {
  const fit = fitElement(
    BAR_CHART_ELEMENT,
    profileConcept(conceptLike(CODE_METRIC), METRICS),
  );
  assert.equal(boundField(fit, "category"), "bucket");
  assert.equal(boundField(fit, "value"), "callCount");

  const html = renderToHtml(renderBarChart(METRICS, conceptLike(CODE_METRIC)));
  // Every bar is slot 1: length already encodes the value.
  assert.equal(html.match(/vk-chart-bar vk-chart-s1/g)?.length, 2);
  assert.ok(!html.includes("vk-chart-bar vk-chart-s2"));
  assert.match(html, /<title>1h: 2,220 callCount<\/title>/);
  assert.match(html, /class="vk-chart-value"[^>]*>2\.2k</);
});

test("pie chart labels each slice with its share and a legend entry", () => {
  const html = renderToHtml(renderPieChart(METRICS, conceptLike(CODE_METRIC)));
  assert.match(html, /class="vk-chart-slice vk-chart-s1"/);
  assert.match(html, /class="vk-chart-slice vk-chart-s2"/);
  assert.match(html, /vk-chart-legend-label">1h</);
  assert.match(html, /99\.1%/);
});

test("a chart folds past the palette rather than inventing a seventh hue", () => {
  const rows = Array.from({ length: 9 }, (_, i) => ({
    id: `r${i}`,
    kind: `kind-${i}`,
    n: 9 - i,
  }));
  const html = renderToHtml(
    renderPieChart(rows, { id: "x", entity: "x" }, { bindings: { category: "kind", value: "n" } }),
  );
  assert.match(html, /vk-chart-other/);
  assert.match(html, /vk-chart-legend-label">Other</);
  assert.ok(!html.includes("vk-chart-s7"), "there is no seventh slot");
});

test("stat tiles total the numeric fields and always show the row count", () => {
  const fit = fitElement(
    STAT_TILE_ELEMENT,
    profileConcept(conceptLike(CODE_METRIC), METRICS),
  );
  assert.deepEqual(boundFields(fit, "metric"), ["callCount", "errorCount", "errorRate"]);

  const html = renderToHtml(renderStatTiles(METRICS, conceptLike(CODE_METRIC)));
  assert.match(html, /vk-stat-value">3<\/div><div class="vk-stat-label">codeMetric rows/);
  assert.match(html, /vk-stat-value">2,241<\/div><div class="vk-stat-label">callCount total/);
  assert.match(html, /avg 747 over 3/);
});

test("stat tiles are the one element that renders an empty result", () => {
  const html = renderToHtml(renderStatTiles([], conceptLike(CODE_METRIC)));
  assert.match(html, /vk-stat-value">0</);
});

// ---------------------------------------------------------------------------
// Map -- nothing in the tree carries coordinates yet
// ---------------------------------------------------------------------------

test("no concept in the tree satisfies the map's coordinate requirement", () => {
  const fitting = loadAllDslConcepts()
    .filter((concept) => {
      const profile = profileConcept(conceptLike(concept), [syntheticRow(concept)]);
      return fitElement(MAP_ELEMENT, profile).verdict !== "unfit";
    })
    .map((concept) => concept.id);
  assert.deepEqual(
    fitting,
    [],
    `these concepts now carry coordinates: ${fitting.join(", ")}. That is good ` +
      `news -- point the map's test rows at one of them and assert the render, ` +
      `then delete this sweep. Until then it pins the honest state: the map ` +
      `declares what it needs, nothing supplies it, and no view picker offers it.`,
  );
});

test("the map explains its own absence in words", () => {
  const profile = profileConcept(conceptLike(CALENDAR_EVENT), EVENTS);
  const fit = fitElement(MAP_ELEMENT, profile);
  assert.equal(fit.verdict, "unfit");
  const words = explainFit(MAP_ELEMENT, fit, profile);
  assert.match(words, /Map cannot render calendarEvent\./);
  assert.match(words, /north-south coordinate/);
  assert.match(words, /none named latitude, lat/);
});

test("the map renders once a concept does carry coordinates", () => {
  // Not a concept in the tree -- this is the map's own behaviour under the
  // shape it is waiting for, so the element is exercised rather than only
  // reported unfit.
  const rows = [
    { id: "s1", name: "Reykjavik", latitude: 64.15, longitude: -21.94 },
    { id: "s2", name: "Wellington", latitude: -41.29, longitude: 174.78 },
  ];
  const html = renderToHtml(renderMap(rows, { id: "x", entity: "site" }));
  assert.match(html, /class="vk-map-point vk-chart-s1"/);
  assert.match(html, /<title>Reykjavik \(64\.15, -21\.94\)<\/title>/);
  assert.match(html, /aria-label="Map of 2 site rows by latitude and longitude\."/);
});

// ---------------------------------------------------------------------------
// The library, over the whole tree
// ---------------------------------------------------------------------------

test("every concept in the tree gets at least one element", () => {
  for (const concept of loadAllDslConcepts()) {
    const profile = profileConcept(conceptLike(concept), [syntheticRow(concept)]);
    const fits = fitElements(VIEW_KIT_ELEMENTS, profile).filter(
      (fit) => fit.verdict !== "unfit",
    );
    assert.ok(
      fits.length > 0,
      `${concept.id} has no element at all, which should be impossible -- the ` +
        `row list requires nothing of a concept.`,
    );
  }
});

test("no element throws on any concept in the tree", () => {
  const concepts = loadAllDslConcepts();
  assert.ok(concepts.length > 40, `only ${concepts.length} concepts parsed; the reader broke`);
  for (const concept of concepts) {
    const rows = [syntheticRow(concept)];
    for (const element of VIEW_KIT_ELEMENTS) {
      const node = renderElement(element, rows, conceptLike(concept));
      if (node === undefined) continue;
      assert.equal(
        typeof renderToHtml(node),
        "string",
        `${element.id} failed to render ${concept.id}`,
      );
    }
  }
});

// ---------------------------------------------------------------------------
// Geometry
// ---------------------------------------------------------------------------

// An SVG root clips its own overflow, so a mark or a label outside the viewBox
// is simply invisible -- no error, no warning, just a missing slice label.
// This is the automated stand-in for opening the chart and looking at it.
function assertInsideViewBox(html: string, label: string): void {
  const box = /viewBox="0 0 (\d+) (\d+)"/.exec(html);
  assert.ok(box, `${label}: no viewBox`);
  const [w, h] = [Number(box[1]), Number(box[2])];
  const xs: number[] = [];
  const ys: number[] = [];
  for (const m of html.matchAll(/\s(cx|x1|x2|x)="(-?[\d.]+)"/g)) xs.push(Number(m[2]));
  for (const m of html.matchAll(/\s(cy|y1|y2|y)="(-?[\d.]+)"/g)) ys.push(Number(m[2]));
  for (const m of html.matchAll(/[ML](-?[\d.]+),(-?[\d.]+)/g)) {
    xs.push(Number(m[1]));
    ys.push(Number(m[2]));
  }
  assert.ok(xs.length > 0 && ys.length > 0, `${label}: no coordinates found`);
  for (const x of xs) {
    assert.ok(x >= -1 && x <= w + 1, `${label}: x=${x} is outside the ${w}-wide viewBox`);
  }
  for (const y of ys) {
    assert.ok(y >= -1 && y <= h + 1, `${label}: y=${y} is outside the ${h}-tall viewBox`);
  }
}

test("every chart keeps its marks and labels inside the viewBox", () => {
  const concept = conceptLike(CODE_METRIC);
  assertInsideViewBox(renderToHtml(renderBarChart(METRICS, concept)), "bar");
  assertInsideViewBox(renderToHtml(renderLineChart(METRICS, concept)), "line");
  assertInsideViewBox(renderToHtml(renderPieChart(METRICS, concept)), "pie");
  // The pie's share labels sit outside the arc; a near-even split puts one at
  // each compass point, which is the case that used to clip.
  const even = Array.from({ length: 4 }, (_, i) => ({ id: `e${i}`, kind: `kind-${i}`, n: 25 }));
  assertInsideViewBox(
    renderToHtml(
      renderPieChart(even, { id: "x", entity: "x" }, { bindings: { category: "kind", value: "n" } }),
    ),
    "even pie",
  );
  assertInsideViewBox(
    renderToHtml(
      renderMap(
        [
          { id: "n", latitude: 89.9, longitude: -179.9 },
          { id: "s", latitude: -89.9, longitude: 179.9 },
        ],
        { id: "x", entity: "site" },
      ),
    ),
    "map",
  );
});

test("a chart of one category is still a chart", () => {
  // The degenerate cases: one bar, one point, one slice. A full-circle arc in
  // particular collapses to nothing if it is drawn as a single arc segment.
  const one = [{ id: "a", kind: "only", n: 5, at: "2026-08-08T10:00:00Z" }];
  const concept = { id: "x", entity: "x" };
  assert.match(renderToHtml(renderBarChart(one, concept)), /vk-chart-bar/);
  assert.match(renderToHtml(renderLineChart(one, concept)), /vk-chart-dot/);
  const pie = renderToHtml(renderPieChart(one, concept));
  assert.match(pie, /vk-chart-slice/);
  assert.match(pie, /A80,80 0 1 1/, "a lone slice is two half-arcs, not a zero-length one");
  assert.match(pie, /100%/);
});

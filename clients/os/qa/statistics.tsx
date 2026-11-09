import { createRoot } from "react-dom/client";
import { useState, type ReactNode } from "react";

import { ScopeControl } from "../src/apps/accounts/ScopeControl";
import { Head, Subhead } from "../src/kit";
import { Overview } from "../src/kit/Overview";
import { absent, figureOf } from "../src/kit/measure";
import {
  PeriodControl,
  RankedList,
  SITE_PERIODS,
  StatFigures,
  TimeChart,
  type ChartPoint,
  type Period,
  type RankedRow,
  type StatFigure,
} from "../src/kit/statistics";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// The statistics kit, rendered (DESIGN.md: "the acceptance for any surface
// change ... is rendered screenshots, both modes, empty and populated").
//
//   npx vite --config qa/vite.config.ts --port 5199 --strictPort
//   http://localhost:5199/statistics.html?mode=dark
//
// `mode=light|dark`, `theme=graphite|vellum|cobalt`, `width=narrow` (390px),
// `view=page` (a statistics page: figures, chart, ranked lists, Health),
// `view=overview` (an Overview whose figures are statistics), `view=charts`
// (every chart state: gaps, hours, twelve months, a rate, one point, nothing)
// and `view=states` (reading and empty). Fixtures only; nothing is read.

const params = new URLSearchParams(location.search);
const mode = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.theme = mode;
document.documentElement.dataset.osTheme = params.get("theme") ?? "graphite";
applyPackStyles([]);
const narrow = params.get("width") === "narrow";
const view = params.get("view") ?? "page";

const count = (value: number) => value.toLocaleString("en-US");
const percent = (value: number) => `${Math.round(value * 100)}%`;
const DAY = 86_400_000;
const HOUR = 3_600_000;

function series(start: string, length: number, step: number, value: (index: number) => number | null): ChartPoint[] {
  const base = Date.parse(start);
  return Array.from({ length }, (_, index) => ({ at: new Date(base + index * step).toISOString(), value: value(index) }));
}

function months(length: number, value: (index: number) => number | null): ChartPoint[] {
  return Array.from({ length }, (_, index) => ({ at: new Date(Date.UTC(2025, 10 + index, 1)).toISOString(), value: value(index) }));
}

/** A week-shaped wave: quieter weekends, a slow climb. */
function wave(index: number, base: number, swing: number): number {
  const weekend = index % 7 === 5 || index % 7 === 6 ? -base * 0.35 : 0;
  return Math.max(0, Math.round(base + swing * Math.sin(index / 1.3) + weekend + index * base * 0.01));
}

const SERIES: Record<string, { points: ChartPoint[]; previous: ChartPoint[]; format: (value: number) => string }> = {
  visitors: { points: series("2026-10-10T00:00:00Z", 30, DAY, (i) => (i < 5 ? null : wave(i, 420, 60))), previous: series("2026-09-10T00:00:00Z", 30, DAY, (i) => wave(i + 3, 360, 50)), format: count },
  visits: { points: series("2026-10-10T00:00:00Z", 30, DAY, (i) => (i < 5 ? null : wave(i, 530, 70))), previous: series("2026-09-10T00:00:00Z", 30, DAY, (i) => wave(i + 3, 470, 60)), format: count },
  views: { points: series("2026-10-10T00:00:00Z", 30, DAY, (i) => (i < 5 ? null : wave(i, 1480, 210))), previous: series("2026-09-10T00:00:00Z", 30, DAY, (i) => wave(i + 3, 1300, 180)), format: count },
  bounce: { points: series("2026-10-10T00:00:00Z", 30, DAY, (i) => (i < 5 ? null : 0.38 + 0.06 * Math.sin(i / 2))), previous: series("2026-09-10T00:00:00Z", 30, DAY, (i) => 0.42 + 0.05 * Math.sin(i / 2)), format: percent },
};

const FIGURES: StatFigure[] = [
  { id: "visitors", label: "Visitors", figure: figureOf(12_044), previous: figureOf(10_753), chartable: true },
  { id: "visits", label: "Visits", figure: figureOf(15_310), previous: figureOf(15_290), chartable: true },
  { id: "views", label: "Page views", figure: figureOf(42_117), previous: figureOf(45_870), chartable: true },
  { id: "perVisit", label: "Views per visit", figure: figureOf(2.75), previous: figureOf(3), format: (value) => value.toFixed(1) },
  { id: "bounce", label: "Bounce rate", figure: figureOf(0.38), previous: figureOf(0.42), format: percent, chartable: true },
];

const HEALTH: StatFigure[] = [
  { id: "requests", label: "Requests", figure: figureOf(188_410), previous: figureOf(171_002), chartable: true },
  { id: "errors", label: "Errors", figure: figureOf(14), previous: figureOf(0), chartable: true },
  { id: "notFound", label: "Not found", figure: figureOf(1_203), previous: figureOf(1_410), chartable: true },
  { id: "p95", label: "Response time, p95", figure: absent("unmeasured"), previous: figureOf(120), format: (ms) => `${Math.round(ms)} ms`, chartable: true },
];

const PAGES: RankedRow[] = ["/", "/pricing", "/collections/winter", "/products/wool-scarf", "/blog/how-we-dye-our-yarn-with-plants-and-patience", "/about", "/contact", "/products/beanie", "/faq", "/shipping", "/returns", "/careers"]
  .map((label, index) => ({ key: label, label, value: Math.round(4200 / (index + 1.2)) }));
const SOURCES: RankedRow[] = [["Direct", 5120], ["search.example", 3311], ["social.example", 1208], ["newsletter", 640], ["news.example", 92]]
  .map(([label, value]) => ({ key: String(label), label: String(label), value: Number(value) }));
const COUNTRIES: RankedRow[] = [["Germany", 3920], ["United States", 2811], ["France", 1402], ["Netherlands", 988], ["Unknown", 120]]
  .map(([label, value]) => ({ key: String(label), label: String(label), value: Number(value) }));
const CAMPAIGNS: RankedRow[] = [["Winter launch", 1204, "42% opened · 3.1% clicked"], ["Restock alert", 803, "38% opened · 5.4% clicked"], ["Holiday hours", 211, "29% opened"]]
  .map(([label, value, secondary]) => ({ key: String(label), label: String(label), value: Number(value), secondary: String(secondary), onOpen: () => {} }));

const credit = <a href="https://db-ip.com" target="_blank" rel="noreferrer">IP Geolocation by DB-IP</a>;

function Pair({ children }: { children: ReactNode }) {
  return <div style={{ display: "grid", gridTemplateColumns: narrow ? "minmax(0, 1fr)" : "repeat(2, minmax(0, 1fr))", gap: narrow ? 24 : 40 }}>{children}</div>;
}

function Page() {
  const [period, setPeriod] = useState<Period>("30d");
  const [selected, setSelected] = useState("visitors");
  const [health, setHealth] = useState("requests");
  const chart = SERIES[selected] ?? SERIES["visitors"]!;
  return <>
    <Head title="Statistics"><PeriodControl id="qa-period" value={period} periods={SITE_PERIODS} onChange={setPeriod} /></Head>
    <StatFigures label="Website figures" figures={FIGURES} selected={selected} onSelect={setSelected} />
    <TimeChart label={FIGURES.find((figure) => figure.id === selected)?.label ?? "Visitors"} bucket="day" points={chart.points} previous={chart.previous} format={chart.format} />
    <Pair>
      <RankedList title="Pages" rows={PAGES} onShowAll={() => {}} showAllLabel="All pages" />
      <RankedList title="Sources" rows={SOURCES} onShowAll={() => {}} showAllLabel="All sources" />
    </Pair>
    <Pair>
      <RankedList title="Countries" rows={COUNTRIES} footer={credit} onShowAll={() => {}} showAllLabel="All countries" />
      <RankedList title="Devices" rows={[]} empty="Nothing in this period." />
    </Pair>
    <Subhead>Health</Subhead>
    <StatFigures label="Health figures" figures={HEALTH} selected={health} onSelect={setHealth} />
    <TimeChart label="Requests" bucket="day" points={SERIES["views"]!.points} previous={SERIES["views"]!.previous} format={count} />
  </>;
}

function OverviewView() {
  const [selected, setSelected] = useState("sent");
  const [client, setClient] = useState("");
  // A staff member's scope (spec D13), built here rather than read: the
  // control draws whatever scope it is handed.
  const scope = {
    showControl: true,
    value: client,
    options: [{ value: "", label: "All clients" }, { value: "a", label: "First client" }, { value: "b", label: "Second client" }],
    accountIds: client === "" ? [] : [client],
    set: setClient,
  };
  const figures: StatFigure[] = [
    { id: "sent", label: "Sent", figure: figureOf(48_120), previous: figureOf(41_009), chartable: true },
    { id: "openRate", label: "Open rate", figure: figureOf(0.42), previous: figureOf(0.4), format: percent, chartable: true },
    { id: "clickRate", label: "Click rate", figure: figureOf(0.031), previous: figureOf(0.031), format: (v) => `${(v * 100).toFixed(1)}%`, chartable: true },
    { id: "unsubscribes", label: "Unsubscribes", figure: figureOf(38), previous: figureOf(51), chartable: true },
    { id: "subscribes", label: "New subscribers", figure: figureOf(212), previous: figureOf(0), chartable: true },
  ];
  return <Overview
    figures={<StatFigures label="Email figures" figures={figures} selected={selected} onSelect={setSelected} />}
    actions={<><ScopeControl id="qa-scope" scope={scope} /><PeriodControl id="qa-email-period" value="30d" periods={["7d", "30d", "90d", "12m"]} onChange={() => {}} /></>}
  >
    <TimeChart label="Sent" bucket="day" points={SERIES["views"]!.points} previous={SERIES["views"]!.previous} format={count} />
    <RankedList title="Top campaigns" rows={CAMPAIGNS} />
  </Overview>;
}

function Charts() {
  const year = months(13, (index) => (index < 2 ? null : 9000 + index * 820 + (index % 3) * 640));
  const hours = series("2026-11-07T15:00:00Z", 25, HOUR, (index) => Math.max(0, Math.round(40 + 30 * Math.sin((index - 6) / 3.8))));
  const hoursBefore = series("2026-11-06T15:00:00Z", 25, HOUR, (index) => Math.max(0, Math.round(34 + 26 * Math.sin((index - 5) / 3.8))));
  const rate = series("2026-11-02T00:00:00Z", 7, DAY, (index) => [0.41, 0.38, 0.45, null, 0.36, 0.44, 0.39][index] ?? null);
  return <>
    <Head title="Charts" />
    <Subhead>Twenty-four hours</Subhead>
    <TimeChart label="Page views" bucket="hour" points={hours} previous={hoursBefore} format={count} />
    <Subhead>Twelve months, no previous period</Subhead>
    <TimeChart label="Visitors" bucket="month" points={year} format={count} />
    <Subhead>A rate with a gap</Subhead>
    <TimeChart label="Bounce rate" bucket="day" points={rate} format={percent} />
    <Subhead>The first day counted</Subhead>
    <TimeChart label="Visitors" bucket="day" points={series("2026-11-02T00:00:00Z", 7, DAY, (index) => (index === 6 ? 31 : null))} format={count} />
    <Subhead>Nothing measured</Subhead>
    <TimeChart label="Visitors" bucket="day" points={series("2026-11-02T00:00:00Z", 7, DAY, () => null)} format={count} />
  </>;
}

function States() {
  const reading = FIGURES.map((figure) => ({ ...figure, loading: true }));
  return <>
    <Head title="Reading" />
    <StatFigures label="Website figures" figures={reading} selected="visitors" onSelect={() => {}} />
    <Pair>
      <RankedList title="Pages" rows={[]} loading />
      <RankedList title="Sources" rows={[]} empty="Nothing in this period." />
    </Pair>
  </>;
}

function Preview() {
  return <main className="os-window-content os-app-stack" style={{ maxWidth: narrow ? 390 : 1040, margin: "24px auto", background: "var(--os-glass-solid)", height: "calc(100dvh - 48px)", overflow: "auto" }}>
    {view === "overview" ? <OverviewView /> : view === "charts" ? <Charts /> : view === "states" ? <States /> : <Page />}
  </main>;
}

createRoot(document.getElementById("root")!).render(<Preview />);
requestAnimationFrame(() => requestAnimationFrame(() => {
  document.title = "loaded";
}));

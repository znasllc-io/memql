import type { ReactNode } from "react";
import { InlineSkeleton } from "./ContentSkeleton";
import { Head } from "./index";
import { Measure } from "./MeasureView";
import type { Figure } from "./measure";

export interface OverviewMetric {
  label: string;
  figure: Figure;
  detail?: string;
  /**
   * How a measured value is written, when it is not a plain count -- Nexus's
   * "4 to 2" is one figure with its counterpart beside it. An absent figure
   * ignores it and draws the em dash every absent value draws.
   */
  format?: (value: number) => string;
  /**
   * The read behind this figure has not answered yet: its SHAPE is drawn, never
   * a caption (DESIGN.md, "Loading is the shape of the content"). Distinct from
   * an absent figure, which is an answer -- "nothing has reported this".
   */
  loading?: boolean;
}

/** App summaries share a layout; each app supplies its own measured facts. */
export function Overview({ metrics, scope, children, actions }: {
  metrics: readonly OverviewMetric[];
  scope?: string;
  children: ReactNode;
  actions?: ReactNode;
}) {
  return <section className="os-overview os-app-stack" aria-label="Overview" data-os-page-context={JSON.stringify({ page: "Overview", scope })}>
    <Head title="Overview" meta={scope}>{actions}</Head>
    <dl className="os-overview-metrics" aria-label="Overview statistics">
      {metrics.map(metric => <div key={metric.label} data-overview-metric={metric.label}>
        <dt>{metric.label}</dt><dd>{metric.loading ? <InlineSkeleton label={`Loading ${metric.label.toLowerCase()}`} /> : <Measure figure={metric.figure} format={metric.format} />}</dd>
        {metric.detail && !metric.loading ? <small>{metric.detail}</small> : null}
      </div>)}
    </dl>
    {children}
  </section>;
}

/**
 * `unknown` is an ABSENT answer rather than a value -- things nothing has
 * classified yet. It draws as a hatch and a hollow dot, the way the Nexus
 * kind band draws its unclassified steps, so it can never be read as one of
 * the classes beside it.
 */
export interface OverviewSegment { label: string; count: number; tone?: "good" | "warn" | "quiet" | "unknown" }

/** A current distribution, never a fabricated activity history. */
export function OverviewBreakdown({ title, segments }: { title: string; segments: readonly OverviewSegment[] }) {
  const visible = segments.filter(s => s.count > 0);
  const total = visible.reduce((sum, s) => sum + s.count, 0);
  if (!total) return null;
  return <section className="os-overview-breakdown" aria-label={title}>
    <h4>{title}</h4>
    <div className="os-overview-distribution" aria-hidden>{visible.map(s => <span key={s.label} data-tone={s.tone ?? "quiet"} style={{ flex: s.count }} />)}</div>
    <ul>{visible.map(s => <li key={s.label} data-tone={s.tone ?? "quiet"}><span aria-hidden />{s.label}<strong>{s.count}</strong></li>)}</ul>
  </section>;
}

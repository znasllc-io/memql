import type { ReactNode } from "react";
import { Head } from "./index";
import { Measure } from "./MeasureView";
import type { Figure } from "./measure";

export interface OverviewMetric {
  label: string;
  figure: Figure;
  detail?: string;
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
        <dt>{metric.label}</dt><dd><Measure figure={metric.figure} /></dd>
        {metric.detail ? <small>{metric.detail}</small> : null}
      </div>)}
    </dl>
    {children}
  </section>;
}

export interface OverviewSegment { label: string; count: number; tone?: "good" | "warn" | "quiet" }

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

import { Button, Caption, Fact, Facts, Head, Notice, Panel, Subhead, formatFreshness, useNow } from "../../kit";
import {
  FAMILY_ORDER,
  absenceSentence,
  compareArms,
  familyTitle,
  formatFigure,
  formatSpread,
  markHeight,
  trendSummary,
  type Figure,
  type Trend,
} from "./benchmarks";
import { useBenchmarks } from "./useBenchmarks";

/**
 * Benchmarks (epic memql#4993). What the platform measures about itself, and
 * what it does not.
 *
 * THE DESIGN IDEA, in one sentence: an absence takes the same room as a
 * number. A benchmarks page normally shows big figures and hides the gaps;
 * this one puts "nothing measured this, and here is why" in the slot the
 * figure would have occupied, at the same size, in the same column. That is
 * not decoration -- the epic's standard is that the numbers must be honest
 * before they are good, and a surface that made absences small would be
 * arguing the opposite of the thing it displays.
 *
 * THE PICTURE is a run of marks, one per benchmark run, oldest at the left.
 * A measured run is a filled mark whose height is its value against that
 * metric's own range; an unmeasured run is an OPEN NOTCH sitting on the
 * baseline -- visibly a different kind of mark, not a bar of height zero. A
 * bar of height zero is a measurement of zero, which for durability's headline
 * is the exact opposite of what an absence means.
 *
 * Plain DOM in the OS's own idiom, following TrafficStrip and SendBar: no
 * charting dependency, one hue, and every value reachable in words. `--os-warn`
 * and `--os-error` are never a data series here -- amber is warn and red is
 * error everywhere in this shell, and a regression is said in words rather
 * than painted red.
 */
export function BenchmarksSection() {
  const b = useBenchmarks(true);
  const now = useNow(30_000);

  const byFamily = new Map<string, Trend[]>();
  for (const t of b.trends) {
    const list = byFamily.get(t.family) ?? [];
    list.push(t);
    byFamily.set(t.family, list);
  }

  return (
    <div className="os-settings">
      <Head
        title="Benchmarks"
        meta={b.newest === null ? undefined : `${b.runs.length} run${b.runs.length === 1 ? "" : "s"}`}
      />

      <Panel label="Where the numbers come from">
        <Subhead>Where the numbers come from</Subhead>
        {b.newest === null ? (
          <Caption>
            {b.seeding
              ? "Reading the cluster's benchmark runs."
              : "No benchmark run has been published to this cluster. The proving suite runs on every pull request and writes its figures here; a cluster that has never had one is empty rather than broken."}
          </Caption>
        ) : (
          <Facts>
            <Fact label="Newest run" value={b.newest.startedAt.slice(0, 10)} />
            <Fact label="Commit" value={b.newest.commit} mono />
            <Fact label="Tier" value={b.newest.tier} mono />
            <Fact label="Verdict" value={b.newest.verdict} mono />
            <Fact label="Scenarios" value={String(b.newest.scenarioCount)} />
            <Fact
              label="Corpus"
              value={b.newest.corpus}
              mono
              title="Two runs with different corpus fingerprints measured different things and are not a trend."
            />
            <Fact label="Ran on" value={b.newest.runner || "—"} mono />
            <Fact
              label="Measured against"
              value={b.newest.models.length === 0 ? "—" : b.newest.models.join(", ")}
              mono
              title="A CI run replays a recorded tape. Where the tape says `synthetic` its responses are placeholders: the counts are facts about the plan and the platform, and nothing model-dependent is published from them."
            />
          </Facts>
        )}
        <Caption>
          Every figure is a median with its spread and its N, stamped with the commit it came from.
          {b.readAt === "" ? " " : ` Figures read ${formatFreshness(b.readAt, now)}. `}
          <Button onClick={b.reload} busy={b.loadingSamples} busyLabel="Reading">
            Look again
          </Button>
        </Caption>
      </Panel>

      {b.error === "" ? null : (
        <Notice
          tone="warn"
          sentence="The cluster refused the read."
          detail={b.error}
          next="Benchmark rows are cluster-owner tier. If this says the read returned nothing, the account you are signed in as is below that floor."
        />
      )}

      {FAMILY_ORDER.map((family) => {
        const trends = byFamily.get(family);
        if (trends === undefined || trends.length === 0) return null;
        return (
          <Panel key={family} label={familyTitle(family)}>
            <Subhead>{familyTitle(family)}</Subhead>
            <ol className="os-bench-list">
              {trends.map((t) => (
                <MetricLine key={t.metric} trend={t} />
              ))}
            </ol>
          </Panel>
        );
      })}
    </div>
  );
}

/**
 * One metric: its two rails, its newest figures, and -- where there is no
 * figure -- the sentence saying why, in the number's own place.
 */
function MetricLine({ trend }: { trend: Trend }) {
  const platform = newestOf(trend.platform);
  const baseline = newestOf(trend.baseline);
  const comparison = compareArms(trend);

  return (
    <li className="os-bench-metric">
      <p className="os-bench-name os-mono">{trend.metric}</p>
      <div className="os-bench-arms">
        <ArmRow label="Platform" series={trend.platform} figure={platform} unit={trend.unit} accent />
        <ArmRow label="Bare loop" series={trend.baseline} figure={baseline} unit={trend.unit} accent={false} />
      </div>
      {comparison === "" ? null : <p className="os-bench-compare">{comparison}</p>}
      <p className="os-sr-only">{trendSummary(trend)}</p>
    </li>
  );
}

function newestOf(series: readonly (Figure | null)[]): Figure | null {
  for (let i = series.length - 1; i >= 0; i -= 1) {
    if (series[i] !== null) return series[i]!;
  }
  return null;
}

function ArmRow({
  label,
  series,
  figure,
  unit,
  accent,
}: {
  label: string;
  series: readonly (Figure | null)[];
  figure: Figure | null;
  unit: string;
  accent: boolean;
}) {
  return (
    <div className="os-bench-arm">
      <span className="os-bench-arm-label">{label}</span>
      <Rail series={series} accent={accent} />
      <span className="os-bench-value">
        {figure === null ? (
          // Not published by any run shown. Different again from published-as-
          // unmeasured, and it says which.
          <span className="os-bench-absent">— This arm published no figure.</span>
        ) : figure.median === null ? (
          <span className="os-bench-absent">— {absenceSentence(figure.absent, figure.detail)}</span>
        ) : (
          <>
            <strong className="os-bench-number">{formatFigure(figure.median, unit)}</strong>
            <span className="os-bench-spread">
              {formatSpread(figure) === "" ? "" : `${formatSpread(figure)} · `}
              N={figure.n ?? 0}
            </span>
          </>
        )}
      </span>
    </div>
  );
}

/**
 * The run of marks. `role="img"` with a label, because a shape a screen reader
 * announces as a list of empty items is worse than no shape at all -- the
 * prose summary beside it carries the values.
 */
function Rail({ series, accent }: { series: readonly (Figure | null)[]; accent: boolean }) {
  return (
    <ol className="os-bench-rail" data-os-bench-accent={accent ? "on" : "off"} role="img" aria-label={railLabel(series)}>
      {series.map((f, i) => {
        const kind = f === null ? "gap" : f.median === null ? "absent" : "measured";
        const height = f?.median === null || f === null ? 0 : markHeight(f.median, series);
        return (
          <li
            key={i}
            className="os-bench-mark"
            data-os-bench-mark={kind}
            title={f === null ? "not published by this run" : f.median === null ? absenceSentence(f.absent, "") : `${f.measuredOn} · ${f.commit}`}
          >
            <span className="os-bench-fill" style={{ height: `${Math.round(height * 100)}%` }} aria-hidden />
          </li>
        );
      })}
    </ol>
  );
}

function railLabel(series: readonly (Figure | null)[]): string {
  const measured = series.filter((f) => f !== null && f.median !== null).length;
  const absent = series.filter((f) => f !== null && f.median === null).length;
  return `${series.length} runs: ${measured} measured, ${absent} unmeasured`;
}

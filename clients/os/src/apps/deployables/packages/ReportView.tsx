import { AlertTriangle, Box, FileCode2, Package as PackageIcon, Zap } from "lucide-react";

import { Chip, Chips, Notice } from "../../../kit";
import { copyFor } from "./refusals";
import type { AnalysisReport, ReportProblem } from "./rows";

// The analysis report: what deploying this package would do.
//
// ===========================================================================
// THIS IS THE THING A PERSON READS BEFORE SAYING YES
// ===========================================================================
// The confirm gate is always present (design D12), and this is what stands
// behind it. So the report is not a diagnostic dump -- it is the answer to one
// question, asked in the order somebody asks it: what web apps will exist,
// what will this add to the cluster, and what is wrong with any of it.
//
// Two things it deliberately does NOT do:
//
//   * It never summarises a refusal. The server's sentence names the path, the
//     domain or the construct, and that is the fact that helps; this surface
//     supplies the headline and prints the sentence underneath, verbatim.
//   * It never hides the Go pack. A `bff/` that silently did not deploy is how
//     somebody concludes the platform ran their Go. It is reported, in place,
//     as a thing that was left out on purpose.

export function ReportView({ report }: { report: AnalysisReport | null }) {
  if (report === null) {
    return <p className="os-caption">No analysis has run for this deployment yet.</p>;
  }

  const deployables = report.deployables ?? [];
  const domains = (report.dslDomains ?? []).filter((d) => d.reserved !== true);
  const goPacks = report.goPacks ?? [];
  const problems = report.problems ?? [];
  const blocking = problems.filter((p) => p.fatal);
  // A PROBLEM ALREADY SHOWN ON ITS APP IS NOT SHOWN AGAIN. `rep.add` records
  // every problem on the report AND the per-deployable ones on their own
  // deployable, so a non-fatal problem scoped to an app is in both places --
  // and until `deployable_target_not_offered` arrived (epic memql#4885) the
  // only non-fatal problem was the Go pack's, which this block already
  // suppressed by hand with `goPacks.length === 0`. The new one printed
  // "iOS is not offered on this cluster yet" twice on one screen: once inside
  // the mobile app's card, where it belongs, and once at the foot of the
  // report under the MemQL heading, which is about DSL domains and has nothing
  // to say about an iOS app. Found by looking at the rendered page; the diff
  // showed a filter that had been correct for its whole previous life.
  //
  // Keyed on (code, scope) rather than on the object, because the report is
  // JSON off a row and the two copies are separate values.
  const shownOnAnApp = new Set(
    (report.deployables ?? [])
      .filter((d) => d.problem !== undefined)
      .map((d) => `${d.problem?.code}|${d.problem?.scope ?? d.name}`),
  );
  const notes = problems.filter(
    (p) => !p.fatal && !shownOnAnApp.has(`${p.code}|${p.scope ?? ""}`),
  );

  return (
    <div className="os-report">
      {blocking.length > 0 ? (
        <div className="os-report-problems">
          {blocking.map((p, i) => (
            <ProblemNotice key={`${p.code}-${i}`} problem={p} tone="error" />
          ))}
        </div>
      ) : null}

      <section className="os-report-part">
        <h4 className="os-report-heading">
          <Box size={12} aria-hidden /> Web apps
        </h4>
        {deployables.length === 0 ? (
          <p className="os-caption">This package declares no web apps.</p>
        ) : (
          <ul className="os-report-list">
            {deployables.map((d) => (
              <li key={d.name} className="os-report-item" data-problem={d.problem ? "true" : "false"}>
                <div className="os-report-item-head">
                  <span className="os-report-name">{d.name}</span>
                  <Chip>{d.kind}</Chip>
                  {d.prebuilt ? <Chip tone="accent">already built</Chip> : null}
                </div>
                <p className="os-report-path">{d.path}</p>
                <p className="os-report-plan">{d.buildPlan}</p>
                {d.binding?.storeDomain ? (
                  <p className="os-report-plan">Fronts {d.binding.storeDomain}</p>
                ) : null}
                {d.problem ? <ProblemNotice problem={d.problem} tone="error" /> : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="os-report-part">
        <h4 className="os-report-heading">
          <FileCode2 size={12} aria-hidden /> MemQL this adds
        </h4>
        {domains.length === 0 ? (
          /* Not an absence to apologise for: it is the reason this deploy will
             be fast, and saying so here is what makes the skipped stages on
             the rail read as a design rather than a gap. */
          <p className="os-caption">None. Nothing restarts, and the deploy lands in seconds.</p>
        ) : (
          <ul className="os-report-list">
            {domains.map((d) => (
              <li key={d.domain} className="os-report-item">
                <div className="os-report-item-head">
                  <span className="os-report-name">{d.domain}</span>
                  <span className="os-caption">
                    {d.files} {d.files === 1 ? "file" : "files"}
                  </span>
                </div>
                <Chips label={`What ${d.domain} adds`}>
                  {constructEntries(d.constructs).map(([kind, count]) => (
                    <Chip key={kind}>
                      {count} {plural(kind, count)}
                    </Chip>
                  ))}
                </Chips>
              </li>
            ))}
          </ul>
        )}
      </section>

      {goPacks.length > 0 ? (
        <section className="os-report-part">
          <h4 className="os-report-heading">
            <PackageIcon size={12} aria-hidden /> Go, and where it goes instead
          </h4>
          {goPacks.map((g) => (
            <div key={g.path} className="os-report-item">
              <div className="os-report-item-head">
                <span className="os-report-name">{g.path}</span>
                {g.module ? <span className="os-caption">{g.module}</span> : null}
              </div>
              <p className="os-report-plan">{g.note}</p>
            </div>
          ))}
        </section>
      ) : null}

      {notes.length > 0 && goPacks.length === 0 ? (
        <div className="os-report-problems">
          {notes.map((p, i) => (
            <ProblemNotice key={`${p.code}-${i}`} problem={p} tone="warn" />
          ))}
        </div>
      ) : null}
    </div>
  );
}

/**
 * ProblemNotice renders the server's own sentence, under a headline this build
 * recognises -- or under a neutral one when it does not.
 *
 * Exported because every surface that shows a refusal shows it this way: in
 * place, beside the control that produced it, never as a toast.
 */
export function ProblemNotice({ problem, tone }: { problem: ReportProblem | { code: string; message: string; scope?: string }; tone: "error" | "warn" }) {
  const copy = copyFor(problem.code);
  return (
    <Notice
      tone={tone}
      sentence={copy?.title ?? "This cluster refused"}
      detail={problem.message}
      next={copy?.next === "" ? undefined : copy?.next}
    >
      {problem.scope ? <Chip tone="muted">{problem.scope}</Chip> : null}
    </Notice>
  );
}

/** A build's own output, when there is one. Bounded server-side already. */
export function BuildLog({ tail }: { tail: string }) {
  if (tail.trim() === "") return null;
  return (
    <details className="os-report-log">
      <summary>
        <AlertTriangle size={12} aria-hidden /> Build output
      </summary>
      <pre>{tail}</pre>
    </details>
  );
}

/** The fast-path marker, for a run that skipped the build entirely. */
export function PrebuiltMark() {
  return (
    <span className="os-report-fast">
      <Zap size={11} aria-hidden /> already built
    </span>
  );
}

function constructEntries(constructs: Record<string, number> | undefined): [string, number][] {
  if (!constructs) return [];
  return Object.entries(constructs)
    .filter(([, n]) => n > 0)
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}

function plural(kind: string, count: number): string {
  if (count === 1) return kind;
  if (kind.endsWith("y")) return `${kind.slice(0, -1)}ies`;
  return `${kind}s`;
}

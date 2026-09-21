import { Caption, Fact, Facts } from "../../../../kit";
import { ProblemNotice, ReportView } from "../../packages/ReportView";
import type { DeploymentRow, PackageRow } from "../../packages/rows";
import type { SiteRow } from "../../rows";
import type { RailProblem } from "../rail";

// The What-it-is stop: the verdict, read from the newest run's report.
//
// Read-only, and filled from the report the confirm gate already carried
// (design section C): each app with its kind, path and build plan, each DSL
// domain, a Go pack reported and deferred, and any problem -- a not-offered
// target included, whose sentence the report's own per-app notice renders
// verbatim. While the analysis runs the stop says so in a sentence: the
// rail's ring on this stop is the motion, and a second spinner would be a
// second thing moving on a surface that has exactly one.
//
// A hand-made deployable has no report. The rail's note names its kind; the
// body says only what the row knows beyond that -- a title and notes.
// Whether a zip had index.html at its root is a fact the publish checked and
// the row does not carry, so it is not claimed here.
//
// THE STORE IS NOT A BUILD FACT AND IS NO LONGER SHOWN HERE (epic
// memql#5530). It used to be: two Facts naming the store's domain and the
// secret holding its Storefront token, on the stop that reports what
// DEPLOYING would do. A store is what the deployable is CONNECTED to, not
// something a build decided, and the chip that led here from the workspace
// pointed at the build report for it. Both moved to the Store pane, which is
// where the store is read and changed.

export function WhatItIsStop({
  site,
  run,
  refusal,
}: {
  site: SiteRow;
  pkg: PackageRow | null;
  run: DeploymentRow | null;
  refusal: RailProblem | null;
}) {
  const report = run?.report ?? null;
  const analyzing = run?.status === "analyzing";
  // The report renders its own fatal problems as notices, so a refusal the
  // report already carries is not rendered twice on one stop.
  const reportCarries = refusal !== null && (report?.problems ?? []).some((p) => p.fatal && p.code === refusal.code);
  const facts = site.title !== "" || site.notes !== "";

  if (refusal === null && !analyzing && report === null && !facts) return null;

  return (
    <div className="os-stop-body">
      {refusal && !reportCarries ? <ProblemNotice problem={refusal} tone="error" /> : null}
      {analyzing ? (
        <Caption>Analyzing the source. The build plan will appear here.</Caption>
      ) : report !== null ? (
          <ReportView report={report} only={site.packageDeployableName || undefined} />
      ) : null}
      {facts ? (
        <Facts>
          {site.title === "" ? null : <Fact label="Title" value={site.title} />}
          {site.notes === "" ? null : <Fact label="Notes" value={site.notes} />}
        </Facts>
      ) : null}
    </div>
  );
}

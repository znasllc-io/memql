import { Caption, Fact, Facts } from "../../../../kit";
import { STOREFRONT_KIND } from "../../concepts";
import { ProblemNotice, ReportView } from "../../packages/ReportView";
import type { DeploymentRow, PackageRow } from "../../packages/rows";
import { storefrontBinding, type SiteRow } from "../../rows";
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
// body says only what the row knows beyond that -- a title, notes, and for a
// storefront the store it fronts and the NAME of the secret holding its
// token. Whether a zip had index.html at its root is a fact the publish
// checked and the row does not carry, so it is not claimed here.

export function WhatItIsStop({
  site,
  pkg,
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
  const storefront = pkg === null && site.kind === STOREFRONT_KIND ? storefrontBinding(site) : null;
  const facts = site.title !== "" || site.notes !== "" || storefront !== null;

  if (refusal === null && !analyzing && report === null && !facts) return null;

  return (
    <div className="os-stop-body">
      {refusal && !reportCarries ? <ProblemNotice problem={refusal} tone="error" /> : null}
      {analyzing ? (
        <Caption>Reading the tree now. The verdict lands here when the analysis is done.</Caption>
      ) : report !== null ? (
        <ReportView report={report} />
      ) : null}
      {facts ? (
        <Facts>
          {site.title === "" ? null : <Fact label="Title" value={site.title} />}
          {site.notes === "" ? null : <Fact label="Notes" value={site.notes} />}
          {storefront === null ? null : (
            <>
              <Fact label="Store" value={storefront.storeDomain} mono />
              {/* THE NAME, NEVER THE VALUE. `storefrontTokenRef` NAMES a
                  v1:platform:globalSecret row; the token itself is not on this
                  row and is not fetched here. The edge resolves it at serve
                  time into the site's runtime-config document, and that is the
                  only place it is dereferenced. */}
              <Fact
                label="Storefront token"
                value={storefront.storefrontTokenRef}
                mono
                title="The name of the secret that holds the token. The value is resolved by the edge at serve time and is never read here."
              />
            </>
          )}
        </Facts>
      ) : null}
    </div>
  );
}

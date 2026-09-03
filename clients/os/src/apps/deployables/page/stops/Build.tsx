import { Fact, Facts } from "../../../../kit";
import { BuildLog, ProblemNotice } from "../../packages/ReportView";
import type { DeploymentRow } from "../../packages/rows";
import type { RailProblem } from "../rail";

// The Build stop, in its two readings (design section C).
//
// Prebuilt: the rail's note already says it was skipped and why ("its built
// output is in the source"); the body adds the build plan from the report --
// what the source ships built, and where -- and nothing else. Needs a build:
// the engine's typed refusal, rendered in place with its copy above and the
// server's sentence beneath, VERBATIM, because that sentence names the
// command it would have run and the two ways forward, and a paraphrase would
// drop exactly the half a person acts on. The build output follows it.
//
// The Build epic replaces the second reading with progress on the surface
// that built it, and the log; it changes what this stop says, not where it
// is.

export function BuildStop({
  run,
  app,
  refusal,
}: {
  run: DeploymentRow | null;
  /** The app's name in the manifest; "" for a hand-made deployable. */
  app: string;
  /** The newest run's refusal, when it stopped here. */
  refusal: RailProblem | null;
}) {
  const appReport = app === "" ? null : ((run?.report?.deployables ?? []).find((d) => d.name === app) ?? null);
  const log = run?.buildLogTail ?? "";

  if (refusal === null && appReport === null && log.trim() === "") return null;

  return (
    <div className="os-stop-body">
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}
      {appReport === null ? null : (
        <Facts>
          <Fact label="Plan" value={appReport.buildPlan} />
          <Fact label="Output" value={appReport.output} mono />
        </Facts>
      )}
      <BuildLog tail={log} />
    </div>
  );
}

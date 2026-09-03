import { Fact, Facts } from "../../../../kit";
import { BuildLog, ProblemNotice } from "../../packages/ReportView";
import { buildSurfaceLabel, type DeploymentRow } from "../../packages/rows";
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
// The Build epic filled that in (epic memql#4900): a run that actually built
// says WHERE it built and on WHICH replica, above the log. Both come off the
// run's own row, so they are the only durable record of it -- the directory
// the build ran in is deleted when the plan ends, and on a two-replica
// workbench "which node" is the difference between a bad build script and
// one sick machine.

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

  const surface = run === null ? "" : buildSurfaceLabel(run);

  if (refusal === null && appReport === null && surface === "" && log.trim() === "") return null;

  return (
    <div className="os-stop-body">
      {refusal ? <ProblemNotice problem={refusal} tone="error" /> : null}
      {appReport === null ? null : (
        <Facts>
          <Fact label="Plan" value={appReport.buildPlan} />
          <Fact label="Output" value={appReport.output} mono />
        </Facts>
      )}
      {/* Rendered only when there IS a surface. A run that never reached the
          build stage has none on it, and an empty pair of labels would be
          four words claiming a fact this cluster does not hold. */}
      {surface === "" ? null : (
        <Facts>
          <Fact label="Built" value={surface} />
          {(run?.builtOn?.nodeId ?? "") === "" ? null : <Fact label="On" value={run!.builtOn!.nodeId} mono />}
        </Facts>
      )}
      <BuildLog tail={log} />
    </div>
  );
}

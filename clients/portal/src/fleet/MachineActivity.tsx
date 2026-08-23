import type { ReactNode } from "react";

import { Badge, DataText, EmptyState, Skeleton, type StatusTone } from "../ui";
import { formatDuration, formatMoment } from "./format";
import { chipsFromMap } from "./labels";
import type { RoutingRecord } from "./rows";
import { useMachineActivity } from "./useMachines";

// One machine's recent calls, and -- the part this list exists for -- WHY each
// one landed here.
//
// The routing record is the whole point. Before it, "the agent ran a command
// on machine B" was the entire story available after the fact, and every
// question an operator actually had ("why not machine A", "did something
// refuse first", "which policy was in force") had no answer anywhere. Rendering
// it is what turns a call log into something you can debug a routing rule
// with.
//
// An EMPTY routing object is rendered as "not recorded", never as an empty
// table: rows written before the router existed carry none, and so do paths
// that were denied before anything was chosen. "Chose nothing" and "we did not
// write down what was chosen" are different facts.

const OUTCOME_TONE: Record<string, StatusTone> = {
  success: "ok",
  failure: "danger",
  cancelled: "neutral",
  timeout: "warn",
  denied_by_scope: "danger",
  denied_by_policy: "danger",
  denied_by_classifier: "danger",
  kill_switch_engaged: "danger",
  no_worker_available: "warn",
  rerouted: "warn",
};

export function MachineActivity({
  workerId,
  asOperator,
}: {
  workerId: string;
  // True on the all-machines view. It selects the operator-scoped read, which
  // is the only one that returns rows for a machine the caller does not own.
  asOperator: boolean;
}): ReactNode {
  const { invocations, loading, error } = useMachineActivity(workerId, true, asOperator);

  if (loading && invocations.length === 0) return <Skeleton variant="rows" rows={3} />;

  if (error !== "") {
    return (
      <p role="alert" className="text-sm text-danger">
        Could not read this machine&rsquo;s recent calls: {error}
      </p>
    );
  }

  if (invocations.length === 0) {
    return (
      <EmptyState
        statement={
          asOperator
            ? "No calls recorded against this machine."
            : "No calls recorded against this machine. This list is scoped to machines you own."
        }
      />
    );
  }

  return (
    <ul className="flex flex-col gap-2">
      {invocations.map((one) => (
        <li key={one.id} className="rounded border border-line bg-surface px-3 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone={OUTCOME_TONE[one.outcome] ?? "neutral"}>{one.outcome || "unknown"}</Badge>
            <span className="text-sm font-medium">
              {one.tool}
              <span className="text-subtle"> · </span>
              {one.action}
            </span>
            <span className="text-xs text-subtle">
              <DataText kind="time">{formatMoment(one.createdAt)}</DataText>
            </span>
            {one.durationMs > 0 ? (
              <span className="text-xs text-subtle">
                <DataText kind="number">{formatDuration(one.durationMs)}</DataText>
              </span>
            ) : null}
          </div>

          {one.errorMessage === "" && one.errorCode === "" ? null : (
            <p className="mt-1 text-xs text-danger">
              {one.errorCode === "" ? "" : `${one.errorCode}: `}
              {one.errorMessage}
            </p>
          )}

          <RoutingReading routing={one.routing} />
        </li>
      ))}
    </ul>
  );
}

function RoutingReading({ routing }: { routing: RoutingRecord }): ReactNode {
  if (!routing.present) {
    return (
      <p className="mt-1 text-xs text-subtle">
        Routing not recorded. Either the call predates the router, or it was refused before a
        machine was chosen.
      </p>
    );
  }

  const requires = chipsFromMap(routing.requireLabels);
  const prefers = chipsFromMap(routing.preferLabels);

  return (
    <dl className="mt-1.5 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 text-xs">
      <Reading label="Strategy" value={routing.strategy || "not recorded"} />
      <Reading
        label="Candidates"
        value={
          routing.candidatesConsidered.length === 0
            ? "not recorded"
            : `${routing.candidatesConsidered.length} considered, in order: ${routing.candidatesConsidered.join(", ")}`
        }
      />
      <Reading
        label="Attempts"
        value={
          routing.attempts <= 1
            ? "1 -- the first candidate took it"
            : `${routing.attempts} -- a candidate refused before starting and the fallback moved on`
        }
      />
      <Reading label="Selected by" value={routing.selectedBy || "not recorded"} />
      {routing.reroutedFrom === "" ? null : (
        <Reading label="Rerouted from" value={routing.reroutedFrom} />
      )}
      {routing.policyId === "" ? (
        <Reading label="Policy" value="none -- the default firstFit applied" />
      ) : (
        <Reading label="Policy" value={routing.policyId} />
      )}
      {requires.length === 0 ? null : <Reading label="Required" value={requires.join(", ")} />}
      {prefers.length === 0 ? null : <Reading label="Preferred" value={prefers.join(", ")} />}
    </dl>
  );
}

function Reading({ label, value }: { label: string; value: string }): ReactNode {
  return (
    <>
      <dt className="text-subtle">{label}</dt>
      <dd className="min-w-0 break-words text-muted">{value}</dd>
    </>
  );
}

import { useState } from "react";

import { Button, Notice } from "../../../kit";
import { formatDuration, formatMoment } from "../format";
import { OUTCOME_TONE, type InvocationRow } from "../rows";
import { RoutingRecordView } from "./RoutingRecordView";
import { useInvocations } from "./useInvocations";

// The per-machine call history: what ran on this machine recently, and how
// the router chose it.
//
// Mounted from the machine detail panel (task 001's surface) rather than from
// the Routing section, because the question it answers is "what happened on
// THIS machine" -- and the answer is only meaningful next to the machine.
// The Routing section owns the policy, which is per-person; this is per-row.
//
// `enabled` is the expansion state, so the read runs when the panel opens
// rather than on every machine in the list. Twenty machines rendering a
// collapsed history would be twenty queries on load, for a panel nobody has
// opened.

export function CallHistory({ workerId, machineLabel }: { workerId: string; machineLabel: string }) {
  const [open, setOpen] = useState(false);
  const { invocations, loading, error, readAt, refresh } = useInvocations(workerId, open);

  return (
    <div className="os-fleet-history">
      <div className="os-head-actions">
        <Button onClick={() => setOpen((v) => !v)} ariaLabel={`Recent calls on ${machineLabel}`}>
          {open ? "Hide recent calls" : "Recent calls"}
        </Button>
        {/* "Re-read" everywhere, deliberately. This said "Refresh" while
            three other controls in the same app said "Re-read" for the same
            act -- one action keeps one name through the whole product, or the
            vocabulary stops being signposting and becomes noise. */}
        {open ? (
          <Button onClick={refresh} busy={loading} busyLabel="Reading...">
            Re-read
          </Button>
        ) : null}
      </div>

      {!open ? null : (
        <>
          {/* A read with no subscription behind it has to say how old it is,
              or the absence of new rows reads as "nothing is happening"
              rather than as "nobody has asked lately". */}
          <p className="os-caption">
            {readAt === null
              ? "Reading this machine's recent calls."
              : `Read at ${formatMoment(readAt.toISOString())}. Call telemetry is not broadcast to browsers (volume), so this list refreshes on request rather than on its own.`}
          </p>

          {error ? (
            <Notice
              tone="error"
              sentence="The call history could not be read."
              next={
                invocations.length > 0
                  ? "The calls below are from the last successful read."
                  : "Nothing was loaded."
              }
              detail={error}
            />
          ) : null}

          {invocations.length === 0 && !loading && error === "" ? (
            <p className="os-caption">No calls recorded for this machine.</p>
          ) : null}

          <ul className="os-fleet-calls">
            {invocations.map((call) => (
              <li key={call.id}>
                <CallLine call={call} />
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

function CallLine({ call }: { call: InvocationRow }) {
  const [expanded, setExpanded] = useState(false);
  // An outcome this client does not know about renders in the neutral tone
  // and by its own name. The enum grows server-side, and a value we cannot
  // classify is still a value an operator needs to read.
  const tone = OUTCOME_TONE[call.outcome] ?? "warn";
  const when = call.startedAt || call.createdAt;

  return (
    <div className="os-fleet-call">
      <button
        type="button"
        className="os-fleet-call-head"
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
      >
        <span className="os-fleet-outcome" data-tone={tone}>
          {call.outcome || "unknown"}
        </span>
        <span className="os-mono">
          {call.tool}
          {call.action ? `.${call.action}` : ""}
        </span>
        <span className="os-caption">{formatMoment(when)}</span>
        <span className="os-caption">{formatDuration(call.durationMs)}</span>
      </button>
      {expanded ? (
        <div className="os-fleet-call-body">
          {call.errorCode || call.errorMessage ? (
            <p className="os-fleet-call-error os-mono">
              {[call.errorCode, call.errorMessage].filter((one) => one !== "").join(": ")}
            </p>
          ) : null}
          <RoutingRecordView routing={call.routing} />
        </div>
      ) : null}
    </div>
  );
}

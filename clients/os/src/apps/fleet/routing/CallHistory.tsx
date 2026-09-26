import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { History } from "lucide-react";
import { InfoDetail } from "../../../kit/InfoDetail";
import { useState } from "react";

import { Button, Notice, EmptyState, RefreshButton, Subhead, RecordList, RecordRow } from "../../../kit";
import { formatDuration, formatMoment } from "../../../kit/format";
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

export function CallHistory({ workerId, machineLabel, standalone = false }: { workerId: string; machineLabel: string; standalone?: boolean }) {
  const [open, setOpen] = useState(standalone);
  const { invocations, loading, error, readAt, refresh } = useInvocations(workerId, open);

  return (
    <div className="os-fleet-history">
      <div className="fleet-bank-heading">
        {standalone ? <Subhead meta={readAt && !loading && !error ? invocations.length : undefined}>Recent calls</Subhead> : <Button onClick={() => setOpen((v) => !v)} ariaLabel={`Recent calls on ${machineLabel}`}>
          {open ? "Hide recent calls" : "Recent calls"}
        </Button>}
        {open ? <span className="fleet-heading-actions">
          <InfoDetail title="Call history"><p>Calls routed to this machine. App sessions are listed separately in Activity.</p><p>{readAt === null ? "The first read has not completed." : `Read at ${formatMoment(readAt.toISOString())}. Refresh to see newer calls.`}</p></InfoDetail>
          <RefreshButton label="Refresh recent calls" busy={loading} onClick={refresh} />
        </span> : null}
      </div>

      {!open ? null : (
        <>
          {loading && invocations.length === 0 ? <RecordListSkeleton label="Loading this machine's recent calls" /> : null}

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
            <EmptyState icon={History} title="No calls recorded">Calls appear here when work runs on this machine. App sessions are available in Activity.</EmptyState>
          ) : null}

          {invocations.length > 0 ? <RecordList as="ul" label="Recent calls">
            {invocations.map((call) => (
              <CallLine key={call.id} call={call} />
            ))}
          </RecordList> : null}
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
      <RecordRow name={`${call.tool}${call.action ? `.${call.action}` : ""}`} secondary={formatMoment(when)}
        state={call.outcome || "unknown"} tone={tone === "ok" ? "accent" : "warn"}
        open={expanded} onOpen={() => setExpanded(v => !v)}>
        <span>{formatDuration(call.durationMs)}</span>
      </RecordRow>
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

import { ArrowLeft } from "lucide-react";

import { Button, Chip, Fact, Facts, Head, Notice, Panel, Subhead, formatBytes, formatMoment } from "../../../kit";
import { FigureValue } from "../../../cluster/FigureValue";
import {
  appLabel,
  sessionIsLive,
  statusTone,
  totalTokens,
  type AppSessionDetailRow,
} from "../rows";
import { useAppSessionDetail } from "./useAppSessions";

// One delegated run: what it was asked, what it spent, and what it said
// (epic memql#5009).
//
// ===========================================================================
// IT REPLACES THE LIST (DESIGN.md rule 11)
// ===========================================================================
// A transcript is TALL -- it is the whole output of a coding agent working on
// somebody's machine -- so appending it beneath the list it was selected from
// would be exactly the 5,069px, two-Head page rule 11 was written against.
// This carries the quiet back-Head instead, DeployablePage's shape. ONE Head
// per view.
//
// ===========================================================================
// THE TRANSCRIPT IS A RECORD, RENDERED VERBATIM
// ===========================================================================
// It goes into a <pre> and nothing is parsed, prettified or re-wrapped. Chunk
// `seq` is monotonic and the engine DROPS out-of-order and duplicate chunks
// (component/worker/session.go), so what arrived is what there is -- any
// interpretation this client applied would be a second account of a run, free
// to be confidently wrong about what the agent did.
//
// TRUNCATION IS STATED, AND POINTS AT THE ARTIFACTS. The row keeps a bounded
// transcript and the FULL one is pushed to the Library at the end of the run.
// A transcript that simply stops reads as a run that stopped, which is the
// failure to avoid -- so when the bound was hit the page says so and names
// where the whole thing is.

export function SessionPage({
  sessionId,
  onBack,
}: {
  sessionId: string;
  onBack: () => void;
}) {
  const { session, loading, error, polling, readAt, reread } = useAppSessionDetail(sessionId);

  return (
    <div className="os-fleet os-fleet-session">
      <Head title={session === null ? "Run" : `${appLabel(session.app)} -- ${session.kind}`}>
        <Button tone="quiet" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden /> Apps
        </Button>
        {/* A FINISHED RUN IS NOT RE-READ ON A TIMER, so the manual re-read is
            the honest control for one. While a run is LIVE the poll is doing
            it, and the word beside the status says so. */}
        {session !== null && !sessionIsLive(session.status) ? (
          <Button onClick={reread}>Re-read</Button>
        ) : null}
      </Head>

      {error === "" ? null : (
        <Notice
          tone="error"
          sentence="This run could not be read."
          next={session === null ? "Nothing was loaded." : "What is below is the last read that landed."}
          detail={error}
        />
      )}

      {loading && session === null ? <p className="os-caption">Reading this run.</p> : null}

      {!loading && session === null && error === "" ? (
        <p className="os-caption">
          There is no run at this id. It may have been swept, or the list may be older than the
          cluster.
        </p>
      ) : null}

      {session === null ? null : (
        <SessionBody session={session} polling={polling} readAt={readAt} />
      )}
    </div>
  );
}

function SessionBody({
  session,
  polling,
  readAt,
}: {
  session: AppSessionDetailRow;
  polling: boolean;
  readAt: Date | null;
}) {
  const live = sessionIsLive(session.status);

  return (
    <>
      <Panel label="Run">
        <Subhead>Status</Subhead>
        <Facts>
          <Fact
            label="Status"
            value={
              <>
                <span className="os-fleet-session-status" data-tone={statusTone(session.status)}>
                  {session.status}
                </span>
                {polling ? <span className="os-caption"> refreshing</span> : null}
              </>
            }
          />
          <Fact label="Billing" value={<Chip tone={session.billing === "subscription" ? "accent" : "muted"}>{session.billing}</Chip>} />
          {/* TOKENS THE APP DID NOT REPORT ARE ABSENT, NOT ZERO. An app that
              said nothing did not say it spent nothing, and the two lead to
              opposite conclusions about a run. */}
          <Fact label="Tokens in" value={<FigureValue figure={session.usage.inputTokens} />} />
          <Fact label="Tokens out" value={<FigureValue figure={session.usage.outputTokens} />} />
          <Fact label="Tokens" value={<FigureValue figure={totalTokens(session)} />} />
          <Fact
            label="Reported cost"
            value={<FigureValue figure={session.usage.costUSD} format={(v) => `$${v.toFixed(4)}`} />}
          />
          <Fact label="Machine" value={session.workerId || "not recorded"} mono />
          <Fact label="Workspace" value={session.workspace || "not recorded"} mono />
          <Fact label="Started" value={formatMoment(session.startedAt)} />
          <Fact label="Ended" value={session.endedAt === "" ? "still running" : formatMoment(session.endedAt)} />
          {session.status === "failed" ? (
            <Fact label="Exit code" value={<FigureValue figure={session.exitCode} />} mono />
          ) : null}
          {session.planId === "" ? null : <Fact label="Plan" value={session.planId} mono />}
          {session.taskId === "" ? null : <Fact label="Task" value={session.taskId} mono />}
        </Facts>

        {session.usage.known ? null : (
          <p className="os-caption">
            This app reported no usage, so the run is billed as <strong>unknown</strong> rather
            than as free. MemQL records what an app reports and never infers the rest.
          </p>
        )}

        <p className="os-caption">
          {readAt === null
            ? "Not read yet."
            : live
              ? `Polling while this run is ${session.status}; last read ${formatMoment(readAt.toISOString())}.`
              : `Read ${formatMoment(readAt.toISOString())}. A finished run does not change, so this is not polled.`}
        </p>

        {session.errorMessage === "" ? null : (
          <Notice tone="error" sentence="The run reported an error." detail={session.errorMessage} />
        )}
        {session.cancelReason === "" ? null : (
          <Notice tone="warn" sentence="The run was cancelled." detail={session.cancelReason} />
        )}
      </Panel>

      {session.prompt === "" ? null : (
        <Panel label="What it was asked">
          <Subhead>What it was asked</Subhead>
          {/* The prompt is a record too -- shown as written, never reflowed. */}
          <pre className="os-fleet-transcript os-fleet-prompt">{session.prompt}</pre>
        </Panel>
      )}

      <Subhead>Transcript</Subhead>
      <p className="os-caption">
        <FigureValue figure={session.transcriptBytes} format={formatBytes} />
        {session.transcriptTruncated ? " kept -- truncated" : " kept"}
      </p>

      {session.transcriptTruncated ? (
        <Notice
          tone="warn"
          sentence="This transcript reached the size the row keeps, so what is below stops short of the end."
          next={
            session.producedArtifactIds.length > 0
              ? "The complete transcript was pushed to your Library at the end of the run -- it is among the artifacts listed below."
              : "The complete transcript is pushed to your Library when the run ends."
          }
        />
      ) : null}

      {/* VERBATIM, in a <pre>, and never parsed. See this file's header. */}
      <pre className="os-fleet-transcript" aria-label="Run transcript">
        {session.transcript === ""
          ? live
            ? "Waiting for the first output."
            : "This run produced no output."
          : session.transcript}
      </pre>

      {session.producedArtifactIds.length === 0 ? null : (
        <>
          <Subhead>Produced artifacts</Subhead>
          <ul className="os-fleet-artifacts" aria-label="Produced artifacts">
            {session.producedArtifactIds.map((artifactId) => (
              <li key={artifactId} className="os-mono">
                {artifactId}
              </li>
            ))}
          </ul>
        </>
      )}
    </>
  );
}

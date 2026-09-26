import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { RefreshButton } from "../FleetControls";

import { EmptyState, Chip, Fact, Facts, Head, Notice, Panel, Subhead, formatMoment } from "../../../kit";
import { Measure } from "../../../kit/MeasureView";
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
// A run's detail is TALL -- what it was asked, what it spent, its recording
// and every artifact it produced -- so appending it beneath the list it was
// selected from would be exactly the 5,069px, two-Head page rule 11 was
// written against.
// This carries the quiet back-Head instead, DeployablePage's shape. ONE Head
// per view.
//
// ===========================================================================
// THE RECORDING REPLACED THE TRANSCRIPT PANEL (epic memql#5396)
// ===========================================================================
// This page used to render a bounded transcript string off the session row:
// every chunk flattened into one field with the stream and the sequence
// discarded. What an app DID -- each command, each file it read or wrote,
// each call back into MemQL -- is now one v1:work:step per action in the
// session's own recording run, and the model's prose is one Library file.
//
// So the panel states the RECORDING and points at both. It deliberately does
// not draw the step timeline: that is the Work app's surface, and a second
// timeline here would be a second account of one run, free to disagree with
// the first. What belongs here is whether the recording is COMPLETE, which
// is the question a person on this page is actually asking.
//
// AN ABSENT COUNT IS NOT ZERO. A session nothing recorded and a session that
// recorded and found nothing to record are different facts about a run, and
// Measure renders the first as unmeasured rather than as a 0 nobody measured.

export function SessionPage({
  sessionId,
  onBack,
}: {
  sessionId: string;
  onBack: () => void;
}) {
  const { session, loading, error, polling, readAt, reread } = useAppSessionDetail(sessionId);

  return (
    <div className="os-fleet os-fleet-session" data-os-page-context={JSON.stringify({ page: "App session", sessionId, app: session?.app, status: session?.status })}>
      <Head title={session === null ? "Run" : `${appLabel(session.app)} -- ${session.kind}`} back={{ label: "Activity", onSelect: onBack }}>
        {/* A FINISHED RUN IS NOT RE-READ ON A TIMER, so the manual re-read is
            the honest control for one. While a run is LIVE the poll is doing
            it, and the word beside the status says so. */}
        {session !== null && !sessionIsLive(session.status) ? (
          <RefreshButton label="Refresh app session" busy={loading} onClick={reread} />
        ) : null}
      </Head>

      {error === "" ? null : (
        <Notice
          tone="error"
          sentence="This run could not be read."
          next={session === null ? "Nothing was loaded." : "Showing the last available session details."}
          detail={error}
        />
      )}

      {loading && session === null ? <ContentSkeleton kind="detail" label="Loading this run" /> : null}

      {!loading && session === null && error === "" ? (
        <EmptyState title="Session no longer available">It may have been removed since the list was last updated.</EmptyState>
      ) : null}

      {session === null ? null : (
        <SessionBody session={session} polling={polling} readAt={readAt} settled={!loading && !error} />
      )}
    </div>
  );
}

function SessionBody({
  session,
  polling,
  readAt,
  settled,
}: {
  session: AppSessionDetailRow;
  polling: boolean;
  readAt: Date | null;
  settled: boolean;
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
          <Fact label="Tokens in" value={<Measure figure={session.usage.inputTokens} />} />
          <Fact label="Tokens out" value={<Measure figure={session.usage.outputTokens} />} />
          <Fact label="Tokens" value={<Measure figure={totalTokens(session)} />} />
          <Fact
            label="Reported cost"
            value={<Measure figure={session.usage.costUSD} format={(v) => `$${v.toFixed(4)}`} />}
          />
          <Fact label="Machine" value={session.workerId || "not recorded"} mono />
          <Fact label="Workspace" value={session.workspace || "not recorded"} mono />
          <Fact label="Started" value={formatMoment(session.startedAt)} />
          <Fact label="Ended" value={session.endedAt === "" ? "still running" : formatMoment(session.endedAt)} />
          {session.status === "failed" ? (
            <Fact label="Exit code" value={<Measure figure={session.exitCode} />} mono />
          ) : null}
          {session.runId === "" ? null : <Fact label="Run" value={session.runId} mono />}
          {session.stepId === "" ? null : <Fact label="Step" value={session.stepId} mono />}
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
              ? `Updates automatically while ${session.status}. Last updated ${formatMoment(readAt.toISOString())}.`
              : `Last updated ${formatMoment(readAt.toISOString())}.`}
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

      <Subhead>Recording</Subhead>
      <Facts>
        <Fact label="Actions recorded" value={<Measure figure={session.recordedSteps} />} />
        <Fact label="Actions lost" value={<Measure figure={session.droppedActions} />} />
        <Fact
          label="Timeline"
          value={session.sessionRunId === "" ? "not recorded" : session.sessionRunId}
          mono
        />
        <Fact
          label="Transcript"
          value={
            session.transcriptFileId === ""
              ? live
                ? "saved to your Library when the session ends"
                : "not saved"
              : session.transcriptFileId
          }
          mono
        />
      </Facts>

      {session.sessionRunId === "" ? (
        <p className="os-caption">
          Nothing recorded this run, so there is no step-by-step account of what the app
          did. That is not the same as a run that did nothing.
        </p>
      ) : (
        <p className="os-caption">
          Every action this app took is a step of the run above -- each command, each file
          it read or wrote, and each call it made back into MemQL, with its arguments and
          the digest of what came back.
        </p>
      )}

      {/* A LOST ACTION IS SAID OUT LOUD. Anything lifted from an incomplete
          recording is incomplete, and a count nobody reads is a count that
          lets that happen quietly. */}
      {session.droppedActions.kind === "measured" && session.droppedActions.value > 0 ? (
        <Notice
          tone="warn"
          sentence="Some of this run's actions were not recorded."
          next="Its sequence has gaps, so anything built from this recording is incomplete. The run itself was unaffected."
        />
      ) : null}

      {session.transcriptTruncated ? (
        <Notice
          tone="warn"
          sentence="The saved transcript is shortened."
          next="The app produced more output than one transcript file holds, and the tail was cut."
        />
      ) : null}

      {session.producedArtifactIds.length === 0 ? null : (
        <>
          <Subhead meta={settled ? session.producedArtifactIds.length : undefined}>Produced artifacts</Subhead>
          <RecordList as="ul" label="Produced artifacts">
            {session.producedArtifactIds.map((artifactId) => (
              <RecordRow key={artifactId} name={artifactId} />
            ))}
          </RecordList>
        </>
      )}
    </>
  );
}

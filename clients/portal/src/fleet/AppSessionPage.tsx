import type { ReactNode } from "react";
import { useParams } from "react-router-dom";

import { Badge, Band, Container, ErrorNotice, PageHeader, Skeleton } from "../ui";
import { appLabel } from "./rows";
import { isLive, useAppSessionDetail } from "./useAppSessions";

// One delegated run's transcript (memql#4363).
//
// THE TRANSCRIPT IS RENDERED VERBATIM, in a monospace block, and never
// parsed. It is the output of somebody's coding agent working on their own
// machine; a renderer that tried to interpret it would be confidently wrong
// about what the agent did, and the only honest thing this page can show is
// what the run actually emitted.
//
// TRUNCATION IS STATED. The row keeps a bounded transcript and the full one
// is pushed to the Library at the end of the run. A transcript that simply
// stopped would read as a run that stopped, so when the bound was hit the
// page says so and points at the artifact.

export function AppSessionPage(): ReactNode {
  const params = useParams<{ sessionId: string }>();
  const sessionId = decodeURIComponent(params.sessionId ?? "");
  const { session, loading, error, polling } = useAppSessionDetail(sessionId);

  if (loading) return <Container><Skeleton /></Container>;
  if (error !== "")
    return (
      <Container>
        <ErrorNotice sentence="Could not read this run." detail={error} />
      </Container>
    );
  if (session === null) {
    return (
      <Container>
        <ErrorNotice sentence="There is no run at this address." next="It may have been cleaned up, or the link may be wrong." />
      </Container>
    );
  }

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          subtitle="Local apps"
          title={`${appLabel(session.app)} · ${session.kind}`}
          blurb={
            session.workspace === ""
              ? "A delegated run on one of your own machines."
              : `A delegated run in ${session.workspace} on one of your own machines.`
          }
        />

        <Band title="Status">
          <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-xs sm:grid-cols-3">
            <Fact label="Status">
              <Badge tone={session.status === "failed" ? "danger" : session.status === "ended" ? "ok" : "neutral"}>
                {session.status}
              </Badge>
              {polling ? <span className="ml-2 text-fg-muted">refreshing</span> : null}
            </Fact>
            <Fact label="Billing">
              <Badge tone={session.billing === "subscription" ? "ok" : "neutral"}>{session.billing}</Badge>
            </Fact>
            <Fact label="Tokens">
              {session.usage.known
                ? `${session.usage.inputTokens} in / ${session.usage.outputTokens} out`
                : "the app reported none"}
            </Fact>
            <Fact label="Reported cost">
              {session.usage.known && session.usage.costUSD > 0
                ? `$${session.usage.costUSD.toFixed(4)}`
                : "not reported"}
            </Fact>
            <Fact label="Started">{session.startedAt}</Fact>
            <Fact label="Ended">{session.endedAt || "still running"}</Fact>
            <Fact label="Machine">{session.workerId}</Fact>
            <Fact label="Plan">{session.planId || "none"}</Fact>
            <Fact label="Task">{session.taskId || "none"}</Fact>
          </dl>
          {session.usage.known ? null : (
            <p className="mt-3 text-xs text-fg-muted">
              This app reported no usage, so the run is billed as
              <span className="font-medium"> unknown</span> rather than as free.
              MemQL records what an app reports and never infers the rest.
            </p>
          )}
          {session.errorMessage === "" ? null : (
            <p className="mt-3 text-xs text-danger">{session.errorMessage}</p>
          )}
        </Band>

        {session.prompt === "" ? null : (
          <Band title="What it was asked" panel>
            <p className="whitespace-pre-wrap text-xs text-fg">{session.prompt}</p>
          </Band>
        )}

        <Band
          title="Transcript"
          meta={
            <span className="text-xs text-fg-muted">
              {session.transcriptBytes} bytes
              {session.transcriptTruncated ? " (truncated)" : ""}
            </span>
          }
          panel
        >
          {session.transcriptTruncated ? (
            <p className="mb-2 rounded border border-warn bg-warn-subtle px-3 py-2 text-xs text-fg">
              This transcript reached the size the row keeps. The complete one
              was pushed to the Library when the run ended -- see the produced
              artifacts below.
            </p>
          ) : null}
          <pre className="max-h-[32rem] overflow-auto whitespace-pre-wrap break-words rounded bg-surface-sunken p-3 font-mono text-[11px] leading-relaxed text-fg">
            {session.transcript === ""
              ? isLive(session.status)
                ? "Waiting for the first output..."
                : "This run produced no output."
              : session.transcript}
          </pre>
        </Band>

        {session.producedArtifactIds.length === 0 ? null : (
          <Band title="Produced artifacts" panel>
            <ul className="flex flex-col gap-1 text-xs">
              {session.producedArtifactIds.map((artifactId) => (
                <li key={artifactId} className="font-mono text-fg">
                  {artifactId}
                </li>
              ))}
            </ul>
          </Band>
        )}
      </section>
    </Container>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }): ReactNode {
  return (
    <div>
      <dt className="text-fg-muted">{label}</dt>
      <dd className="mt-0.5 text-fg">{children}</dd>
    </div>
  );
}

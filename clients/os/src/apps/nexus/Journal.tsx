import { Button, Caption, Chip, Notice, Subhead, formatDuration, formatMoment } from "../../kit";
import type { Journal as JournalState } from "./useNexus";
import { formatMoney, formatTokens, observationKindWord, servedWord } from "./rows";

// THE JOURNAL: the model calls and the observations of one run.
//
// ===========================================================================
// THIS IS NOT LIVE, AND THE PANEL SAYS SO RATHER THAN IMPLYING IT
// ===========================================================================
// `v1:work:modelCall` and `v1:work:observation` carry no broadcast routing
// rule -- deliberately, on volume grounds: one row per model request and one
// per tool result, which is a burst proportional to the work rather than to
// anything a person did. So there is no `graph.node.*` event for a
// subscription to receive, and a live list over either would render "Loading
// from the cluster" and then a list that silently never moved. That is WORSE
// than a plain read, because the caption would be claiming wiring that is not
// there.
//
// So it prints WHEN IT WAS READ and offers to look again -- the same call the
// Training app made for the knowledge side and the Accounts app for its
// ledger. And it says what that costs: a call made since you looked is not
// here.
//
// IT DOES NOT READ ON OPEN. Most visits to a run are about the timeline, and
// the journal is the expensive half; reading it unasked would make every run
// page two extra reads. "Read at" is then never a claim about a read this
// window did not take.

export function JournalPanel({ journal }: { journal: JournalState }) {
  const empty =
    journal.state === "ready" &&
    journal.modelCalls.length === 0 &&
    journal.observations.length === 0;

  return (
    <section className="os-nexus-journal" aria-label="The journal for this run">
      <div className="os-nexus-journal-head">
        <Subhead>Journal</Subhead>
        <span className="os-nexus-journal-when">
          {journal.state === "idle" ? (
            <Caption>Not read yet</Caption>
          ) : journal.state === "loading" ? (
            <Caption>Reading</Caption>
          ) : journal.readAt === "" ? null : (
            <Caption>Read at {formatMoment(journal.readAt)}</Caption>
          )}
        </span>
        <Button
          onClick={journal.read}
          busy={journal.state === "loading"}
          busyLabel="Reading"
          ariaLabel={journal.state === "idle" ? "Read the journal" : "Read the journal again"}
        >
          {journal.state === "idle" ? "Read the journal" : "Look again"}
        </Button>
      </div>

      {journal.state === "idle" ? (
        <Caption>
          The model calls and observations of this run. They are not part of the live feed -- this
          panel reads them when you ask, and says when it looked.
        </Caption>
      ) : null}

      {journal.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The journal could not be read."
          next="Nothing about the run changed. The timeline above is still live."
          detail={journal.error}
        />
      )}

      {empty ? (
        <Caption>
          Nothing journaled for this run. A run whose steps all ran without a model has no model
          calls to show, which is the point.
        </Caption>
      ) : null}

      {journal.modelCalls.length === 0 ? null : (
        <div className="os-nexus-journal-group">
          <p className="os-nexus-journal-count">
            {journal.modelCalls.length} model {journal.modelCalls.length === 1 ? "call" : "calls"}
          </p>
          <ul className="os-nexus-journal-list" aria-label="Model calls">
            {journal.modelCalls.map((call) => (
              <li key={call.id} className="os-nexus-journal-row" data-served={call.served}>
                <span className="os-nexus-journal-model os-mono">{call.model || "--"}</span>
                <span className="os-nexus-journal-served">{servedWord(call.served)}</span>
                {call.stepKey === "" ? null : (
                  <Chip tone="muted" title="The step that made this call">
                    {call.stepKey}
                  </Chip>
                )}
                <span className="os-nexus-journal-figures os-mono">
                  <span>
                    {formatTokens(
                      call.inputTokens === null && call.outputTokens === null
                        ? null
                        : (call.inputTokens ?? 0) + (call.outputTokens ?? 0),
                    )}{" "}
                    tok
                  </span>
                  <span>{formatMoney(call.cost)}</span>
                  <span>{call.latencyMs === null ? "--" : formatDuration(call.latencyMs)}</span>
                </span>
                {call.error === "" ? null : (
                  <span className="os-nexus-journal-error os-mono">{call.error}</span>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}

      {journal.observations.length === 0 ? null : (
        <div className="os-nexus-journal-group">
          <p className="os-nexus-journal-count">
            {journal.observations.length}{" "}
            {journal.observations.length === 1 ? "observation" : "observations"}
          </p>
          <ul className="os-nexus-journal-list" aria-label="Observations">
            {journal.observations.map((observation) => (
              <li key={observation.id} className="os-nexus-journal-row">
                <span className="os-nexus-journal-obs-kind">
                  {observationKindWord(observation.kind)}
                </span>
                {observation.stepKey === "" ? null : (
                  <Chip tone="muted">{observation.stepKey}</Chip>
                )}
                <span className="os-nexus-journal-content">{observation.content}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {journal.state === "ready" ? (
        <Caption>
          Read once, at the moment above. A call made since you looked is not here, and a run still
          working will have more. The journal is kept until its retention window closes, after
          which the run's summary stands in for it.
        </Caption>
      ) : null}
    </section>
  );
}

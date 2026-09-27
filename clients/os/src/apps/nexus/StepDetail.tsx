import type { ReactNode } from "react";

import { AttentionDestination } from "../../attention/Attention";
import {
  Chip,
  Chips,
  ContentSkeleton,
  Fact,
  Facts,
  InlineSkeleton,
  Notice,
  Panel,
  Subhead,
  formatDuration,
  formatMoment,
} from "../../kit";
import { NEXUS_APP_ID } from "./concepts";
import { formatMoney, formatTokens, type StepRow } from "./rows";
import { answeredBy, askedFor, overrideFacts, resultLine, type StepVersion } from "./versions";
import { kindCalledAModel, stepKindMeaning, stepKindWord, stepStatusWord, symptomMeaning, symptomWord } from "./words";

// One step, opened (epic memql#5414: now with its versions).
//
// ===========================================================================
// A DISCLOSURE UNDER THE ROW, NOT A SECOND PAGE
// ===========================================================================
// Rule 11 is about a list and a DETAIL PAGE sharing a scroller -- the tell is
// two Heads. This carries no Head and no acts: the acts for the selected
// version are on the page's one bar (rule 12). It is the row saying more
// about itself, the shape the Deployables rail's open stop has.
//
// ===========================================================================
// THE VERSION PICKER, THEN THE VERSION
// ===========================================================================
// Choice pills, v1 to vN, the current one labelled; then ONE panel for the
// version picked -- the grammar every detail in this shell uses (Panel,
// Subhead, Facts), so a version reads like the thing it is rather than like a
// new layout. What a person changed is said in their words, and who wrote a
// version is named: a version somebody authored is never a recording of an
// app, and the page must not let it look like one.

/** The unseen-change destination this area is (registry: `nexus:interventions`). */
export const STEP_VERSIONS_TARGET = "step-versions";

export function StepDetail({
  step,
  versions,
  versionsLoading,
  current,
  count,
  selected,
  onSelect,
  selectedVersion,
  session,
  viewerId,
  reachable,
  continues,
  onOpenRun,
  verdict,
}: {
  /** The live row: what the step IS, whichever version is picked. */
  step: StepRow;
  versions: readonly StepVersion[];
  /** The versions read has not answered, and there is more than one version to pick from. */
  versionsLoading: boolean;
  current: number | null;
  count: number;
  selected: number;
  onSelect: (version: number) => void;
  /** The picked version's row, or null while it has not arrived. */
  selectedVersion: StepVersion | StepRow | null;
  session: boolean;
  viewerId: string;
  /** The run is one a person can step into: the change marker is acknowledged only then. */
  reachable: boolean;
  /** More steps follow, so the spine's thread runs on past this disclosure. */
  continues: boolean;
  onOpenRun: (runId: string) => void;
  /**
   * The picked version's verdict, in its two halves: the pills for the
   * heading line, and what sits under it (the question, what was said, the
   * validator). Null when the version cannot be judged -- still running, or a
   * step no model or app took part in.
   */
  verdict: { choices: ReactNode; below: ReactNode } | null;
}) {
  const shown = selectedVersion;
  const pickable = versions.length > 1;

  return (
    <div className="os-nexus-step-detail" data-continues={continues || undefined}>
      <p className="os-nexus-step-meaning">
        <strong>{stepKindWord(step.kind)}</strong> -- {stepKindMeaning(step.kind)}
      </p>

      <AttentionDestination appId={NEXUS_APP_ID} sectionId="runs" target={STEP_VERSIONS_TARGET} visible={reachable}>
        {pickable ? (
          <div className="os-nexus-versions" role="radiogroup" aria-label={`Versions of ${step.key}`}>
            <span className="os-nexus-versions-label" aria-hidden>
              Version
            </span>
            {versions.map((version) => {
              const isCurrent = version.version === current;
              const state =
                version.status === "running" || version.status === "failed" ? stepStatusWord(version.status).toLowerCase() : "";
              return (
                <button
                  key={version.version}
                  type="button"
                  role="radio"
                  aria-checked={version.version === selected}
                  aria-label={[`Version ${version.version}`, isCurrent ? "current" : "", state]
                    .filter((part) => part !== "")
                    .join(", ")}
                  className="os-choice os-nexus-version"
                  data-status={version.status}
                  onClick={() => onSelect(version.version)}
                >
                  v{version.version}
                  {isCurrent ? <span className="os-nexus-version-note">Current</span> : null}
                  {state !== "" ? <span className="os-nexus-version-note">{state}</span> : null}
                </button>
              );
            })}
          </div>
        ) : versionsLoading ? (
          <div className="os-nexus-versions">
            <InlineSkeleton label="Loading this step's versions" />
          </div>
        ) : null}

        <Panel label={`Version ${selected} of ${step.key}`}>
          {/* THE VERDICT IS ON THE HEADING LINE, where a person opening the
              step finds it without scrolling past the evidence -- which is
              right below it, starting with what the version asked for and
              what it produced. */}
          <div className="os-nexus-version-head">
            <Subhead meta={count > 1 && selected === current ? "current" : count > 1 ? `of ${count}` : undefined}>
              Version {selected}
            </Subhead>
            {verdict?.choices ?? null}
          </div>
          {verdict?.below ?? null}
          {shown === null ? (
            <ContentSkeleton kind="detail" label={`Loading version ${selected}`} />
          ) : (
            <VersionFacts version={shown} session={session} viewerId={viewerId} />
          )}
        </Panel>
      </AttentionDestination>

      {step.dependsOn.length === 0 ? null : (
        <Chips label="Steps this one waited for">
          {step.dependsOn.map((key) => (
            <Chip key={key} tone="muted">
              {key}
            </Chip>
          ))}
        </Chips>
      )}

      {shown === null || shown.symptom === "" ? null : (
        <Notice
          tone="warn"
          sentence={symptomWord(shown.symptom)}
          next={symptomMeaning(shown.symptom)}
          detail={shown.errorMessage || undefined}
        />
      )}

      {shown === null || shown.childRunId === "" ? null : (
        <p className="os-caption">
          It opened{" "}
          <button type="button" className="os-nexus-link" onClick={() => onOpenRun(shown.childRunId)}>
            a run of its own
          </button>{" "}
          and waited for it.
        </p>
      )}
    </div>
  );
}

function VersionFacts({
  version,
  session,
  viewerId,
}: {
  version: StepRow;
  session: boolean;
  viewerId: string;
}) {
  const asked = askedFor(version.override);
  const answered = answeredBy(version.binding);
  const changes = overrideFacts(version.override, session);
  const result = resultLine(version.result);
  const author =
    version.authoredBy === ""
      ? ""
      : viewerId !== "" && version.authoredBy === viewerId
        ? "You"
        : version.authoredBy;
  // WHO ANSWERED IS ONLY A QUESTION FOR A STEP A MODEL OR AN APP MAY HAVE
  // ANSWERED. On a step that called none, "Answered by --" reads as something
  // missing; on one that did and recorded nothing, the dash is the answer.
  const askable = answered !== "" || kindCalledAModel(version.kind) !== false || version.childRunId !== "";
  const ran =
    version.startedAt === ""
      ? ""
      : version.durationMs === null || version.durationMs <= 0
        ? formatMoment(version.startedAt)
        : `${formatMoment(version.startedAt)}, for ${formatDuration(version.durationMs)}`;
  const ranTitle = [
    version.startedAt === "" ? "" : `Started ${version.startedAt}`,
    version.finishedAt === "" ? "" : `finished ${version.finishedAt}`,
  ]
    .filter((part) => part !== "")
    .join(", ");
  // ABSENT IS NOT ZERO. A step that reported neither renders no row, rather
  // than "0 tokens" beside a model call that happened.
  const spent = [
    version.tokens === null ? "" : `${formatTokens(version.tokens)} tokens`,
    version.cost === null ? "" : formatMoney(version.cost),
  ]
    .filter((part) => part !== "")
    .join(", ");
  const calls =
    version.callName === ""
      ? version.stepType
      : version.stepType === ""
        ? version.callName
        : `${version.callName} (${version.stepType})`;
  return (
    <Facts>
      <Fact label="Status" value={stepStatusWord(version.status)} />
      {asked === "" ? null : <Fact label="Asked for" value={asked} />}
      {askable ? <Fact label="Answered by" value={answered} /> : null}
      {changes.map((change) => (
        <Fact key={change.label} label={change.label} value={change.value} title={change.title} mono={change.mono} />
      ))}
      {author === "" ? null : <Fact label="Written by" value={author} mono={author !== "You"} />}
      <Fact label="Ran" value={ran} title={ranTitle || undefined} />
      {spent === "" ? null : <Fact label="Spent" value={spent} />}
      {result === "" ? null : <Fact label="Result" value={result} mono />}
      {/* A POSTCONDITION HAS THREE ANSWERS AND THE THIRD IS NOT "false".
          Absent means this step declares none; rendering that as "did not
          pass" would mark every run failed on a field nobody wrote. */}
      <Fact
        label="Postcondition"
        value={
          version.postconditionPassed === null
            ? "none declared"
            : version.postconditionPassed
              ? `passed (${version.postconditionKind || "check"})`
              : `did not hold -- ${version.postconditionMessage || "no message"}`
        }
      />
      {/* THE KEY A SIDE EFFECT RAN UNDER: runId:key:attempt, so each version
          has its own, and a resume can ask the far side whether it already
          holds a receipt for exactly this one. */}
      <Fact label="Idempotency key" value={version.idempotencyKey} mono />
      <Fact label="Calls" value={calls} mono />
    </Facts>
  );
}

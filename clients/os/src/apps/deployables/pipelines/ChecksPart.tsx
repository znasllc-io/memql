import { ListChecks } from "lucide-react";

import { Caption, Notice, useNow } from "../../../kit";
import { formatFreshness } from "../../../kit/format";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { Mark } from "./Mark";
import { Phrases } from "./Phrases";
import type { PipelineRow, RunRow } from "./rows";
import { checksBranchOf, lastRunPerBranch, newestFirst } from "./runs";
import { computeShort, deliveryShort, joinWords, runOutcome, runTitle, type Outcome } from "./words";
import "./pipelines.css";
import "./checks.css";

// A SOURCE'S CHECKS (design record D14; issue memql#5501): "a Checks node
// showing the last run per branch", between the source's facts and the apps it
// produces -- the order a commit travels, from the repository through its
// checks to what serves.
//
// THE SAME PART AS "APPS IT PRODUCES", so the two read as one page: an
// `.os-report-part` with a quiet heading, a count once the read has settled,
// and record rows. A row is a branch -- its newest run, named by the branch,
// with the commit's message beneath and the outcome in the Runs list's own
// words -- and it opens that run. The default branch comes first, because it
// is what serves; the rest follow newest first, and past six of them the rest
// are on Runs, which "All runs" opens refined to this source.
//
// THE ACTS ARE ON THE BAR, NOT HERE (DESIGN.md rule 12). Connect pipeline and
// Pipeline settings change what this source does, so they live on the source
// page's one ActionBar; this part says the state they act on.
//
// NOTHING IS SAID BEFORE THE READ HAS ANSWERED. "Not connected" read off a
// feed that has not landed is a claim, not a fact, so until both pipelines
// feeds settle the part is the shape of its rows -- and a read that failed
// says it failed rather than staying a loading shape forever.

/** How many branches the part lists. The rest are on Runs, a click away. */
export const CHECKS_ROWS = 6;

export interface ChecksPartProps {
  /** The source's pipeline, or null when it has none. */
  pipeline: PipelineRow | null;
  /** This pipeline's runs, in any order. */
  runs: readonly RunRow[];
  /** Whether the pipelines and runs feeds have answered: before that nothing is said. */
  settled: boolean;
  /** Why the feeds could not answer, when they could not. */
  error?: string;
  /** Open one run's page. Without it the rows are read-only lines. */
  onOpenRun?: (runId: string) => void;
  /** Open the Runs tab refined to this source. Without it there is no All runs. */
  onOpenRuns?: () => void;
}

export function ChecksPart({ pipeline, runs, settled, error = "", onOpenRun, onOpenRuns }: ChecksPartProps) {
  const now = useNow();
  const rows = pipeline === null ? [] : lastRunPerBranch(runs, pipeline.defaultBranch);
  return (
    <section className="os-report-part pipeline-checks">
      <div className="pipeline-checks-heading">
        <h4 className="os-report-heading">
          <ListChecks size={12} aria-hidden /> Checks
          {/* The count is every branch with a run, not the rows drawn: a
              count describes the collection, before any windowing. */}
          {settled && pipeline !== null ? <span className="os-head-meta">{rows.length}</span> : null}
        </h4>
        {pipeline !== null && onOpenRuns ? (
          <button type="button" className="pipeline-checks-all" onClick={onOpenRuns}>All runs</button>
        ) : null}
      </div>

      {!settled ? (
        error !== "" ? <Notice tone="error" sentence="Checks could not be read." detail={error} /> : <RecordListSkeleton label="Loading checks" rows={2} />
      ) : pipeline === null ? (
        <Caption>
          Not connected. Connecting runs this repository's checks on every push and pull request, and reports them on GitHub.
        </Caption>
      ) : (
        <>
          {pipeline.status === "disconnected" ? (
            <Caption>Disconnected. Its runs stay as history.</Caption>
          ) : rows.length === 0 ? (
            <Caption>No runs yet. The first run starts with the next push or pull request.</Caption>
          ) : null}
          {rows.length > 0 ? (
            <RecordList as="ul">
              {rows.slice(0, CHECKS_ROWS).map((run) => (
                <ChecksRow key={run.id} run={run} now={now} onOpenRun={onOpenRun} />
              ))}
            </RecordList>
          ) : null}
        </>
      )}
    </section>
  );
}

/**
 * A branch's newest run, in the Runs list's own row: the outcome's detail in
 * the middle column, the word and the time stacked at the end -- so a run
 * reads the same on the source's page as on Runs.
 */
function ChecksRow({ run, now, onOpenRun }: { run: RunRow; now: Date; onOpenRun?: (runId: string) => void }) {
  const outcome = runOutcome(run);
  return (
    <RecordRow
      icon={<Mark state={outcome.mark} />}
      name={checksBranchOf(run)}
      secondary={runTitle(run)}
      state={outcome.word}
      tone={outcome.tone}
      // HOW LONG AGO it was opened, not the time of day: the Runs list sits
      // under day headings, and these rows sit under none.
      trailing={run.queuedAt === "" ? null : <time className="pipeline-run-time" dateTime={run.queuedAt}>{formatFreshness(run.queuedAt, now)}</time>}
      current={outcome.mark === "current"}
      {...(onOpenRun ? { onOpen: () => onOpenRun(run.id), label: checksRowLabel(run) } : {})}
    >
      {outcome.detail !== "" ? <span className="pipeline-run-detail"><Phrases text={outcome.detail} /></span> : null}
    </RecordRow>
  );
}

/**
 * A Checks row read out: "Open the newest run on main: Fix shard balance,
 * failed at tests, 7m 40s". The row's NAME is its branch, so the label says
 * what kind of thing opens, then the commit, then how it went.
 */
export function checksRowLabel(run: RunRow): string {
  return `Open the newest run on ${checksBranchOf(run)}: ${runTitle(run)}, ${outcomeClause(runOutcome(run))}`;
}

/** "failed at tests, 7m 40s", "running tests", "passed, 4 stages, 4m 12s". */
function outcomeClause(outcome: Outcome): string {
  const word = outcome.word.toLowerCase();
  if (outcome.detail === "") return word;
  // Where a run stopped or what it is running reads on from the word; any
  // other detail is a clause of its own.
  return outcome.detail.startsWith("at ") || outcome.word === "Running" ? `${word} ${outcome.detail}` : `${word}, ${outcome.detail}`;
}

/**
 * The source page's Pipeline fact (D14: "manifest, stage count, delivery,
 * compute"): "memql-package.yaml, 4 stages, webhook, cluster".
 *
 * THE STAGE COUNT IS WHAT A RUN PLANNED. The pipeline row does not carry its
 * manifest -- the manifest lives in the repository and is read at each run's
 * commit -- so the count is the newest planned run's, and with no planned run
 * yet the fact leaves it out rather than guessing one.
 */
export function pipelineFact(pipeline: PipelineRow | null, runs: readonly RunRow[]): string {
  if (pipeline === null) return "Not connected";
  if (pipeline.status === "disconnected") return "Disconnected";
  const planned = [...runs].sort(newestFirst).find((run) => run.stages.length > 0);
  const stages = planned === undefined ? "" : `${planned.stages.length} stage${planned.stages.length === 1 ? "" : "s"}`;
  return joinWords("memql-package.yaml", stages, deliveryShort(pipeline.delivery), computeShort(pipeline.compute));
}

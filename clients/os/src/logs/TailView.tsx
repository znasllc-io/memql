import { Button, Caption, Notice } from "../kit";
import { formatFreshness } from "../kit/format";
import { FollowControl } from "./facets";
import { lineCount } from "./filters";
import { LogDetail } from "./LogDetail";
import { LogLine, ROW_HEIGHT } from "./LogLine";
import type { LogRow } from "./rows";
import type { LogTail } from "./useLogTail";
import { WindowedList } from "./WindowedList";

// Everything below the Head of a following surface (epic memql#4895): the
// scope line, the two empty answers, the windowed list with its jump pill,
// and the detail panel for the selected line.
//
// ONE VIEW FOR THE TWO TAILS. The per-app Logs section and the Logs app's
// Stream differ only in what they READ -- an app's slice against the whole
// store, and which facets stand in Refine. Everything after the Head is the
// same reading of the same tail, and a second copy of it is a copy that
// drifts: the pill's sentence in one, the paused state's dot in the other.
// So each section keeps its Head, its facets and its state, and hands the
// rest here.

export type TailDensity = keyof typeof ROW_HEIGHT;

export function TailView({
  tail,
  rows,
  follow,
  onFollowChange,
  selectedId,
  onSelect,
  onSubject,
  now,
  density,
  label,
  id,
  narrowed,
  emptySentence,
  emptyHint,
}: {
  tail: LogTail;
  /** The rows to show: the tail's, folded to the window. */
  rows: LogRow[];
  follow: boolean;
  onFollowChange: (next: boolean) => void;
  selectedId: string;
  onSelect: (id: string) => void;
  onSubject: (subject: string, subjectConcept: string) => void;
  /** One clock per section, so no two rows disagree about "now". */
  now: Date;
  density: TailDensity;
  /** Names the grid for assistive tech. */
  label: string;
  /** The grid's DOM id prefix. */
  id: string;
  /** Whether a facet narrows the reading -- the line between "nothing
   *  recorded" and "no lines match". */
  narrowed: boolean;
  /** What to say when nothing narrows the reading and nothing arrived. */
  emptySentence: string;
  emptyHint: string;
}) {
  const selected = selectedId === "" ? undefined : rows.find((row) => row.id === selectedId);
  const pending = tail.newSinceScrolled;

  return (
    <>
      {/* Rule 3's shape: quiet text on the scope line. "Following" carries
          the live dot and swaps to "Paused"; the freshness beside it is the
          poll's own stamp -- rendered continuously, never news. */}
      <div className="os-logs-scope">
        <FollowControl following={follow} onToggle={() => onFollowChange(!follow)} />
        <span className="os-caption">
          {tail.lastPolledAt === null
            ? "not read yet"
            : `updated ${formatFreshness(tail.lastPolledAt.toISOString(), now)}`}
        </span>
        {tail.trimmed > 0 ? (
          <span className="os-caption">{lineCount(tail.trimmed)} older let go to stay under the row cap</span>
        ) : null}
      </div>

      {tail.state === "error" ? (
        <Notice
          tone="error"
          sentence="The lines could not be read."
          next="The next poll tries again on its own."
          detail={tail.error}
        />
      ) : null}

      {rows.length === 0 ? (
        tail.state === "seeding" ? (
          <Caption>Reading from the cluster.</Caption>
        ) : (
          /* Empty and filtered-to-empty are DIFFERENT answers: one is about
             the store, the other about the question just asked of it. */
          <div className="os-logs-empty">
            <p className="os-logs-empty-line">{narrowed ? "No lines match." : emptySentence}</p>
            <Caption>{narrowed ? "Clear a facet or widen the window to see more." : emptyHint}</Caption>
          </div>
        )
      ) : (
        <div className="os-logs-list">
          <WindowedList
            rows={rows}
            rowHeight={ROW_HEIGHT[density]}
            renderRow={(row) => <LogLine row={row} now={now} onSubject={onSubject} />}
            rowId={(row) => row.id}
            selectedId={selectedId}
            onSelect={onSelect}
            follow={follow}
            onFollowChange={onFollowChange}
            label={label}
            id={id}
          />
          {/* ABSOLUTE against the list's relative root, never fixed: the
              desk plate is CSS-transformed and would become a fixed
              element's containing block. */}
          {!follow && pending > 0 ? (
            <div className="os-logs-jump">
              <Button onClick={() => onFollowChange(true)}>
                {pending} new {pending === 1 ? "line" : "lines"} -- Jump to latest
              </Button>
            </div>
          ) : null}
        </div>
      )}

      {selected ? <LogDetail row={selected} onClose={() => onSelect("")} /> : null}
    </>
  );
}

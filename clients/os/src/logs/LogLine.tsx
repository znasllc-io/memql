import { formatFreshness } from "../kit/format";
import { attrsInline, conceptWord, levelWord, type LogRow } from "./rows";

// ONE row renderer for every logs surface (epic memql#4895, spec H "Rows").
//
// The anatomy, on one fixed-height line: a two-pixel left rule coloured for
// warn and error only; the elapsed time, tabular and muted, with the exact
// instant on `title`; the level WORD, coloured only for warn and error and
// muted otherwise -- colour is never the only carrier; the component; the
// message in the mono voice; the attributes inline as `key=value`, muted,
// capped at forty percent of the row; and, when the line is about something,
// a small quiet mark at the row end that narrows to it.
//
// No badge, no per-row border, no arrival ring: a log is nothing but
// arrivals, and a row that announced itself would be a list that never
// stopped announcing. The selected row sits on `--os-raised`, which the
// windowed list's wrapper paints.
//
// Density is the stack's (`.os-app-stack[data-density]`): comfortable rows
// are 30px and compact 22px, and the windowed list is told the same number
// so the geometry and the CSS agree.

/** Row heights per density. The windowed list and the stylesheet both read
 *  these numbers; they must agree or the slice drifts off the scrollbar. */
export const ROW_HEIGHT = { comfortable: 30, compact: 22 } as const;

export function LogLine({
  row,
  now,
  onSubject,
}: {
  row: LogRow;
  /** One clock per section, so two rows cannot disagree about "now". */
  now: Date;
  /** Narrow the surface to this line's subject. Absent = no mark. */
  onSubject?: (subject: string, subjectConcept: string) => void;
}) {
  const attrs = attrsInline(row.attributes);
  const noteworthy = row.level === "warn" || row.level === "error";
  return (
    <div className="os-logs-line" data-level={row.level} data-noteworthy={noteworthy || undefined}>
      <span className="os-logs-time" role="gridcell" title={row.occurredAt}>
        {formatFreshness(row.occurredAt, now)}
      </span>
      <span className="os-logs-level" role="gridcell" data-level={row.level}>
        {levelWord(row.level)}
      </span>
      <span className="os-logs-component" role="gridcell" title={row.component}>
        {row.component || "--"}
      </span>
      <span className="os-logs-message os-mono" role="gridcell" title={row.message}>
        {row.message}
      </span>
      <span className="os-logs-attrs os-mono" role="gridcell" title={attrs === "" ? undefined : attrs}>
        {attrs}
      </span>
      <span className="os-logs-subject-cell" role="gridcell">
        {row.subject !== "" && onSubject ? (
          <button
            type="button"
            className="os-logs-subject"
            title={`${row.subjectConcept || "subject"} ${row.subject}`}
            aria-label={`Narrow to ${conceptWord(row.subjectConcept)} ${row.subject}`}
            onClick={(event) => {
              // The row beneath selects on click; the mark narrows instead,
              // and both would be one gesture doing two things.
              event.stopPropagation();
              onSubject(row.subject, row.subjectConcept);
            }}
          >
            {conceptWord(row.subjectConcept)}
          </button>
        ) : null}
      </span>
    </div>
  );
}

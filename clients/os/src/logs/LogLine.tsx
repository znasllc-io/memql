import { formatFreshness } from "../kit/format";
import { attrsInline, conceptWord, levelWord, type LogRow } from "./rows";

// ONE row renderer for every logs surface (epic memql#4895, spec H "Rows").
//
// The anatomy, on one fixed-height line: a two-pixel left rule coloured for
// warn and error only; the elapsed time, tabular and muted, with the exact
// instant on `title`; the level WORD, coloured only for warn and error and
// muted otherwise -- colour is never the only carrier; the component; the
// message in the mono voice; the attributes inline as `key=value`, muted, at
// most forty percent of the row and the FIRST thing to give way when the row
// is short; and, when the line is about something, a small quiet mark at the
// row end, at most eighteen characters, that narrows to it.
//
// THREE LAYOUTS, ONE PER LIST (R40, R40b, epic memql#5478), named on the
// list as `data-layout` by useLogRowLayout from its width and the reader's
// font size. WIDE: the attributes are an aligned column, because the row has
// room for it beside a comfortable message. MEDIUM: the message first, sized
// by its own length, with the attributes in whatever it leaves. NARROW: two
// rows -- time, level, component and the mark, then the whole message beneath
// them; the inline attributes are left to the line's detail, which lists
// every one of them. The message is the line, so it is never the cell that
// loses.
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

/** A log list's density: the key of every row-geometry table here. */
export type LogDensity = keyof typeof ROW_HEIGHT;

/** The same rows, stacked on two lines in a narrow list
 *  (`.os-logs-list[data-layout="narrow"]` in the stylesheet), at the default
 *  16px root. Two lines of text are most of the row, so the row grows with
 *  the reader's font size: stackedRowHeight. */
export const STACKED_ROW_HEIGHT = { comfortable: 48, compact: 40 } as const;

/** STACKED_ROW_HEIGHT at a root font size, in whole pixels. Held at its 16px
 *  value, a compact stacked row's message ran 3px out of its row under the
 *  largest of Chrome's preset font sizes (a 24px root); scaled, the row keeps
 *  the proportions it has at the default. The one-line heights hold their
 *  line at every preset, so they stay as they are. */
export function stackedRowHeight(density: LogDensity, rootPx: number): number {
  return Math.round((STACKED_ROW_HEIGHT[density] * rootPx) / 16);
}

/** The list width below which one line cannot hold the fixed cells and a
 *  readable message, per density, in two parts. MEASURED in a browser with
 *  the brand fonts, at the default 16px root and again at 20px.
 *
 *  `scaled` is sized in characters of rem-sized fonts, so it grows with the
 *  reader's font size: time, level and component (49 + 53 + 121px
 *  comfortable; 45 + 49 + 111px compact), a 32-character message in the mono
 *  voice (230px; 211px) and the subject mark's eighteen characters (119px).
 *  `fixed` is pixels and does not: five gaps (50px; 40px), the row's padding
 *  and rule (20px), the mark's padding and border (16px), the list's border
 *  (2px) and a classic scrollbar (16px). */
export const STACK_BELOW = {
  comfortable: { scaled: 572.5, fixed: 104 },
  compact: { scaled: 534.7, fixed: 94 },
} as const;

/** The list width from which a row holds the fixed cells, the widest mark, a
 *  COMFORTABLE message -- forty characters, a quarter more than the readable
 *  minimum -- and the whole forty-percent attribute column, per density, in
 *  STACK_BELOW's two parts. Derived from the same measurements: the cells,
 *  the mark and the message are sixty percent of the line at most, because
 *  the column takes the other forty, so they and the row's own gaps and mark
 *  padding (66px; 56px) are divided by 0.6 -- time, level and component, the
 *  mark's eighteen characters and forty of the message's (288px; 264px) come
 *  to 630.1px comfortable and 587.5px compact before that -- and the list's
 *  border, the row's padding and rule and a classic scrollbar (38px) are
 *  added after it. */
export const WIDE_FROM = {
  comfortable: { scaled: 1050.2, fixed: 148 },
  compact: { scaled: 979.1, fixed: 131.3 },
} as const;

/** One of the two measures in CSS pixels at a root font size. Only the part
 *  sized in characters scales: scaling the pixels as well would ask too much
 *  under a large font and too little under a small one, where the line
 *  overflows. */
function atRoot(measure: { scaled: number; fixed: number }, rootPx: number): number {
  return Math.ceil((measure.scaled * rootPx) / 16 + measure.fixed);
}

/** The width below which a list's rows stack, at a root font size. */
export function stackBelow(density: LogDensity, rootPx: number): number {
  return atRoot(STACK_BELOW[density], rootPx);
}

/** The width from which a list's attributes are an aligned column, at a root
 *  font size. */
export function wideFrom(density: LogDensity, rootPx: number): number {
  return atRoot(WIDE_FROM[density], rootPx);
}

/** How a list lays its rows out (`.os-logs-list[data-layout]`). */
export type LogLayout = "wide" | "medium" | "narrow";

/** The layout for a list this wide at this root font size. An unmeasured
 *  list (zero wide: a test's DOM, a list not yet laid out) is WIDE, the one
 *  line every row had before R40. */
export function logLayout(width: number, density: LogDensity, rootPx: number): LogLayout {
  if (width <= 0 || width >= wideFrom(density, rootPx)) return "wide";
  return width < stackBelow(density, rootPx) ? "narrow" : "medium";
}

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

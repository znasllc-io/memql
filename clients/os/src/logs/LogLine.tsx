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
// are 30px and compact 22px at the default font, and the windowed list is
// told each row's height (rowHeightAt) so the geometry and the CSS agree.

/** One-line row heights per density at the default 16px root. The windowed
 *  list places every row by rowHeightAt's answer; the stylesheet's line is
 *  `height: 100%` of the row it is given. */
export const ROW_HEIGHT = { comfortable: 30, compact: 22 } as const;

/** A log list's density: the key of every row-geometry table here. */
export type LogDensity = keyof typeof ROW_HEIGHT;

/** The same rows, stacked on two lines in a narrow list
 *  (`.os-logs-list[data-layout="narrow"]` in the stylesheet), at the default
 *  16px root. */
export const STACKED_ROW_HEIGHT = { comfortable: 48, compact: 40 } as const;

/** How much of each height above is TEXT at the default root -- line boxes,
 *  which grow with the reader's font size. The rest is pixels and stays: the
 *  mark's border, the stacked row's 3px gap and the room around the lines.
 *  MEASURED: one line is as tall as its tallest box, the subject mark, whose
 *  line is the root font size (16px) inside a 1px border; two stacked lines
 *  are the mark's line over the message's (15.84px in the mono voice at 12px,
 *  14.52px at 11px). */
const ROW_TEXT = {
  line: { comfortable: 16, compact: 16 },
  stacked: { comfortable: 31.84, compact: 30.52 },
} as const;

/** The list width below which one line cannot hold the fixed cells and a
 *  readable message, per density, in two parts. MEASURED in a browser with
 *  the brand fonts, at the default 16px root and again at 20px.
 *
 *  `scaled` is sized in characters of rem-sized fonts, so it grows with the
 *  reader's font size: time, level and component (49 + 53 + 121px
 *  comfortable; 45 + 49 + 111px compact), a 32-character message in the mono
 *  voice (230px; 211px) and the subject mark's eighteen characters (119px).
 *  `fixed` is pixels and does not: the space after the three fixed cells and
 *  before the mark (4 x 10px; 4 x 8px -- the attributes' own space is inside
 *  them, and they are empty here), the row's padding and rule (20px), the
 *  mark's padding and border (16px), the list's border (2px) and a classic
 *  scrollbar (16px). */
export const STACK_BELOW = {
  comfortable: { scaled: 572.5, fixed: 94 },
  compact: { scaled: 534.7, fixed: 86 },
} as const;

/** The list width from which the message's share of a WIDE row is a
 *  COMFORTABLE message -- sixty characters -- beside the widest mark, per
 *  density, in STACK_BELOW's two parts. Sixty because below it the aligned
 *  column costs a typical line its end: a half-screen window cut a
 *  70-character `ok  github.com/.../pipelinesteps  6.865s` at 46 characters
 *  that MEDIUM shows whole (R40c). Derived from the same measurements. In
 *  WIDE the flexible width (what the fixed cells and the mark's fixed column
 *  leave) goes seventy percent to the message and thirty to the attributes,
 *  so the message's sixty characters (432px; 396px) are divided by 0.7
 *  (617.1px; 565.7px) and added to the fixed cells and the mark's eighteen
 *  characters (342.1px; 323.5px). The pixel part is STACK_BELOW's own -- the
 *  cells' spaces, the mark's padding and border, the row's padding and rule,
 *  the list's border and a classic scrollbar -- because the same things stand
 *  beside the message here. */
export const WIDE_FROM = {
  comfortable: { scaled: 959.3, fixed: 94 },
  compact: { scaled: 889.2, fixed: 86 },
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

/** A row's height in whole pixels in this layout at this root font size.
 *  Only its text scales, as only the characters do in the width measures, so
 *  the room around the lines is the same at every size and no row is ever
 *  shorter than what is in it. */
export function rowHeightAt(layout: LogLayout, density: LogDensity, rootPx: number): number {
  const stacked = layout === "narrow";
  const total = (stacked ? STACKED_ROW_HEIGHT : ROW_HEIGHT)[density];
  const text = ROW_TEXT[stacked ? "stacked" : "line"][density];
  return Math.round((text * rootPx) / 16 + (total - text));
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

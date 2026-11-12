import type { ReactNode } from "react";
import { ChevronRight } from "lucide-react";

import { Caption } from "../Caption";
import { Button, Subhead } from "../controls";
import { formatCount } from "../format";
import { RecordListSkeleton } from "../RecordListSkeleton";
import "./statistics.css";

// THE RANKED LIST of a statistics page (spec 8): the leading values of one
// dimension -- pages, sources, countries, devices, websites, campaigns.
//
// A row is its label, its figure at the trailing edge, and a faint bar behind
// it as long as the figure is against the largest one shown. The bar is the
// soft accent, behind the text rather than beside it, so the list reads as a
// list first and a chart second; it is hidden from assistive technology,
// which has the figure.
//
// THE TOP EIGHT, AND THE REST AS A PAGE. "All pages" stands on the list's
// heading, where a section's own act stands (DESIGN.md rule 1, Subhead
// `actions`), and opens the whole list as a page in the app under the trail;
// never a pop-up (spec 10). The heading keeps its height whether or not the
// act is there, so a list that grows past eight does not move.
//
// FLAT, LIKE EVERY RECORD LIST: no dividers, rows apart by their spacing, a
// whole-row hover wash on a row that opens, and the record list's chevron to
// say it opens. Loading is the record list's shape and never words; empty is
// said only after a read came back empty; a refresh keeps the rows it has.

export interface RankedRow {
  key: string;
  label: string;
  value: number;
  /** One quiet line under the label ("42% opened"). */
  secondary?: string;
  /** Makes the row a button that opens what it names. */
  onOpen?: () => void;
}

/** How many rows a list shows before the rest becomes a page. */
const DEFAULT_LIMIT = 8;

/** A bar's length against the largest value shown, as a CSS width. */
function barWidth(value: number, largest: number): string {
  if (!(largest > 0) || !(value > 0)) return "0%";
  return `${Math.round(Math.min(1, value / largest) * 10_000) / 100}%`;
}

function RowBody({ row, largest, format, opens }: { row: RankedRow; largest: number; format: (value: number) => string; opens: boolean }) {
  return (
    <>
      <span className="os-ranked-bar" aria-hidden="true" style={{ width: barWidth(row.value, largest) }} />
      <span className="os-ranked-text">
        <span className="os-ranked-label" title={row.label}>{row.label}</span>
        {row.secondary ? <span className="os-ranked-secondary">{row.secondary}</span> : null}
      </span>
      <span className="os-ranked-value">{format(row.value)}</span>
      {opens ? <ChevronRight size={14} className="os-ranked-chevron" aria-hidden /> : null}
    </>
  );
}

export function RankedList({
  title,
  rows,
  limit = DEFAULT_LIMIT,
  format = formatCount,
  onShowAll,
  showAllLabel = "Show all",
  footer,
  control,
  loading = false,
  empty = "Nothing yet.",
}: {
  /** The list's heading, and its name to assistive technology. */
  title: string;
  /** Largest first. */
  rows: readonly RankedRow[];
  limit?: number;
  /** How a value is written. Default: a grouped count ("1,204"). */
  format?: (value: number) => string;
  /** Opens the whole list as a page. Offered only when there are more rows than `limit`. */
  onShowAll?: () => void;
  showAllLabel?: string;
  /** A quiet line under the rows (the country data's attribution); drawn only under rows. */
  footer?: ReactNode;
  /**
   * A control that chooses what the list shows (Devices' Device / Browser /
   * OS tabs), drawn under the heading and above the rows, so the heading
   * lines up with the lists beside it and the control reads as the list's own.
   */
  control?: ReactNode;
  /** The read has not answered: with no rows yet, the list's shape is drawn. */
  loading?: boolean;
  /** What an empty answer says: a sentence, or what the page draws for a list it was not given. */
  empty?: ReactNode;
}) {
  const shown = rows.slice(0, Math.max(0, limit));
  const largest = Math.max(0, ...shown.map((row) => (Number.isFinite(row.value) ? row.value : 0)));
  // `null` keeps the heading a row while the act is absent (kit Subhead), so it
  // does not change shape when the list grows past its limit.
  const showAll = onShowAll === undefined ? undefined : rows.length > limit ? <Button tone="quiet" onClick={onShowAll}>{showAllLabel}</Button> : null;

  return (
    <section className="os-ranked-list" aria-label={title}>
      <Subhead actions={showAll}>{title}</Subhead>
      {control ? <div className="os-ranked-control">{control}</div> : null}
      {rows.length === 0 ? (
        loading ? <RecordListSkeleton label={`Loading ${title.toLowerCase()}`} rows={Math.min(Math.max(limit, 1), 5)} /> : <Caption>{empty}</Caption>
      ) : (
        <>
          <ol className="os-ranked-rows">
            {shown.map((row) => (
              <li key={row.key}>
                {row.onOpen ? (
                  <button type="button" className="os-ranked-row" onClick={row.onOpen}>
                    <RowBody row={row} largest={largest} format={format} opens />
                  </button>
                ) : (
                  <div className="os-ranked-row">
                    <RowBody row={row} largest={largest} format={format} opens={false} />
                  </div>
                )}
              </li>
            ))}
          </ol>
          {footer ? <div className="os-ranked-footer">{footer}</div> : null}
        </>
      )}
    </section>
  );
}

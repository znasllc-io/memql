import { useRef, type KeyboardEvent } from "react";

import { InlineSkeleton } from "../ContentSkeleton";
import { formatCount } from "../format";
import { Measure } from "../MeasureView";
import { changeLabel, changeOf, type Figure } from "../measure";
import "./statistics.css";

// THE FIGURE ROW of a statistics page (spec 8).
//
// Each figure is its label, its value (kit `Measure`: an absent figure is an
// em dash with its reason, never a zero) and its change from the previous
// period in quiet text ("+12%", "New", or nothing when there is nothing to
// compare with). The figures the page's series can draw are the options of ONE
// radiogroup, and the checked one is what the chart below them draws; a figure
// the series cannot draw (an average with no per-bucket value) still stands
// in its place, as a plain figure, because a control that changes nothing is
// not a control.
//
// A RADIOGROUP, NOT TABS. The figures do not switch the page; they choose one
// of several values for the same chart, which is what a radio says. The arrow
// keys move the choice (and focus with it), the group is one tab stop, and
// the checked figure is the stop.
//
// THE ROW LOOKS LIKE THE OVERVIEW'S OWN METRICS -- the same label, the same
// 28px figure, the same columns (DESIGN.md, "App overviews": four in a row
// then two; five in a row then three then two, one definition in
// styles/index.css for both) -- so an app whose Overview is its statistics
// draws one figure row, not a second kind of one. Given to an Overview as its
// `figures`, it stands on the metrics' own strip, hairline included.

export interface StatFigure {
  id: string;
  label: string;
  figure: Figure;
  /** The same figure for the previous period. Absent when there is none (twelve months). */
  previous?: Figure;
  /** How a measured value is written. Default: a grouped count ("1,204"). */
  format?: (value: number) => string;
  /** Whether the series carries this figure per bucket, so the chart can draw it. */
  chartable?: boolean;
  /** The read behind it has not answered yet: the figure's SHAPE is drawn, never a caption. */
  loading?: boolean;
}

function FigureBody({ figure }: { figure: StatFigure }) {
  const change = figure.loading ? "" : changeLabel(changeOf(figure.figure, figure.previous));
  return (
    <>
      <span className="os-stat-figure-label" title={figure.label}>{figure.label}</span>
      <span className="os-stat-figure-value">
        {figure.loading
          ? <InlineSkeleton label={`Loading ${figure.label.toLowerCase()}`} />
          : <Measure figure={figure.figure} format={figure.format ?? formatCount} />}
      </span>
      {/* Always present, so a figure with no change keeps the row's height
          and every value stands on the same line. */}
      <span className="os-stat-figure-change">{change}</span>
    </>
  );
}

export function StatFigures({ label, figures, selected, onSelect }: {
  /** Names the group to assistive technology ("Website figures"). */
  label: string;
  figures: readonly StatFigure[];
  /** The id of the figure the chart draws. */
  selected?: string;
  /** Without it nothing is chosen, and every figure is a plain one. */
  onSelect?: (id: string) => void;
}) {
  const choices = onSelect === undefined ? [] : figures.filter((figure) => figure.chartable === true);
  const checked = choices.some((figure) => figure.id === selected) ? selected : undefined;
  // One tab stop: the checked figure, or the first that could be checked.
  const tabStop = checked ?? choices[0]?.id;
  const buttons = useRef(new Map<string, HTMLButtonElement>());

  function onKeyDown(event: KeyboardEvent<HTMLButtonElement>, id: string): void {
    const at = choices.findIndex((figure) => figure.id === id);
    if (at < 0) return;
    const last = choices.length - 1;
    const next =
      event.key === "ArrowRight" || event.key === "ArrowDown" ? (at === last ? 0 : at + 1)
      : event.key === "ArrowLeft" || event.key === "ArrowUp" ? (at === 0 ? last : at - 1)
      : event.key === "Home" ? 0
      : event.key === "End" ? last
      : null;
    if (next === null) return;
    event.preventDefault();
    const target = choices[next];
    if (target === undefined) return;
    onSelect?.(target.id);
    buttons.current.get(target.id)?.focus();
  }

  return (
    <div className="os-stat-figures">
      <div className="os-stat-figures-grid" role={choices.length > 0 ? "radiogroup" : "group"} aria-label={label}>
        {figures.map((figure) =>
          choices.includes(figure) ? (
            <button
              key={figure.id}
              ref={(element) => {
                if (element === null) buttons.current.delete(figure.id);
                else buttons.current.set(figure.id, element);
              }}
              type="button"
              role="radio"
              className="os-stat-figure"
              data-stat-figure={figure.id}
              aria-checked={figure.id === checked}
              tabIndex={figure.id === tabStop ? 0 : -1}
              onClick={() => onSelect?.(figure.id)}
              onKeyDown={(event) => onKeyDown(event, figure.id)}
            >
              <FigureBody figure={figure} />
            </button>
          ) : (
            <div key={figure.id} className="os-stat-figure" data-stat-figure={figure.id}>
              <FigureBody figure={figure} />
            </div>
          ),
        )}
      </div>
    </div>
  );
}

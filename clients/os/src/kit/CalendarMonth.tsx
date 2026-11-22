import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";

import { IconButton } from "./IconButton";

// A MONTH TO PICK ONE DAY FROM, drawn in the OS's own style (Jesús, Nov 20
// 2026: the share view's "Pick a date…" opens "our own calendar, not the
// browser's; past days are greyed out"). The kit had no date picker: the one
// date control, DateTimeInput, is the browser's own field, which a site
// control must never fall back to.
//
// Days are LOCAL calendar days, compared by year, month and day -- never by
// instant, so a time zone cannot shift which square is "today". Days outside
// [min, max] are drawn and disabled, not hidden, so the grid keeps its shape.
//
// ONE TAB STOP: the grid is a roving-focus group. Arrows move a day or a week,
// Page Up/Down a month, Home/End the week's ends, Enter or Space picks.

export interface CalendarDay {
  year: number;
  /** 0-11, as Date counts months. */
  month: number;
  day: number;
}

export function dayOf(date: Date): CalendarDay {
  return { year: date.getFullYear(), month: date.getMonth(), day: date.getDate() };
}

/** -1, 0 or 1 as a is before, the same as, or after b. */
export function compareDays(a: CalendarDay, b: CalendarDay): number {
  return Math.sign(a.year - b.year || a.month - b.month || a.day - b.day);
}

export function addDays(day: CalendarDay, n: number): CalendarDay {
  return dayOf(new Date(day.year, day.month, day.day + n));
}

function addMonths(day: CalendarDay, n: number): CalendarDay {
  // Clamped to the target month's last day, so Jan 31 + 1 is Feb 28, not Mar 3.
  const last = new Date(day.year, day.month + n + 1, 0).getDate();
  return dayOf(new Date(day.year, day.month + n, Math.min(day.day, last)));
}

function clamp(day: CalendarDay, min: CalendarDay, max: CalendarDay): CalendarDay {
  return compareDays(day, min) < 0 ? min : compareDays(day, max) > 0 ? max : day;
}

const WEEKDAYS = ["S", "M", "T", "W", "T", "F", "S"] as const;
const WEEKDAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"] as const;

export function CalendarMonth({
  value,
  min,
  max,
  today,
  onPick,
  label,
  autoFocus = false,
}: {
  value: CalendarDay | null;
  /** The first day that can be picked; earlier ones are disabled. */
  min: CalendarDay;
  /** The last day that can be picked; later ones are disabled. */
  max: CalendarDay;
  today: CalendarDay;
  onPick: (day: CalendarDay) => void;
  /** Names the grid for assistive tech. */
  label: string;
  autoFocus?: boolean;
}) {
  const [focused, setFocused] = useState<CalendarDay>(() => clamp(value ?? today, min, max));
  const [shown, setShown] = useState<{ year: number; month: number }>(() => ({ year: focused.year, month: focused.month }));
  const grid = useRef<HTMLDivElement>(null);
  const moved = useRef(autoFocus);

  // Focus follows the roving day only after the person has moved it (or the
  // caller asked), so opening the calendar never steals focus by surprise.
  useEffect(() => {
    if (!moved.current) return;
    grid.current?.querySelector<HTMLButtonElement>('button[tabindex="0"]')?.focus();
  }, [focused, shown]);

  const move = (next: CalendarDay) => {
    const day = clamp(next, min, max);
    moved.current = true;
    setFocused(day);
    setShown({ year: day.year, month: day.month });
  };

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const steps: Record<string, () => CalendarDay> = {
      ArrowLeft: () => addDays(focused, -1),
      ArrowRight: () => addDays(focused, 1),
      ArrowUp: () => addDays(focused, -7),
      ArrowDown: () => addDays(focused, 7),
      PageUp: () => addMonths(focused, -1),
      PageDown: () => addMonths(focused, 1),
      Home: () => addDays(focused, -new Date(focused.year, focused.month, focused.day).getDay()),
      End: () => addDays(focused, 6 - new Date(focused.year, focused.month, focused.day).getDay()),
    };
    const step = steps[event.key];
    if (!step) return;
    event.preventDefault();
    move(step());
  };

  const first = new Date(shown.year, shown.month, 1);
  const daysInMonth = new Date(shown.year, shown.month + 1, 0).getDate();
  // Six rows always, so the grid does not change height between months.
  const cells = Array.from({ length: 42 }, (_, i) => dayOf(new Date(shown.year, shown.month, 1 - first.getDay() + i)));
  const title = first.toLocaleDateString(undefined, { month: "long", year: "numeric" });
  const canBack = compareDays({ year: shown.year, month: shown.month, day: 1 }, { ...min, day: 1 }) > 0;
  const canForward = compareDays({ year: shown.year, month: shown.month, day: daysInMonth }, max) < 0;
  const page = (n: number) => {
    const target = addMonths({ year: shown.year, month: shown.month, day: 1 }, n);
    setShown({ year: target.year, month: target.month });
    setFocused(clamp({ ...target, day: Math.min(focused.day, new Date(target.year, target.month + 1, 0).getDate()) }, min, max));
  };

  return (
    <div className="os-calendar">
      <div className="os-calendar-head">
        <IconButton label="Previous month" disabled={!canBack} onClick={() => page(-1)}>
          <ChevronLeft size={14} aria-hidden />
        </IconButton>
        <span className="os-calendar-title" aria-live="polite">{title}</span>
        <IconButton label="Next month" disabled={!canForward} onClick={() => page(1)}>
          <ChevronRight size={14} aria-hidden />
        </IconButton>
      </div>
      <div ref={grid} className="os-calendar-grid" role="grid" aria-label={`${label}, ${title}`} onKeyDown={onKeyDown}>
        <div role="row" className="os-calendar-week">
          {WEEKDAYS.map((name, i) => (
            <span key={i} role="columnheader" className="os-calendar-weekday" aria-label={WEEKDAY_NAMES[i]}>{name}</span>
          ))}
        </div>
        {Array.from({ length: 6 }, (_, row) => (
          <div key={row} role="row" className="os-calendar-week">
            {cells.slice(row * 7, row * 7 + 7).map((day) => {
              const outside = day.month !== shown.month;
              const disabled = compareDays(day, min) < 0 || compareDays(day, max) > 0;
              const picked = value !== null && compareDays(day, value) === 0;
              const isFocused = compareDays(day, focused) === 0;
              return (
                <span key={`${day.month}-${day.day}`} role="gridcell" aria-selected={picked}>
                  <button
                    type="button"
                    className="os-calendar-day"
                    data-outside={outside || undefined}
                    data-today={compareDays(day, today) === 0 || undefined}
                    data-picked={picked || undefined}
                    disabled={disabled}
                    tabIndex={isFocused && !disabled ? 0 : -1}
                    aria-label={new Date(day.year, day.month, day.day).toLocaleDateString(undefined, { weekday: "long", month: "long", day: "numeric", year: "numeric" })}
                    onClick={() => onPick(day)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault();
                        event.stopPropagation();
                        onPick(day);
                      }
                    }}
                    onFocus={() => { if (!isFocused) setFocused(day); }}
                  >
                    {day.day}
                  </button>
                </span>
              );
            })}
          </div>
        ))}
      </div>
    </div>
  );
}

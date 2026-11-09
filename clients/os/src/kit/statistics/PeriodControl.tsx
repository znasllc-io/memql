import { Select } from "../controls";
import { isPeriod, periodLabel, type Period } from "./period";

// THE PERIOD CONTROL (spec 8): the kit's quiet select at the right of a Head
// or Subhead, reading "Last 30 days", the way SortControl reads "Newest
// first". It lists only the periods the page's read offers, and hands back
// only one of them. Where the choice is remembered is the page's (its app's
// settings document), not the control's.

export function PeriodControl({ id, value, periods, onChange }: {
  id: string;
  value: Period;
  /** What the page's read offers, in order (`SITE_PERIODS`, `EMAIL_PERIODS`). */
  periods: readonly Period[];
  onChange: (period: Period) => void;
}) {
  return (
    <Select
      tone="quiet"
      id={id}
      label="Period"
      title="Period"
      value={value}
      onChange={(next) => {
        if (isPeriod(next, periods)) onChange(next);
      }}
    >
      {periods.map((period) => (
        <option key={period} value={period}>
          {periodLabel(period)}
        </option>
      ))}
    </Select>
  );
}

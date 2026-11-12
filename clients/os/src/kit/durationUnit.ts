// A LENGTH OF TIME A TESTER SHORTENS (Jesús, 2026-11-10 and 2026-11-12): a
// number with a Days / Minutes switch, so "5 days" becomes "5 minutes" to walk a
// flow end to end, then goes back. Email > Automations' waits and Reviews >
// Settings' wait before asking show the same two choices in a ChoiceRow labelled
// "Wait unit" (DESIGN.md, "Details and settings content"). Switching to Minutes
// keeps the length of time (5 days reads 7,200); switching to Days rounds to
// whole days.

export type DurationUnit = "days" | "minutes";

export const MINUTES_PER_DAY = 1440;

/** The two choices, for the ChoiceRow labelled "Wait unit". */
export const DURATION_UNITS: readonly { value: DurationUnit; label: string }[] = [
  { value: "days", label: "Days" },
  { value: "minutes", label: "Minutes" },
];

/** A length shown in a unit: Minutes keeps it exactly; Days rounds to whole days, at least `minDays`. */
export function durationIn(totalMinutes: number, unit: DurationUnit, minDays = 0): number {
  return unit === "minutes" ? totalMinutes : Math.max(minDays, Math.round(totalMinutes / MINUTES_PER_DAY));
}

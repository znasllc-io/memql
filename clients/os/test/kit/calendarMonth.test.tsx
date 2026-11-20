import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { addDays, CalendarMonth, compareDays, type CalendarDay } from "../../src/kit/CalendarMonth";

// The kit's own calendar (Nov 20 2026): the share view's "Pick a date…". Days
// are local calendar days; days outside [min, max] are drawn and disabled.

const today: CalendarDay = { year: 2026, month: 10, day: 20 }; // Fri, Nov 20 2026
const label = (d: CalendarDay) =>
  new Date(d.year, d.month, d.day).toLocaleDateString(undefined, { weekday: "long", month: "long", day: "numeric", year: "numeric" });
const day = (d: CalendarDay) => screen.getByRole("button", { name: label(d) }) as HTMLButtonElement;

describe("calendar days", () => {
  it("compare and step by local calendar day, across a month's end", () => {
    expect(compareDays(today, { ...today, day: 21 })).toBe(-1);
    expect(compareDays(today, today)).toBe(0);
    expect(addDays(today, 11)).toEqual({ year: 2026, month: 11, day: 1 });
    expect(addDays({ year: 2026, month: 0, day: 1 }, -1)).toEqual({ year: 2025, month: 11, day: 31 });
  });
});

describe("CalendarMonth", () => {
  function draw(onPick = vi.fn()) {
    render(<CalendarMonth value={null} min={today} max={addDays(today, 89)} today={today} label="End" onPick={onPick} />);
    return onPick;
  }

  it("greys out the days before the first one that can be picked", () => {
    const onPick = draw();
    expect(day({ ...today, day: 19 }).disabled).toBe(true);
    expect(day(today).disabled).toBe(false);
    fireEvent.click(day({ ...today, day: 19 }));
    expect(onPick).not.toHaveBeenCalled();
    fireEvent.click(day({ ...today, day: 26 }));
    expect(onPick).toHaveBeenCalledWith({ year: 2026, month: 10, day: 26 });
  });

  it("is one tab stop, moved by the arrow keys and picked with Enter", () => {
    const onPick = draw();
    const focusable = screen.getAllByRole("button").filter((b) => b.getAttribute("tabindex") === "0" && b.className === "os-calendar-day");
    expect(focusable).toHaveLength(1);
    fireEvent.keyDown(screen.getByRole("grid"), { key: "ArrowDown" });
    expect(day({ ...today, day: 27 }).getAttribute("tabindex")).toBe("0");
    // Never onto a day that cannot be picked.
    fireEvent.keyDown(screen.getByRole("grid"), { key: "ArrowUp" });
    fireEvent.keyDown(screen.getByRole("grid"), { key: "ArrowUp" });
    expect(day(today).getAttribute("tabindex")).toBe("0");
    fireEvent.keyDown(day(today), { key: "Enter" });
    expect(onPick).toHaveBeenCalledWith(today);
  });

  it("pages months, but not before the first day or past the last", () => {
    draw();
    expect((screen.getByRole("button", { name: "Previous month" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Next month" }));
    fireEvent.click(screen.getByRole("button", { name: "Next month" }));
    fireEvent.click(screen.getByRole("button", { name: "Next month" }));
    // Nov 20 + 89 days is Feb 17 2027: February is the last month offered.
    expect(screen.getByRole("grid").getAttribute("aria-label")).toContain("2027");
    expect((screen.getByRole("button", { name: "Next month" }) as HTMLButtonElement).disabled).toBe(true);
    expect(day({ year: 2027, month: 1, day: 17 }).disabled).toBe(false);
    expect(day({ year: 2027, month: 1, day: 18 }).disabled).toBe(true);
  });
});

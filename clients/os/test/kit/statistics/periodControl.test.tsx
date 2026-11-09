import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { PeriodControl } from "../../../src/kit/statistics/PeriodControl";
import { EMAIL_PERIODS, SITE_PERIODS } from "../../../src/kit/statistics/period";
import { chooseOption, openSelect, selectedLabel } from "../../selectControl";

afterEach(cleanup);

// THE PERIOD CONTROL (spec 8): a quiet menu at the right of a heading reading
// "Last 30 days". It lists only what the page's read offers, so Email, whose
// facts are counted by the day, never offers the last 24 hours.

describe("the period control", () => {
  it("period control offers only the given periods", () => {
    const onChange = vi.fn();
    render(<PeriodControl id="email-period" value="30d" periods={EMAIL_PERIODS} onChange={onChange} />);
    const trigger = screen.getByLabelText("Period");
    expect(selectedLabel(trigger)).toBe("Last 30 days");
    const list = openSelect(trigger);
    expect(within(list).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "Last 7 days",
      "Last 30 days",
      "Last 90 days",
      "Last 12 months",
    ]);
    chooseOption(trigger, "Last 90 days");
    expect(onChange).toHaveBeenCalledWith("90d");
  });

  it("offers the last 24 hours where the page reads by the hour", () => {
    render(<PeriodControl id="site-period" value="7d" periods={SITE_PERIODS} onChange={() => {}} />);
    const list = openSelect(screen.getByLabelText("Period"));
    expect(within(list).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "Last 24 hours",
      "Last 7 days",
      "Last 30 days",
      "Last 90 days",
      "Last 12 months",
    ]);
  });

  it("is the kit's quiet select, so it sits on a heading without a box", () => {
    render(<PeriodControl id="site-period" value="7d" periods={SITE_PERIODS} onChange={() => {}} />);
    expect(screen.getByLabelText("Period").closest(".os-select-wrap-quiet")).not.toBeNull();
  });
});

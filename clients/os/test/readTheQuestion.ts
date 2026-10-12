import { act } from "@testing-library/react";
import { vi } from "vitest";

import { CONFIRM_SETTLE_MS } from "../src/kit/ActionBar";

/**
 * What a person does between an ask and its answer: reads the question.
 *
 * An act that took the place of the one just pressed, under the same label,
 * ignores presses for `CONFIRM_SETTLE_MS` (kit/ActionBar): pressed at once, it
 * is the second click of the double-click that asked, and the bar does not
 * take it. A test that confirms waits that long first, as a person does -- on
 * the clock the test runs on, real or fake.
 */
export async function readTheQuestion(): Promise<void> {
  await act(async () => {
    if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(CONFIRM_SETTLE_MS);
    else await new Promise((resolve) => setTimeout(resolve, CONFIRM_SETTLE_MS));
  });
}

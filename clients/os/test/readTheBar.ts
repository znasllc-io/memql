import { act } from "@testing-library/react";
import { vi } from "vitest";

import { CONFIRM_SETTLE_MS } from "../src/kit/ActionBar";

/**
 * What a person does between a bar changing and pressing it: reads it.
 *
 * A bar whose acts have just changed -- into a question, back from one, to the
 * next step of a wizard, or because what it is about changed -- takes no press
 * for `CONFIRM_SETTLE_MS` (kit/ActionBar): pressed at once, any act is the
 * second click of the double-click that changed it. A test that presses a bar
 * it has just changed waits that long first, as a person does -- on the clock
 * the test runs on, real or fake.
 */
export async function readTheBar(): Promise<void> {
  await act(async () => {
    if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(CONFIRM_SETTLE_MS);
    else await new Promise((resolve) => setTimeout(resolve, CONFIRM_SETTLE_MS));
  });
}

/**
 * The same moment, for a synchronous test whose Date is fake
 * (`vi.useFakeTimers({ toFake: ["Date"] })`): the clock moves past it at once,
 * and nothing else runs.
 */
export function readTheBarNow(): void {
  if (!vi.isFakeTimers()) throw new Error("readTheBarNow needs a fake Date: vi.useFakeTimers({ toFake: [\"Date\"] })");
  vi.setSystemTime(Date.now() + CONFIRM_SETTLE_MS);
}

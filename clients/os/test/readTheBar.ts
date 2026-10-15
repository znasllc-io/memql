import { act } from "@testing-library/react";
import { afterEach, beforeEach, vi } from "vitest";

import { CONFIRM_SETTLE_MS } from "../src/kit/pressHold";

/**
 * What a person does between a bar changing and pressing it: reads it.
 *
 * A bar whose acts have just changed -- into a question, back from one, to the
 * next step of a wizard, or because what it is about changed -- takes no press
 * for `CONFIRM_SETTLE_MS` (kit/pressHold): pressed at once, any act is the
 * second click of the double-click that changed it. The same holds for a
 * question answered in place, outside any bar (`InPlaceAsk`). A test that
 * presses what has just changed waits that long first, as a person does -- on
 * the clock the test runs on, real or fake.
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

/** The gap between the two clicks of a double-click. */
export const DOUBLE_CLICK_MS = 50;

/**
 * The second click of a double-click is a moment after the first: on a fake
 * Date, the clock moves that far and nothing else runs. A test that presses
 * again straight after this is the person who clicked twice, not the one who
 * read what the first click put there (`readTheBar`).
 */
export function betweenTheClicks(): void {
  if (!vi.isFakeTimers()) throw new Error("betweenTheClicks needs a fake Date: vi.useFakeTimers({ toFake: [\"Date\"] })");
  vi.setSystemTime(Date.now() + DOUBLE_CLICK_MS);
}

/**
 * Run the tests of the describe block it is called in on a fake Date, so a
 * test can place its presses in time (`betweenTheClicks`, `readTheBar`) without
 * waiting. Only the Date is fake: timers, promises and polling run as they do.
 */
export function useFakeDate(): void {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
  });
  afterEach(() => {
    vi.useRealTimers();
  });
}

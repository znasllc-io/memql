import { act } from "@testing-library/react";
import { vi } from "vitest";

import { CONFIRM_SETTLE_MS } from "../src/kit/ActionBar";

/**
 * What a person does between an ask and its answer: reads the question.
 *
 * A bar that has just turned into a question takes no press for
 * `CONFIRM_SETTLE_MS` (kit/ActionBar), and neither does an ask back in the
 * place just pressed: pressed at once, either is the second click of the
 * double-click that asked. A test that answers a question waits that long
 * first, as a person does -- on the clock the test runs on, real or fake.
 */
export async function readTheQuestion(): Promise<void> {
  await act(async () => {
    if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(CONFIRM_SETTLE_MS);
    else await new Promise((resolve) => setTimeout(resolve, CONFIRM_SETTLE_MS));
  });
}

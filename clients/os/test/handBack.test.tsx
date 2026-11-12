import { cleanup, render, screen } from "@testing-library/react";
import { useEffect, useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); });

const HOLD_MS = 3;

/**
 * What a read brings in: nothing at first, then content with a mount effect.
 * The read settles while a wait has act turned off, so React commits the
 * content on its own scheduler and queues that commit's effects as a later
 * task.
 */
function Arrives({ read, onMountEffect }: { read: Promise<void>; onMountEffect: () => void }) {
  const [arrived, setArrived] = useState(false);
  useEffect(() => { void read.then(() => setArrived(true)); }, [read]);
  return arrived ? <Arrived onMountEffect={onMountEffect} /> : null;
}

function Arrived({ onMountEffect }: { onMountEffect: () => void }) {
  useEffect(onMountEffect, [onMountEffect]);
  return <p>arrived</p>;
}

function contentAReadBringsIn() {
  let arrive!: () => void;
  const read = new Promise<void>((resolve) => { arrive = resolve; });
  const seen = { mountEffectRan: false };
  render(<Arrives read={read} onMountEffect={() => { seen.mountEffectRan = true; }} />);
  return { arrive, seen };
}

/**
 * Holds the thread for a few milliseconds straight after each zero-delay
 * timer is armed, which is what a busy runner does to a test at random.
 *
 * Testing Library hands a wait back on a zero-delay timer. Node runs a timer
 * that has come due before the immediates queued ahead of it, and React runs a
 * commit's effects in an immediate. So the hold makes the hand-back win a race
 * it loses on an idle machine about 299 times in 300. Without the wait that
 * test/setup.ts adds, a test then resumes with the effects of the content it
 * waited for still to come.
 *
 * Returns how many timers it held, so a test can prove it forced that order.
 */
function holdAfterEachZeroDelayTimer(): () => number {
  const arm = globalThis.setTimeout;
  let held = 0;
  vi.stubGlobal("setTimeout", (handler: () => void, delay?: number) => {
    const timer = arm(handler, delay);
    if (delay === 0) {
      held++;
      const until = performance.now() + HOLD_MS;
      while (performance.now() < until) { /* the thread is not given up */ }
    }
    return timer;
  });
  return () => held;
}

describe("a Testing Library wait in this suite", () => {
  it("hands back after the mount effects of the content it waited for, however the timers fall", async () => {
    const { arrive, seen } = contentAReadBringsIn();
    const held = holdAfterEachZeroDelayTimer();

    const found = screen.findByText("arrived");
    arrive();
    const paragraph = await found;

    expect(held(), "no zero-delay timer was armed, so this test no longer forces the order it is about").toBeGreaterThan(0);
    expect(paragraph.textContent).toBe("arrived");
    expect(seen.mountEffectRan).toBe(true);
  });

  it("still hands back in a test that has faked setImmediate", async () => {
    vi.useFakeTimers({ toFake: ["setImmediate", "clearImmediate"] });
    const { arrive } = contentAReadBringsIn();

    const found = screen.findByText("arrived");
    arrive();

    expect((await found).textContent).toBe("arrived");
  });
});

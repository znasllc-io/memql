import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { browserZone, elapsedClock, localToInstant, shortDay, shortMoment, whenWords } from "../../src/apps/email/when";

// WHEN A CAMPAIGN GOES, IN A PERSON'S OWN CLOCK (memql#6033).
//
// A date and time is picked as the browser's own `datetime-local` string -- the
// viewer's zone, no offset -- and the engine takes an instant. These are the
// conversions and the sentences, pinned in one zone with no daylight saving so
// an instant is the same answer on every machine that runs the suite.

const ZONE = "America/Phoenix";
let previousZone: string | undefined;

beforeAll(() => {
  previousZone = process.env.TZ;
  process.env.TZ = ZONE;
});

afterAll(() => {
  if (previousZone === undefined) delete process.env.TZ;
  else process.env.TZ = previousZone;
});

describe("a local date and time as an instant", () => {
  it("reads the value on this browser's clock and answers in UTC", () => {
    expect(localToInstant("2026-10-14T09:00")).toBe("2026-10-14T16:00:00Z");
    // The control's own seconds form, which some browsers send.
    expect(localToInstant("2026-10-14T09:00:30")).toBe("2026-10-14T16:00:30Z");
  });

  it("is null for anything that is not a date and time", () => {
    for (const value of [
      "",
      "garbage",
      "2026-10-14",
      "09:00",
      "2026-13-01T09:00",
      "2026-02-30T09:00",
      "2026-10-14T24:00",
      "2026-10-14T09:60",
      "14/10/2026 09:00",
      "2026-10-14T09:00Z",
    ]) {
      expect(localToInstant(value), value).toBeNull();
    }
  });
});

describe("the zone", () => {
  it("names this browser's zone", () => {
    expect(browserZone()).toBe(ZONE);
  });
});

describe("when, in words", () => {
  it("says now, a moment, or a cadence from a moment", () => {
    expect(whenWords({ kind: "now" }, ZONE)).toBe("now");
    // October 14, 2031 is a Tuesday.
    expect(whenWords({ kind: "schedule", at: "2031-10-14T09:00" }, ZONE)).toBe("on Tue, Oct 14 at 9:00");
    expect(whenWords({ kind: "repeat", weeks: 2, at: "2031-10-14T09:00" }, ZONE)).toBe("every 2 weeks from Tue, Oct 14 at 9:00");
    expect(whenWords({ kind: "repeat", weeks: 1, at: "2031-10-14T09:00" }, ZONE)).toBe("every week from Tue, Oct 14 at 9:00");
  });

  it("reads the clock on a 24-hour face, so an afternoon is never mistaken for a morning", () => {
    expect(whenWords({ kind: "schedule", at: "2031-10-14T14:30" }, ZONE)).toBe("on Tue, Oct 14 at 14:30");
    expect(whenWords({ kind: "schedule", at: "2031-10-14T00:05" }, ZONE)).toBe("on Tue, Oct 14 at 0:05");
  });

  it("reads the moment on the zone it is told", () => {
    // 9:00 in Phoenix is 18:00 in Berlin (summer time).
    expect(whenWords({ kind: "schedule", at: "2031-07-15T09:00" }, "Europe/Berlin")).toBe("on Tue, Jul 15 at 18:00");
  });

  it("gives a row its short forms: a day and time, and a day", () => {
    const instant = localToInstant("2031-10-14T09:00")!;
    expect(shortMoment(instant)).toBe("Oct 14, 9:00");
    expect(shortDay(localToInstant("2026-10-03T12:00")!)).toBe("Oct 3");
    // Not an instant: said as it is, never a blank.
    expect(shortMoment("soon")).toBe("soon");
  });

  it("measures a wait in minutes and seconds", () => {
    expect(elapsedClock(0)).toBe("0:00");
    expect(elapsedClock(42_400)).toBe("0:42");
    expect(elapsedClock(125_000)).toBe("2:05");
    expect(elapsedClock(-5_000)).toBe("0:00");
  });
});

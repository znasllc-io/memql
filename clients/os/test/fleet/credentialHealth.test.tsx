import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

// What a machine's CONNECTION can be wrong about, and when the Fleet says so
// (epic memql#5327, designs D4 and D6).
//
// ===========================================================================
// THE TWO FAILURES THIS COVERS ARE BOTH SILENT WITHOUT IT
// ===========================================================================
// A credential that expires disconnects a machine on a date, permanently,
// with nothing on the page having led up to it. A clock far enough out made a
// machine read OFFLINE while it was connected and beating -- the symptom
// design D6 fixed at the source, and this is the explanation for anybody who
// has already seen it.
//
// Both are on the facts list too, always. What is tested here is the half
// that goes looking for the reader rather than waiting to be found -- which
// means the assertions that matter most are the ones about SILENCE: a
// component that warned about everything would be a component nobody reads.

const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { CredentialHealth } = await import("../../src/apps/fleet/machines/CredentialHealth");
const { machineRow } = await import("./harness");

const NOW = new Date("2026-09-22T12:00:00Z");

function show(over: Record<string, unknown>) {
  return render(
    <CredentialHealth
      machine={machineFromRow(machineRow({ id: "v1:worker:registration:a", displayName: "Alpha", ...over }))}
      now={NOW}
    />,
  );
}

afterEach(cleanup);

describe("the credential warning", () => {
  it("says nothing about a token that does not expire", () => {
    // EVERY TOKEN MINTED BEFORE EXPIRY EXISTED IS HERE, so this is the common
    // case rather than an edge one. "No expiry" is not a problem to fix and
    // not a thing to warn about -- rotation gives it one, without a person.
    const { container } = show({ credentialExpiresAt: "" });
    expect(container.textContent).toBe("");
  });

  it("says nothing about a token that expires months from now", () => {
    show({ credentialExpiresAt: "2026-12-20T12:00:00Z" });
    expect(screen.queryByText(/credential/i)).toBeNull();
  });

  it("warns when the credential expires inside the window, and names the remedy", () => {
    show({ credentialExpiresAt: "2026-09-28T12:00:00Z" });
    expect(screen.getByText(/Alpha's credential expires/)).toBeTruthy();
    // THE REMEDY IS THE POINT. A date on its own leaves a person to work out
    // whether they have to do anything, and the answer -- "nothing, if the
    // machine stays on" -- is not guessable.
    expect(screen.getByText(/Cockpit renews it while the machine is connected/)).toBeTruthy();
  });

  it("reports an expired credential as an error, not a warning", () => {
    // THE DIFFERENCE IS WHETHER SOMETHING CAN STILL BE DONE. Before the date
    // the machine renews itself; after it, the only route back is pairing
    // again, and the two must not read alike.
    show({ credentialExpiresAt: "2026-09-01T12:00:00Z" });
    expect(screen.getByText(/credential expired/)).toBeTruthy();
    expect(screen.getByText(/Pair it again/)).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
  });

  it("treats an unreadable date as no expiry rather than as an expired one", () => {
    // READING IT AS EXPIRED WOULD DECLARE A WORKING MACHINE DEAD on a string
    // nobody can parse. The raw value is still on the facts list for anybody
    // investigating; this surface stays quiet.
    const { container } = show({ credentialExpiresAt: "not a date" });
    expect(container.textContent).toBe("");
  });

  it("says nothing at all about a machine that has already been removed", () => {
    // Neither warning leads anywhere on a revoked row: its credential
    // expiring is moot and its clock decides nothing.
    const { container } = show({
      credentialExpiresAt: "2026-09-01T12:00:00Z",
      clockSkewMs: -120000,
      revokedAt: "2026-09-10T00:00:00Z",
    });
    expect(container.textContent).toBe("");
  });
});

describe("the clock warning", () => {
  it("says nothing when the cockpit reports no timestamp", () => {
    // ABSENT IS NOT ZERO. A cockpit that stamps no timestamp has said nothing
    // about its clock, and dressing that as agreement would be an invented
    // fact about a machine nobody measured.
    const { container } = show({ credentialExpiresAt: "" });
    expect(container.textContent).toBe("");
  });

  it("says nothing about a clock that agrees", () => {
    const { container } = show({ credentialExpiresAt: "", clockSkewMs: 0 });
    expect(container.textContent).toBe("");
  });

  it("says nothing about a skew below the online window", () => {
    // BELOW THE WINDOW IT IS A CURIOSITY. The threshold is not a round number
    // -- it is the figure at which the machine's own timestamps would have put
    // it outside the window that decides whether it shows as online.
    const { container } = show({ credentialExpiresAt: "", clockSkewMs: 12000 });
    expect(container.textContent).toBe("");
  });

  it("names the direction, not a signed number", () => {
    // "-47s" makes a reader work out which way round the subtraction went.
    // Somebody checking an NTP setting needs the word.
    show({ credentialExpiresAt: "", clockSkewMs: -47000 });
    expect(screen.getByText(/47 seconds behind the cluster's/)).toBeTruthy();
  });

  it("names the other direction too", () => {
    show({ credentialExpiresAt: "", clockSkewMs: 47000 });
    expect(screen.getByText(/47 seconds ahead of the cluster's/)).toBeTruthy();
  });

  it("reads a large skew in minutes", () => {
    show({ credentialExpiresAt: "", clockSkewMs: -300000 });
    expect(screen.getByText(/5 minutes behind/)).toBeTruthy();
  });

  it("says that nothing here is wrong because of it", () => {
    // THE HONEST PART. Since design D6 the cluster times a machine by its own
    // clock, so a skewed machine is NOT offline, NOT slow, and NOT misrouted.
    // A warning that implied otherwise would send somebody chasing a fault
    // that is not there.
    show({ credentialExpiresAt: "", clockSkewMs: 60000 });
    expect(screen.getByText(/Nothing here is timed by it/)).toBeTruthy();
  });
});

describe("both at once", () => {
  it("says both things rather than the worse one", () => {
    // TWO SEPARATE REPAIRS, so two lines. Collapsing to whichever is more
    // severe sends somebody to fix one thing and leaves the other for the
    // next time they look.
    show({ credentialExpiresAt: "2026-09-25T12:00:00Z", clockSkewMs: 90000 });
    expect(screen.getByText(/credential expires/)).toBeTruthy();
    expect(screen.getByText(/90 seconds ahead/)).toBeTruthy();
  });
});

describe("the offset's figure", () => {
  it("reads in the unit that fits its size", async () => {
    // THE DEFECT THIS PINS was visible only in a rendered capture: the facts
    // list showed `-212000 ms`, directly under a round trip written as
    // `34 ms, checked 30s ago`. Nobody reads 212000 ms as three and a half
    // minutes. Milliseconds are right for a clock that is nearly right and
    // useless for one that is not.
    const { formatClockOffset } = await import("../../src/apps/fleet/rows");
    expect(formatClockOffset(40)).toBe("40 ms ahead");
    expect(formatClockOffset(-40)).toBe("40 ms behind");
    expect(formatClockOffset(47000)).toBe("47s ahead");
    expect(formatClockOffset(-212000)).toBe("3m 32s behind");
    expect(formatClockOffset(-300000)).toBe("5m behind");
  });

  it("calls a measured zero agreement, not a signed zero", async () => {
    // "0 ms ahead" reads as a measurement that came out oddly. The clocks
    // agreeing is the answer somebody is actually looking for.
    const { formatClockOffset } = await import("../../src/apps/fleet/rows");
    expect(formatClockOffset(0)).toBe("in step with the cluster");
  });

  it("agrees with the sentence above it", async () => {
    // ONE READING OF THE NUMBER, TWO RENDERINGS. The list wants it terse and
    // the advisory wants it in prose, but "212000 ms" and "3m 32s" must never
    // be two answers to one question.
    const { formatClockOffset } = await import("../../src/apps/fleet/rows");
    show({ credentialExpiresAt: "", clockSkewMs: -212000 });
    expect(formatClockOffset(-212000)).toContain("3m 32s");
    expect(screen.getByText(/3 minutes 32 seconds behind/)).toBeTruthy();
  });
});

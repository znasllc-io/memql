import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readTheBar, useFakeDate } from "../readTheBar";
import { seededAccessFor, seededAccessWithout } from "../seededAccess";
import { chooseOption, openSelect, selectedLabel } from "../selectControl";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted -- the
// shape deployables.test.tsx already uses. The fake beneath it answers at
// `executeNamed`, so the generated builders run and the STRING is what is asserted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { AccessCodesPanel } from "../../src/apps/deployables/access/AccessCodesPanel";
import { expiryWord, lastOpenedWord } from "../../src/apps/deployables/access/expiry";
import { accessCodesFromRows } from "../../src/apps/deployables/access/useAccessCodes";
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { formatMoment } from "../../src/kit/format";
import { setEffectiveCapabilities, type OrganizationCapability } from "../../src/system/roles";
import {
  FIXTURE_ACCESS_CODES,
  PLATFORM_SITE,
  accessCodeRow,
  accessGateRow,
  click,
  droppedSocket,
  fakeConnection,
  loseTheReply,
  refusedByEngine,
  siteRow,
  type as typeInto,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// THE ACCESS CODES PANEL (design record 2026-10-29-deployable-access-codes,
// section 5; clients/os/DESIGN.md rules 7, 8 and 12).
//
// WHAT THIS SURFACE CAN GET WRONG, in the order it would hurt:
//
//   - by leaving a working credential somewhere it can be read again: a row, a
//     store, the screen after the person has moved on;
//   - by letting a double-click press what the first click put there, which here
//     can throw away a code that is shown once;
//   - by drawing a refusal, or a read that failed, as an empty list;
//   - by offering an act the engine will refuse, or claiming an expiry or a last
//     opened it does not know.
//
// Every case runs on a fixed Date, so a time on the wire is a literal and a press
// can be placed against the hold (`readTheBar`).

const NOW = Date.parse("2026-10-30T12:00:00.000Z");
const inMinutes = (n: number): string => new Date(NOW + n * 60_000).toISOString();
const inDays = (n: number): string => inMinutes(n * 24 * 60);

const DESK = siteRow({
  id: "site-desk",
  hostname: "desk.memql.example.com",
  packageId: "pkg-desk",
  packageDeployableName: "client-desk",
});

const DASHBOARD = accessGateRow({ gate: "dashboard" });
const ENGAGEMENT = accessGateRow({ gate: "engagement", usesSubject: true });
const [FIRST_CODE, SECOND_CODE] = FIXTURE_ACCESS_CODES;
const SUBJECT = "Subject (optional)";

function reviewers(over: Partial<Row> = {}): Row {
  return accessCodeRow({ id: "code-desk", label: "Client reviewers", ...over });
}

function pilot(over: Partial<Row> = {}): Row {
  return accessCodeRow({ id: "code-pilot", label: "Pilot client", gate: "dashboard", codeHint: "3XPA", ...over });
}

function mountPanel(seed: FakeSeed, opts: { site?: Row; connection?: FakeConnection } = {}) {
  const connection = opts.connection ?? fakeConnection(seed);
  h.connection = connection;
  const view = render(withSession(<AccessCodesPanel site={siteFromRow(opts.site ?? DESK)} />));
  return { connection, ...view };
}

/** Decisions for a person who holds the access part in these organizations and no others. */
function holdingIn(...accounts: string[]): OrganizationCapability[] {
  return accounts.flatMap((accountId) =>
    [["read", "data"], ["read", "app:deployables"], ["update", "data"], ["execute", "app:deployables/access"]].map(([verb, resource]) => ({
      accountId,
      verb: verb!,
      resource: resource!,
      effect: "allow" as const,
    })),
  );
}

/** The panel as somebody who is no cluster operator sees it: their organizations decide. */
function mountHolding(seed: FakeSeed, site: Row, ...accounts: string[]) {
  const connection = fakeConnection(seed);
  h.connection = connection;
  const tree = withSession(<AccessCodesPanel site={siteFromRow(site)} />, { role: "writer" });
  setEffectiveCapabilities(seededAccessFor("writer"), holdingIn(...accounts));
  return { connection, ...render(tree) };
}

const addControl = () => screen.findByRole("button", { name: "Add an access code" });

async function rowOf(label: string): Promise<HTMLElement> {
  return (await screen.findByText(label)).closest("li") as HTMLElement;
}

/** Presses a row's act and reads the question it asks. */
async function ask(label: string, act: "Regenerate" | "Remove" | "Change expiry of"): Promise<HTMLElement> {
  await click(await screen.findByRole("button", { name: `${act} ${label}` }));
  await readTheBar();
  return rowOf(label);
}

/** Presses a row's act, reads the question, and answers yes. */
async function answerYes(label: string, act: "Regenerate" | "Remove"): Promise<void> {
  const row = await ask(label, act);
  await click(within(row).getByRole("button", { name: act }));
}

/** Opens the Add form and fills what every code needs. */
async function fillAdd(name: string): Promise<void> {
  await click(await addControl());
  await typeInto(screen.getByLabelText("Name") as HTMLInputElement, name);
}

/**
 * Holds what the named call answers until released, having taken its answer
 * when it was asked: a read in flight that comes back with the world as it was.
 */
function holdAnswers(connection: FakeConnection, construct: string): () => void {
  const real = connection.query.executeNamed.bind(connection.query);
  let release = (): void => {};
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  connection.query.executeNamed = (async (name: string, call: string, opts?: object) => {
    const answer = await real(name, call, opts);
    if (name === construct) await gate;
    return answer;
  }) as typeof connection.query.executeNamed;
  return release;
}

useFakeDate();

beforeEach(() => {
  vi.setSystemTime(NOW);
  localStorage.clear();
  sessionStorage.clear();
  h.connection = null;
});

// ---------------------------------------------------------------------------
// The way in
// ---------------------------------------------------------------------------

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

async function openPage(seed: FakeSeed, hostname: string, session: Parameters<typeof withSession>[1] = {}, connection: FakeConnection = fakeConnection(seed)): Promise<HTMLElement> {
  h.connection = connection;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { userId: "u-me", ...session }));
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(hostname)).closest("button"));
  return screen.findByRole("region", { name: `Deployable ${hostname}` });
}

describe("the lock sits after Traffic and is absent without the part, on a system-owned site, and with no package deployable", () => {
  it("sits immediately after Traffic and opens the codes in a dialog", async () => {
    const page = await openPage({ sites: [DESK], accessGates: [DASHBOARD], accessCodes: [reviewers()] }, "desk.memql.example.com");
    const traffic = within(page).getByRole("button", { name: "Traffic" });
    const lock = within(page).getByRole("button", { name: "Access codes" });
    expect(traffic.nextElementSibling).toBe(lock);
    await click(lock);
    const dialog = await screen.findByRole("dialog", { name: "Access codes" });
    expect(await within(dialog).findByText("Client reviewers")).toBeTruthy();
  });

  it("is absent without the part", async () => {
    const page = await openPage({ sites: [DESK] }, "desk.memql.example.com", { role: "developer", capabilities: seededAccessWithout("developer", "app:deployables/access") });
    expect(within(page).getByRole("button", { name: "Traffic" })).toBeTruthy();
    expect(within(page).queryByRole("button", { name: "Access codes" })).toBeNull();
  });

  it("is absent on a system-owned site, even one with a package deployable", async () => {
    const system = { ...PLATFORM_SITE, packageId: "pkg-os", packageDeployableName: "os" } as Row;
    const page = await openPage({ sites: [system] }, String(PLATFORM_SITE["hostname"]));
    expect(within(page).getByRole("button", { name: "Traffic" })).toBeTruthy();
    expect(within(page).queryByRole("button", { name: "Access codes" })).toBeNull();
  });

  it("is absent with no package deployable", async () => {
    const plain = siteRow({ id: "site-plain", hostname: "plain.memql.example.com" });
    const page = await openPage({ sites: [plain] }, "plain.memql.example.com");
    expect(within(page).getByRole("button", { name: "Traffic" })).toBeTruthy();
    expect(within(page).queryByRole("button", { name: "Access codes" })).toBeNull();
  });
});

describe("the dialog holds while a write is out and while a new code stands", () => {
  const closeControl = () => screen.getByRole("button", { name: "Close Access codes" }) as HTMLButtonElement;
  const pressEscape = () => fireEvent.keyDown(screen.getByRole("dialog", { name: "Access codes" }), { key: "Escape" });
  const codesSeed = (): FakeSeed => ({ sites: [DESK], accessGates: [DASHBOARD], accessCodes: [reviewers()] });

  async function openCodes(seed: FakeSeed, connection: FakeConnection): Promise<HTMLElement> {
    const page = await openPage(seed, "desk.memql.example.com", {}, connection);
    await click(within(page).getByRole("button", { name: "Access codes" }));
    await screen.findByText("Client reviewers");
    return page;
  }

  it("will not close on Close or Escape while a regenerate is out, because its reply is the only copy of the code", async () => {
    const seed = codesSeed();
    const connection = fakeConnection(seed);
    const release = holdAnswers(connection, "siteAccessCodeRegenerate");
    await openCodes(seed, connection);
    await answerYes("Client reviewers", "Regenerate");

    expect(closeControl().disabled).toBe(true);
    await click(closeControl());
    pressEscape();
    expect(screen.getByRole("dialog", { name: "Access codes" })).toBeTruthy();

    release();
    expect(await screen.findByText(FIRST_CODE)).toBeTruthy();
  });

  it("lets go once a refused write has answered, with nothing left to lose", async () => {
    const seed = { ...codesSeed(), accessRemoveError: refusedByEngine("siteAccessCodeRemove", "Not this time.") };
    await openCodes(seed, fakeConnection(seed));
    await answerYes("Client reviewers", "Remove");
    await screen.findByText("Not this time.");
    expect(closeControl().disabled).toBe(false);
  });

  it("will not close over a new code, and Done is the way out", async () => {
    const seed = codesSeed();
    const page = await openCodes(seed, fakeConnection(seed));
    await answerYes("Client reviewers", "Regenerate");
    await screen.findByText(FIRST_CODE);

    expect(closeControl().disabled).toBe(true);
    pressEscape();
    expect(screen.getByText(FIRST_CODE)).toBeTruthy();

    await readTheBar();
    await click(screen.getByRole("button", { name: "Done" }));
    expect(closeControl().disabled).toBe(false);
    await click(closeControl());
    expect(screen.queryByRole("dialog", { name: "Access codes" })).toBeNull();

    await click(within(page).getByRole("button", { name: "Access codes" }));
    await screen.findByText("Client reviewers");
    expect(document.body.innerHTML).not.toContain(FIRST_CODE);
  });

  it("closes as ever with nothing out and no code standing", async () => {
    const seed = codesSeed();
    await openCodes(seed, fakeConnection(seed));
    expect(closeControl().disabled).toBe(false);
    pressEscape();
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Access codes" })).toBeNull());
  });
});

// ---------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------

describe("the list shows label, what it unlocks, hint, expiry word and last opened, and never a code", () => {
  it("reads every fact off the row, with the exact time behind the word", async () => {
    mountPanel({
      accessGates: [DASHBOARD],
      accessCodes: [reviewers({ codeHint: "9FBZ", expiresAt: inMinutes(8), issuedAt: inDays(-1), lastOpenedAt: inMinutes(-2) })],
    });
    const row = await rowOf("Client reviewers");
    const text = row.textContent ?? "";
    expect(text).toContain("Unlocks dashboard");
    expect(text).toContain("Ends in 9FBZ");
    expect(text).toContain("Expires in 8 minutes");
    expect(text).toContain("Last opened 2 minutes ago");
    expect(within(row).getByTitle(formatMoment(inMinutes(8))).textContent).toContain("Expires in 8 minutes");
  });

  it("says whose part of the gate a subject opens", async () => {
    mountPanel({ accessGates: [ENGAGEMENT], accessCodes: [reviewers({ gate: "engagement", subject: "acme" })] });
    expect((await rowOf("Client reviewers")).textContent).toContain("Unlocks engagement for acme");
  });

  it("draws a code past its expiry as Expired, in words", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: inMinutes(-5) })] });
    const row = await rowOf("Client reviewers");
    expect(within(row).getByText("Expired")).toBeTruthy();
    expect(row.textContent).not.toContain("Expires in");
  });

  it("turns to Expired while the panel stays open, with nothing else happening", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval"] });
    vi.setSystemTime(NOW);
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: inMinutes(1) })] });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    const row = await rowOf("Client reviewers");
    expect(within(row).getByText("Expires in 1 minute")).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(61_000);
    });
    expect(within(row).getByText("Expired")).toBeTruthy();
  });

  it("never draws a code a row happens to carry", async () => {
    const planted = FIRST_CODE;
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ code: planted, codeHash: "digest-of-it" })] });
    await rowOf("Client reviewers");
    expect(document.body.innerHTML).not.toContain(planted);
    expect(document.body.innerHTML).not.toContain("digest-of-it");
  });

  it("keeps the rows on screen while it reads them again", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] };
    const connection = fakeConnection(seed);
    mountPanel(seed, { connection });
    await rowOf("Pilot client");
    const release = holdAnswers(connection, "siteAccessCodesForSite");
    await answerYes("Pilot client", "Remove");
    expect(document.querySelector(".os-record-skeleton")).toBeNull();
    expect(screen.getByText("Client reviewers")).toBeTruthy();
    release();
    await waitFor(() => expect(screen.queryByText("Pilot client")).toBeNull());
  });
});

describe("a failed read does not look empty; loading is a skeleton; no gates explains @accessGate and has no Add control", () => {
  const REFUSAL = "No deployable you can read has that id. Reload the list of deployables and choose one you can see.";

  it("says a refused read in its own sentence, without the frames, and reads again on Try again", async () => {
    const seed: FakeSeed = {
      accessGates: [DASHBOARD],
      accessCodes: [reviewers()],
      accessCodesError: refusedByEngine("siteAccessCodesForSite", REFUSAL),
    };
    mountPanel(seed);
    const said = await screen.findByText(REFUSAL);
    expect(said.closest(".os-notice")?.getAttribute("data-tone")).toBe("error");
    expect(document.body.textContent).not.toContain("MemQL engine failed");
    expect(document.body.textContent).not.toContain("siteAccessCodesForSite");
    expect(screen.queryByText("No access codes yet")).toBeNull();
    expect(document.querySelector(".os-record-skeleton")).toBeNull();

    seed.accessCodesError = undefined;
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Client reviewers")).toBeTruthy();
    expect(screen.queryByText(REFUSAL)).toBeNull();
  });

  it("still says something when the refusal has no words in it", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodesError: "siteAccessCodesForSite: " });
    expect(await screen.findByText("The access codes could not be read.")).toBeTruthy();
    expect(screen.queryByText("No access codes yet")).toBeNull();
  });

  it("says so when the read after a write fails, keeps the new code, and reads again on Try again", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers()] };
    mountPanel(seed);
    await rowOf("Client reviewers");

    seed.accessCodesError = refusedByEngine("siteAccessCodesForSite", "The cluster did not answer.");
    await answerYes("Client reviewers", "Regenerate");

    expect(await screen.findByText("The cluster did not answer.")).toBeTruthy();
    expect(screen.getByText(FIRST_CODE)).toBeTruthy();
    expect(screen.queryByText("Client reviewers")).toBeNull();
    expect(screen.queryByText("No access codes yet")).toBeNull();
    expect(document.querySelector(".os-record-skeleton")).toBeNull();

    seed.accessCodesError = undefined;
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Client reviewers")).toBeTruthy();
    expect(screen.getByText(FIRST_CODE)).toBeTruthy();
    expect(screen.queryByText("The cluster did not answer.")).toBeNull();
  });

  it("reads a coded refusal in this app's words, without the code", async () => {
    mountPanel({
      accessGates: [DASHBOARD],
      accessCodesError: refusedByEngine("siteAccessCodesForSite", 'capability_not_held: "siteAccessCodesForSite" is not permitted for the selected organization\'s target'),
    });
    expect(await screen.findByText(/you have not been granted/)).toBeTruthy();
    expect(document.body.textContent).not.toContain("capability_not_held");
    expect(screen.queryByText("No access codes yet")).toBeNull();
  });

  it("does not print the socket's words when the connection drops under a read", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers()] };
    const connection = fakeConnection(seed);
    const real = connection.query.executeNamed.bind(connection.query);
    connection.query.executeNamed = (async () => {
      throw droppedSocket();
    }) as unknown as typeof connection.query.executeNamed;
    mountPanel(seed, { connection });
    const said = await screen.findByText(/connection to the cluster dropped/);
    expect(said.closest(".os-notice")?.getAttribute("data-tone")).toBe("error");
    expect(document.body.textContent).not.toContain("stream closed");

    connection.query.executeNamed = real as typeof connection.query.executeNamed;
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Client reviewers")).toBeTruthy();
  });

  it("says one refusal once when both reads were refused for it", async () => {
    mountPanel({
      accessCodesError: refusedByEngine("siteAccessCodesForSite", REFUSAL),
      accessGatesError: refusedByEngine("siteAccessGates", REFUSAL),
    });
    await screen.findByText(REFUSAL);
    expect(screen.getAllByText(REFUSAL)).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Try again" })).toHaveLength(1);
  });

  it("shows a skeleton while the reads are out, and no empty state", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [] };
    const connection = fakeConnection(seed);
    const releaseCodes = holdAnswers(connection, "siteAccessCodesForSite");
    mountPanel(seed, { connection });
    await waitFor(() => expect(connection.callsNamed("siteAccessCodesForSite")).toHaveLength(1));
    const status = document.querySelector(".os-record-skeleton") as HTMLElement;
    expect(status.getAttribute("aria-busy")).toBe("true");
    expect(status.textContent).toContain("Loading access codes");
    expect(screen.queryByText("No access codes yet")).toBeNull();
    releaseCodes();
    expect(await screen.findByText("No access codes yet")).toBeTruthy();
    expect(document.querySelector(".os-record-skeleton")).toBeNull();
  });

  it("does not say the codes are empty while the gates are still out", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [] };
    const connection = fakeConnection(seed);
    const releaseGates = holdAnswers(connection, "siteAccessGates");
    mountPanel(seed, { connection });
    await waitFor(() => expect(connection.callsNamed("siteAccessGates")).toHaveLength(1));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByText("No access codes yet")).toBeNull();
    expect(screen.queryByText(/@accessGate/)).toBeNull();
    releaseGates();
    expect(await screen.findByText("No access codes yet")).toBeTruthy();
  });

  it("says it is not connected rather than drawing a skeleton nothing is filling", async () => {
    h.connection = null;
    render(withSession(<AccessCodesPanel site={siteFromRow(DESK)} />));
    expect(await screen.findByText(/Not connected to the cluster/)).toBeTruthy();
    expect(document.querySelector(".os-record-skeleton")).toBeNull();
  });

  it("explains @accessGate and leaves Add absent when no gates are declared", async () => {
    mountPanel({ accessGates: [], accessCodes: [] });
    expect(await screen.findByText("@accessGate")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add an access code" })).toBeNull();
    expect(screen.queryByText("No access codes yet")).toBeNull();
  });

  it("says the same over codes that are left when every gate has gone", async () => {
    mountPanel({ accessGates: [], accessCodes: [reviewers()] });
    expect((await rowOf("Client reviewers")).textContent).toContain("no longer declares");
    expect(await screen.findByText("@accessGate")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add an access code" })).toBeNull();
  });

  it("offers Add once a gate is known, over an empty list", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    expect(await addControl()).toBeTruthy();
    expect(await screen.findByText("No access codes yet")).toBeTruthy();
    expect(screen.queryByText(/@accessGate/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Adding
// ---------------------------------------------------------------------------

describe("Add preselects a single gate, asks for a subject only when the gate uses one, and sends the expiry the preset names", () => {
  it("preselects one gate, asks no subject for it, and sends what was filled in", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("  Client reviewers ");
    expect(selectedLabel(screen.getByLabelText("Unlocks"))).toBe("dashboard");
    expect(screen.queryByLabelText(SUBJECT)).toBeNull();
    chooseOption(screen.getByLabelText("Expires"), "10 minutes");
    await click(screen.getByRole("button", { name: "Create" }));
    expect(connection.callsNamed("siteAccessCodeCreate")).toEqual([
      'builtin siteAccessCodeCreate(siteId: "site-desk", label: "Client reviewers", gate: "dashboard", expiresAt: "2026-10-30T12:10:00.000Z")',
    ]);
  });

  it("offers the five presets in order and then Custom", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await click(await addControl());
    const list = openSelect(screen.getByLabelText("Expires"));
    expect(within(list).getAllByRole("option").map((o) => o.textContent)).toEqual(["10 minutes", "1 hour", "1 day", "7 days", "30 days", "Custom"]);
  });

  it.each([
    ["10 minutes", "2026-10-30T12:10:00.000Z"],
    ["1 hour", "2026-10-30T13:00:00.000Z"],
    ["1 day", "2026-10-31T12:00:00.000Z"],
    ["7 days", "2026-11-06T12:00:00.000Z"],
    ["30 days", "2026-11-29T12:00:00.000Z"],
  ])("sends %s as %s", async (preset, wire) => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Timed");
    chooseOption(screen.getByLabelText("Expires"), preset);
    await click(screen.getByRole("button", { name: "Create" }));
    expect(connection.callsNamed("siteAccessCodeCreate")[0]).toContain(`expiresAt: "${wire}"`);
  });

  it("measures a preset from the press and not from when the form opened", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Slow typist");
    chooseOption(screen.getByLabelText("Expires"), "10 minutes");
    vi.setSystemTime(NOW + 5 * 60_000);
    await click(screen.getByRole("button", { name: "Create" }));
    expect(connection.callsNamed("siteAccessCodeCreate")[0]).toContain('expiresAt: "2026-10-30T12:15:00.000Z"');
  });

  it("makes a person choose among several gates, and asks a subject only for the gate that reads one", async () => {
    const { connection } = mountPanel({ accessGates: [ENGAGEMENT, DASHBOARD], accessCodes: [] });
    await fillAdd("Acme");
    const create = screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    expect(screen.queryByLabelText(SUBJECT)).toBeNull();

    const list = openSelect(screen.getByLabelText("Unlocks"));
    expect(within(list).getAllByRole("option").map((o) => o.textContent)).toEqual(["Choose a gate", "dashboard", "engagement"]);
    fireEvent.click(within(list).getByRole("option", { name: "engagement" }));
    await typeInto(screen.getByLabelText(SUBJECT) as HTMLInputElement, "acme");
    expect(create.disabled).toBe(false);
    await click(create);
    expect(connection.callsNamed("siteAccessCodeCreate")[0]).toContain('gate: "engagement", subject: "acme", expiresAt:');
  });

  it("does not ask for a subject the engine does not require: left empty, none is sent", async () => {
    const { connection } = mountPanel({ accessGates: [ENGAGEMENT], accessCodes: [] });
    await fillAdd("Whoever holds it");
    expect((screen.getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(false);
    await click(screen.getByRole("button", { name: "Create" }));
    expect(connection.callsNamed("siteAccessCodeCreate")[0]).not.toContain("subject");
  });

  it("sends no subject typed for a gate that was then changed", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD, ENGAGEMENT], accessCodes: [] });
    await fillAdd("Changed my mind");
    chooseOption(screen.getByLabelText("Unlocks"), "engagement");
    await typeInto(screen.getByLabelText(SUBJECT) as HTMLInputElement, "acme");
    chooseOption(screen.getByLabelText("Unlocks"), "dashboard");
    expect(screen.queryByLabelText(SUBJECT)).toBeNull();
    await click(screen.getByRole("button", { name: "Create" }));
    expect(connection.callsNamed("siteAccessCodeCreate")[0]).not.toContain("subject");
  });

  it.each([
    ["true", true],
    ["false", false],
  ])("reads usesSubject %s as text, which is how a scalar flag has reached this client on a builtin's row", async (flag, asks) => {
    mountPanel({ accessGates: [accessGateRow({ gate: "engagement", usesSubject: flag })], accessCodes: [] });
    await click(await addControl());
    expect(screen.queryByLabelText(SUBJECT) !== null).toBe(asks);
  });

  it("holds Create back until the name has something in it", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await click(await addControl());
    const create = screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    await typeInto(screen.getByLabelText("Name") as HTMLInputElement, "   ");
    expect(create.disabled).toBe(true);
    await typeInto(screen.getByLabelText("Name") as HTMLInputElement, "Named");
    expect(create.disabled).toBe(false);
  });

  it("creates on Enter from the name, with the same guard as the button", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await click(await addControl());
    const name = screen.getByLabelText("Name") as HTMLInputElement;
    fireEvent.keyDown(name, { key: "Enter" });
    expect(connection.callsNamed("siteAccessCodeCreate")).toHaveLength(0);
    await typeInto(name, "Named");
    await act(async () => {
      fireEvent.keyDown(name, { key: "Enter" });
    });
    expect(connection.callsNamed("siteAccessCodeCreate")).toHaveLength(1);
  });

  it("puts focus on the name when the form opens and back on Add when it is cancelled", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await click(await addControl());
    expect(document.activeElement).toBe(screen.getByLabelText("Name"));
    await click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByLabelText("Name")).toBeNull();
    expect(document.activeElement).toBe(await addControl());
    expect(connection.callsNamed("siteAccessCodeCreate")).toHaveLength(0);
  });

  it("lists the new code once it lands, without the form", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Client reviewers");
    await click(screen.getByRole("button", { name: "Create" }));
    const row = await rowOf("Client reviewers");
    expect(row.textContent).toContain(`Ends in ${FIRST_CODE.slice(-4)}`);
    expect(screen.queryByLabelText("Name")).toBeNull();
  });
});

describe("custom expiry names the time zone and a server refusal renders in place with its sentence, draft kept", () => {
  const ZONE = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const fieldForTheDate = () => screen.getByLabelText(`Expires at · ${ZONE}`) as HTMLInputElement;

  it("names this browser's zone and sends the instant that wall clock means", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Far away");
    chooseOption(screen.getByLabelText("Expires"), "Custom");
    await typeInto(fieldForTheDate(), "2026-11-02T09:30");
    await click(screen.getByRole("button", { name: "Create" }));
    const instant = new Date(2026, 10, 2, 9, 30).toISOString().replace(".000Z", "Z");
    expect(connection.callsNamed("siteAccessCodeCreate")[0]).toContain(`expiresAt: "${instant}"`);
  });

  it("holds Create back until a custom date is given", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Far away");
    const create = screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;
    expect(create.disabled).toBe(false);
    chooseOption(screen.getByLabelText("Expires"), "Custom");
    expect(create.disabled).toBe(true);
    await typeInto(fieldForTheDate(), "2026-11-02T09:30");
    expect(create.disabled).toBe(false);
  });

  it("renders a refusal in the form in its own words, keeps the draft, and lets the same press try again", async () => {
    const seed: FakeSeed = {
      accessGates: [DASHBOARD, ENGAGEMENT],
      accessCodes: [],
      accessCreateError: refusedByEngine("siteAccessCodeCreate", "That expiry is more than 366 days away; choose an earlier time, and extend the expiry later if the code is still needed."),
    };
    const { connection } = mountPanel(seed);
    await fillAdd("Far away");
    chooseOption(screen.getByLabelText("Unlocks"), "engagement");
    await typeInto(screen.getByLabelText(SUBJECT) as HTMLInputElement, "acme");
    chooseOption(screen.getByLabelText("Expires"), "Custom");
    await typeInto(fieldForTheDate(), "2029-01-01T09:30");
    await click(screen.getByRole("button", { name: "Create" }));

    const said = await screen.findByText(/more than 366 days away/);
    expect(said.textContent).toBe("That expiry is more than 366 days away; choose an earlier time, and extend the expiry later if the code is still needed.");
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Far away");
    expect(selectedLabel(screen.getByLabelText("Unlocks"))).toBe("engagement");
    expect((screen.getByLabelText(SUBJECT) as HTMLInputElement).value).toBe("acme");
    expect(fieldForTheDate().value).toBe("2029-01-01T09:30");

    seed.accessCreateError = undefined;
    await typeInto(fieldForTheDate(), "2026-12-01T09:30");
    await click(screen.getByRole("button", { name: "Create" }));
    await screen.findByText(FIRST_CODE);
    expect(screen.queryByText(/more than 366 days away/)).toBeNull();
    expect(connection.callsNamed("siteAccessCodeCreate")).toHaveLength(2);
  });

  it("keeps the draft when the read after a refusal fails too", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD, ENGAGEMENT], accessCodes: [] };
    const { connection } = mountPanel(seed);
    await fillAdd("Acme");
    chooseOption(screen.getByLabelText("Unlocks"), "engagement");
    await typeInto(screen.getByLabelText(SUBJECT) as HTMLInputElement, "acme");
    seed.accessCreateError = refusedByEngine("siteAccessCodeCreate", "That did not complete. Reload the list to see where it stands, and try again.");
    seed.accessGatesError = refusedByEngine("siteAccessGates", "The cluster did not answer.");
    await click(screen.getByRole("button", { name: "Create" }));
    await screen.findByText("The cluster did not answer.");

    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Acme");
    expect(selectedLabel(screen.getByLabelText("Unlocks"))).toBe("engagement");
    expect((screen.getByLabelText(SUBJECT) as HTMLInputElement).value).toBe("acme");
    expect(screen.getByText("That did not complete. Reload the list to see where it stands, and try again.")).toBeTruthy();

    seed.accessCreateError = undefined;
    seed.accessGatesError = undefined;
    await click(screen.getByRole("button", { name: "Create" }));
    await screen.findByText(FIRST_CODE);
    expect(connection.callsNamed("siteAccessCodeCreate")[1]).toContain('gate: "engagement", subject: "acme"');
  });
});

// ---------------------------------------------------------------------------
// The code, once
// ---------------------------------------------------------------------------

describe("the new code appears once with a warning and copy, and Done discards it", () => {
  const writeText = vi.fn(async (_value: string) => {});

  beforeEach(() => {
    writeText.mockClear();
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  });
  afterEach(() => {
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
  });

  it("shows the code under a warning, copies the whole of it, and takes focus there", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Client reviewers");
    await click(screen.getByRole("button", { name: "Create" }));
    const value = await screen.findByText(FIRST_CODE);
    const notice = value.closest(".os-notice") as HTMLElement;
    expect(notice.getAttribute("data-tone")).toBe("warn");
    expect(notice.textContent).toContain("Client reviewers");
    expect(notice.textContent).toContain("only time");
    const copy = within(notice).getByRole("button", { name: "Copy access code" });
    expect(document.activeElement).toBe(copy);
    await click(copy);
    expect(writeText).toHaveBeenCalledWith(FIRST_CODE);
  });

  it("goes when Done is pressed, and the re-read does not bring it back", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Client reviewers");
    await click(screen.getByRole("button", { name: "Create" }));
    await screen.findByText(FIRST_CODE);
    await readTheBar();
    const reads = connection.callsNamed("siteAccessCodesForSite").length;
    await click(screen.getByRole("button", { name: "Done" }));
    expect(document.body.innerHTML).not.toContain(FIRST_CODE);
    expect(connection.callsNamed("siteAccessCodesForSite")).toHaveLength(reads);
    expect(document.activeElement).toBe(await addControl());
  });

  it("appears for a regenerate too, and Done returns to the control that asked", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers()] });
    await answerYes("Client reviewers", "Regenerate");
    expect((await screen.findByText(FIRST_CODE)).closest(".os-notice")?.textContent).toContain("Client reviewers");
    await readTheBar();
    await click(screen.getByRole("button", { name: "Done" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Regenerate Client reviewers" }));
  });

  it("says so rather than saying nothing when an accepted reply carries no code", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers()] };
    const connection = fakeConnection(seed);
    const real = connection.query.executeNamed.bind(connection.query);
    connection.query.executeNamed = (async (name: string, call: string, opts?: object) => {
      const answer = await real(name, call, opts);
      return name === "siteAccessCodeRegenerate" ? { rows: () => [{ codeId: "code-desk" }] } : answer;
    }) as unknown as typeof connection.query.executeNamed;
    mountPanel(seed, { connection });
    await answerYes("Client reviewers", "Regenerate");
    expect(await screen.findByText(/did not return the code/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Done" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Keep it" })).toBeNull();
  });
});

describe("a write whose answer never came", () => {
  it("says a create may have been saved, puts the list back in view, and offers nothing that would mint again", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [] };
    const connection = fakeConnection(seed);
    loseTheReply(connection, "siteAccessCodeCreate");
    mountPanel(seed, { connection });
    await fillAdd("Client reviewers");
    await click(screen.getByRole("button", { name: "Create" }));

    const said = await screen.findByText(/may have been saved/);
    expect(said.closest(".os-notice")?.getAttribute("data-tone")).toBe("warn");
    expect(document.body.textContent).not.toContain("stream closed");
    expect(screen.queryByLabelText("Name")).toBeNull();
    expect(screen.queryByRole("button", { name: "Create" })).toBeNull();
    // It did land, and the list is where a person finds that out.
    expect(await rowOf("Client reviewers")).toBeTruthy();
    expect(document.activeElement).toBe(await addControl());
    expect(connection.callsNamed("siteAccessCodeCreate")).toHaveLength(1);
  });

  it("does the same for a create accepted with no code in its reply", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [] };
    const connection = fakeConnection(seed);
    const real = connection.query.executeNamed.bind(connection.query);
    connection.query.executeNamed = (async (name: string, call: string, opts?: object) => {
      const answer = await real(name, call, opts);
      return name === "siteAccessCodeCreate" ? { rows: () => [{ codeId: "code-minted-1" }] } : answer;
    }) as unknown as typeof connection.query.executeNamed;
    mountPanel(seed, { connection });
    await fillAdd("Client reviewers");
    await click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByText(/did not return the code/)).toBeTruthy();
    expect(screen.queryByLabelText("Name")).toBeNull();
    expect(await rowOf("Client reviewers")).toBeTruthy();
    expect(document.activeElement).toBe(await addControl());
  });

  it.each([
    ["Regenerate", "siteAccessCodeRegenerate", /regenerate it again/i],
    ["Remove", "siteAccessCodeRemove", /Check the list before trying again/],
  ] as const)("says a %s may have been saved, closes the question, and returns focus", async (act, construct, next) => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] };
    const connection = fakeConnection(seed);
    loseTheReply(connection, construct);
    mountPanel(seed, { connection });
    await answerYes("Pilot client", act);

    const said = await screen.findByText(/may have been saved/);
    expect(said.textContent).toMatch(next);
    expect(document.body.textContent).not.toContain("stream closed");
    expect(screen.queryByRole("button", { name: "Keep it" })).toBeNull();
    expect(document.activeElement).not.toBe(document.body);
  });

  it("says it in words when the socket stays down, and the list comes back on Try again", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers()] };
    const connection = fakeConnection(seed);
    const real = connection.query.executeNamed.bind(connection.query);
    mountPanel(seed, { connection });
    await rowOf("Client reviewers");
    // The write reaches the cluster, and from then on nothing else does.
    connection.query.executeNamed = (async (name: string, call: string, opts?: object) => {
      if (name === "siteAccessCodeRemove") await real(name, call, opts);
      throw droppedSocket();
    }) as typeof connection.query.executeNamed;
    await answerYes("Client reviewers", "Remove");

    expect(await screen.findByText(/may have been saved/)).toBeTruthy();
    expect(await screen.findByText(/connection to the cluster dropped/)).toBeTruthy();
    expect(document.body.textContent).not.toContain("stream closed");
    expect(screen.queryByText("No access codes yet")).toBeNull();

    connection.query.executeNamed = real as typeof connection.query.executeNamed;
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("No access codes yet")).toBeTruthy();
    expect(screen.queryByText(/connection to the cluster dropped/)).toBeNull();
  });

  it("keeps a refusal the engine did answer with apart from one it never answered", async () => {
    const seed: FakeSeed = {
      accessGates: [DASHBOARD],
      accessCodes: [],
      accessCreateError: refusedByEngine("siteAccessCodeCreate", "Shorten the name to 80 characters or fewer."),
    };
    mountPanel(seed);
    await fillAdd("Client reviewers");
    await click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("Shorten the name to 80 characters or fewer.")).toBeTruthy();
    expect(screen.getByLabelText("Name")).toBeTruthy();
    expect(screen.queryByText(/may have been saved/)).toBeNull();
  });
});

describe("the minted code is gone after any other act and is not in any row or storage", () => {
  async function minted(seed: FakeSeed): Promise<FakeConnection> {
    const { connection } = mountPanel(seed);
    await answerYes("Client reviewers", "Regenerate");
    await screen.findByText(FIRST_CODE);
    await readTheBar();
    return connection;
  }

  const twoCodes = (): FakeSeed => ({ accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] });

  function storedAnywhere(): string {
    const dump = (s: Storage): string => Array.from({ length: s.length }, (_, i) => {
      const key = s.key(i) as string;
      return `${key}=${s.getItem(key)}`;
    }).join("\n");
    return `${dump(localStorage)}\n${dump(sessionStorage)}`;
  }

  it("goes when another code is regenerated, and only the new one stands", async () => {
    await minted(twoCodes());
    await answerYes("Pilot client", "Regenerate");
    expect(await screen.findByText(SECOND_CODE)).toBeTruthy();
    expect(document.body.innerHTML).not.toContain(FIRST_CODE);
  });

  it("goes when another code is removed", async () => {
    await minted(twoCodes());
    await answerYes("Pilot client", "Remove");
    await waitFor(() => expect(screen.queryByText("Pilot client")).toBeNull());
    expect(document.body.innerHTML).not.toContain(FIRST_CODE);
  });

  it("goes when an expiry is changed", async () => {
    await minted(twoCodes());
    const row = await ask("Pilot client", "Change expiry of");
    await click(within(row).getByRole("button", { name: "Change expiry" }));
    await waitFor(() => expect(document.body.innerHTML).not.toContain(FIRST_CODE));
  });

  it("goes when another code is created", async () => {
    await minted(twoCodes());
    await fillAdd("Third");
    await click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText(SECOND_CODE)).toBeTruthy();
    expect(document.body.innerHTML).not.toContain(FIRST_CODE);
  });

  it("goes when the next act is refused, because the act was still the next act", async () => {
    await minted({ ...twoCodes(), accessRemoveError: refusedByEngine("siteAccessCodeRemove", "That did not complete.") });
    await answerYes("Pilot client", "Remove");
    await screen.findByText("That did not complete.");
    expect(document.body.innerHTML).not.toContain(FIRST_CODE);
  });

  it("stays through a question answered Keep it, which wrote nothing", async () => {
    const connection = await minted(twoCodes());
    const writes = connection.calls.length;
    const row = await ask("Pilot client", "Remove");
    await click(within(row).getByRole("button", { name: "Keep it" }));
    expect(screen.getByText(FIRST_CODE)).toBeTruthy();
    expect(connection.calls.filter((call) => call.includes("siteAccessCodeRemove"))).toHaveLength(0);
    expect(connection.calls.length).toBe(writes);
  });

  it("is nowhere in the list, a row or storage while it stands", async () => {
    await minted(twoCodes());
    const list = screen.getByRole("list", { name: "Access codes" });
    expect(list.innerHTML).not.toContain(FIRST_CODE);
    expect(storedAnywhere()).not.toContain(FIRST_CODE);
  });

  it("is dropped by a row's projection even when a reply carries it", () => {
    const [row] = accessCodesFromRows([reviewers({ code: FIRST_CODE })]);
    expect(JSON.stringify(row)).not.toContain(FIRST_CODE);
  });
});

describe("a double click on Create sends one call", () => {
  it("sends one, whether or not the page has drawn between the clicks", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Client reviewers");
    const create = screen.getByRole("button", { name: "Create" });
    await act(async () => {
      fireEvent.click(create);
      fireEvent.click(create);
    });
    await screen.findByText(FIRST_CODE);
    expect(connection.callsNamed("siteAccessCodeCreate")).toHaveLength(1);
    expect(screen.queryByText(SECOND_CODE)).toBeNull();
  });

  it("shows Create as busy while the call is out", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [] };
    const connection = fakeConnection(seed);
    const release = holdAnswers(connection, "siteAccessCodeCreate");
    mountPanel(seed, { connection });
    await fillAdd("Client reviewers");
    const create = screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;
    await click(create);
    expect(create.disabled).toBe(true);
    expect(create.textContent).toBe("Creating");
    release();
    await screen.findByText(FIRST_CODE);
  });

  it("sends one when Enter lands after the reply and before the form has gone", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [] };
    const connection = fakeConnection(seed);
    const release = holdAnswers(connection, "siteAccessCodeCreate");
    const held = connection.query.executeNamed.bind(connection.query);
    let replied = false;
    connection.query.executeNamed = (async (name: string, call: string, opts?: object) => {
      const answer = await held(name, call, opts);
      if (name === "siteAccessCodeCreate") replied = true;
      return answer;
    }) as typeof connection.query.executeNamed;
    mountPanel(seed, { connection });
    await fillAdd("Client reviewers");
    const name = screen.getByLabelText("Name") as HTMLInputElement;
    const enter = () => name.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));

    // REACT'S OWN SCHEDULER, NOT act(). Under act() a state update is drawn before
    // the next line runs, so the frame this is about does not exist. On the real
    // scheduler the reply's every hop is a microtask and the render is a later task,
    // so a press can land between them, as a press in a browser can.
    const environment = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
    environment.IS_REACT_ACT_ENVIRONMENT = false;
    try {
      enter();
      release();
      for (let hop = 0; hop < 50; hop++) await Promise.resolve();
      // The frame in question: the reply is in, and the form has not gone.
      expect(replied).toBe(true);
      expect(name.isConnected).toBe(true);
      enter();
      await new Promise((resolve) => setTimeout(resolve, 20));
    } finally {
      environment.IS_REACT_ACT_ENVIRONMENT = true;
    }

    expect(connection.callsNamed("siteAccessCodeCreate")).toHaveLength(1);
    expect(await screen.findByText(FIRST_CODE)).toBeTruthy();
    expect(screen.queryByText(SECOND_CODE)).toBeNull();
  });

  it("does not let the second click of it discard the code it brought", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [] });
    await fillAdd("Client reviewers");
    await click(screen.getByRole("button", { name: "Create" }));
    await screen.findByText(FIRST_CODE);
    const done = screen.getByRole("button", { name: "Done" });
    await click(done);
    expect(screen.getByText(FIRST_CODE)).toBeTruthy();
    await readTheBar();
    await click(done);
    expect(screen.queryByText(FIRST_CODE)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Acting on a code
// ---------------------------------------------------------------------------

describe("Regenerate and Remove ask first, say what stops working, and Keep it writes nothing and returns focus", () => {
  it.each([
    ["Regenerate", "siteAccessCodeRegenerate", "The current code stops working and anyone using it is taken out now."],
    ["Remove", "siteAccessCodeRemove", "stops working and anyone using it is taken out now."],
  ] as const)("%s asks, says what stops working, and Keep it writes nothing", async (act, construct, consequence) => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: inDays(5) })] });
    const row = await rowOf("Client reviewers");
    const trigger = within(row).getByRole("button", { name: `${act} Client reviewers` });
    await click(trigger);
    expect(connection.callsNamed(construct)).toHaveLength(0);
    expect(row.textContent).toContain(consequence);
    // The question takes focus, on its way out rather than on the act.
    expect(document.activeElement).toBe(within(row).getByRole("button", { name: "Keep it" }));

    await readTheBar();
    await click(within(row).getByRole("button", { name: "Keep it" }));
    expect(connection.callsNamed(construct)).toHaveLength(0);
    expect(row.textContent).not.toContain(consequence);
    expect(document.activeElement).toBe(trigger);
  });

  it.each([
    ["Regenerate", "siteAccessCodeRegenerate"],
    ["Remove", "siteAccessCodeRemove"],
  ] as const)("%s cannot be answered by the second click of the press that asked it", async (act, construct) => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers()] });
    const row = await rowOf("Client reviewers");
    await click(within(row).getByRole("button", { name: `${act} Client reviewers` }));
    await click(within(row).getByRole("button", { name: act }));
    expect(connection.callsNamed(construct)).toHaveLength(0);
    await readTheBar();
    await click(within(row).getByRole("button", { name: act }));
    expect(connection.callsNamed(construct)).toHaveLength(1);
  });

  it("regenerates a live code without touching its expiry", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: inDays(5) })] });
    await answerYes("Client reviewers", "Regenerate");
    expect(connection.callsNamed("siteAccessCodeRegenerate")).toEqual(['builtin siteAccessCodeRegenerate(codeId: "code-desk")']);
    expect(await screen.findByText(FIRST_CODE)).toBeTruthy();
    expect((await rowOf("Client reviewers")).textContent).toContain(`Ends in ${FIRST_CODE.slice(-4)}`);
  });

  it("removes a code, and it leaves the list", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] });
    await answerYes("Client reviewers", "Remove");
    expect(connection.callsNamed("siteAccessCodeRemove")).toEqual(['builtin siteAccessCodeRemove(codeId: "code-desk")']);
    await waitFor(() => expect(screen.queryByText("Client reviewers")).toBeNull());
    expect(screen.getByText("Pilot client")).toBeTruthy();
  });

  it("puts focus on Add once a remove has landed, before the re-read takes the row away", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] };
    const connection = fakeConnection(seed);
    mountPanel(seed, { connection });
    await rowOf("Pilot client");
    const release = holdAnswers(connection, "siteAccessCodesForSite");
    await answerYes("Pilot client", "Remove");

    expect(screen.getByText("Pilot client")).toBeTruthy();
    expect(document.activeElement).toBe(await addControl());

    release();
    await waitFor(() => expect(screen.queryByText("Pilot client")).toBeNull());
    expect(document.activeElement).toBe(await addControl());
  });

  it("puts focus on the panel when there is no Add to go back to", async () => {
    const seed: FakeSeed = { accessGates: [], accessCodes: [reviewers(), pilot()] };
    mountPanel(seed);
    await answerYes("Pilot client", "Remove");
    await waitFor(() => expect(screen.queryByText("Pilot client")).toBeNull());
    expect(document.activeElement).toBe(document.querySelector(".access-codes"));
  });

  it("gives an expired code's Regenerate a new expiry to send, and says nobody is using it", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: inMinutes(-5) })] });
    const row = await ask("Client reviewers", "Regenerate");
    expect(row.textContent).toContain("already expired");
    expect(row.textContent).not.toContain("taken out");
    chooseOption(within(row).getByLabelText("Expires"), "7 days");
    await readTheBar();
    await click(within(row).getByRole("button", { name: "Regenerate" }));
    // Seven days from the press, and the question took two readings to get there.
    expect(connection.callsNamed("siteAccessCodeRegenerate")).toEqual([
      'builtin siteAccessCodeRegenerate(codeId: "code-desk", expiresAt: "2026-11-06T12:00:00.800Z")',
    ]);
  });

  it("treats an expiry it cannot read as past, as the engine does, so Regenerate asks for one", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: "" })] });
    const row = await ask("Client reviewers", "Regenerate");
    expect(within(row).getByText("Expiry unknown")).toBeTruthy();
    expect(within(row).getByLabelText("Expires")).toBeTruthy();
  });

  it("renders a refused Remove in the question, which stays to be answered again", async () => {
    const seed: FakeSeed = {
      accessGates: [DASHBOARD],
      accessCodes: [reviewers()],
      accessRemoveError: refusedByEngine("siteAccessCodeRemove", "Someone else changed this access code at the same moment, so this change did not land. Reload the list and try again."),
    };
    mountPanel(seed);
    const row = await ask("Client reviewers", "Remove");
    await click(within(row).getByRole("button", { name: "Remove" }));
    expect(await within(row).findByText(/Someone else changed this access code/)).toBeTruthy();
    expect(row.textContent).not.toContain("MemQL engine failed");

    seed.accessRemoveError = undefined;
    await readTheBar();
    await click(within(row).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(screen.queryByText("Client reviewers")).toBeNull());
  });

  it("keeps a regenerate's refusal on screen after the removed code has left the list", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] };
    mountPanel(seed);
    await rowOf("Client reviewers");
    // Somebody else removed it while this list was on screen, so the re-read
    // that follows the refusal takes the row, and the question, away.
    seed.accessCodes![0]!["revokedAt"] = inMinutes(-1);
    await answerYes("Client reviewers", "Regenerate");
    const sentence = "This access code was removed, and removal is final. To give someone access again, add a new code.";
    expect(await screen.findByText(sentence)).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("Client reviewers")).toBeNull());
    expect(screen.getByText(sentence)).toBeTruthy();
    // The question went with the row, with focus inside it.
    expect(document.activeElement).toBe(await addControl());
  });

  it("reads the list again after every write, accepted or refused", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers()] };
    const { connection } = mountPanel(seed);
    await rowOf("Client reviewers");
    const reads = () => connection.callsNamed("siteAccessCodesForSite").length;

    let before = reads();
    await answerYes("Client reviewers", "Regenerate");
    await waitFor(() => expect(reads()).toBe(before + 1));

    before = reads();
    seed.accessRemoveError = refusedByEngine("siteAccessCodeRemove", "Not this time.");
    await answerYes("Client reviewers", "Remove");
    await screen.findByText("Not this time.");
    await waitFor(() => expect(reads()).toBe(before + 1));
  });

  it("reads again after a write that lands while a read is out, whose answer predates it", async () => {
    const seed: FakeSeed = { accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] };
    const connection = fakeConnection(seed);
    mountPanel(seed, { connection });
    await rowOf("Pilot client");
    const release = holdAnswers(connection, "siteAccessCodesForSite");
    await answerYes("Client reviewers", "Regenerate");
    await waitFor(() => expect(connection.callsNamed("siteAccessCodeRegenerate")).toHaveLength(1));
    await answerYes("Pilot client", "Remove");
    await waitFor(() => expect(connection.callsNamed("siteAccessCodeRemove")).toHaveLength(1));
    release();
    await waitFor(() => expect(screen.queryByText("Pilot client")).toBeNull());
  });
});

describe("changing an expiry", () => {
  it("sends the new time, keeps the code, and returns focus", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: inDays(5) })] });
    const row = await ask("Client reviewers", "Change expiry of");
    chooseOption(within(row).getByLabelText("Expires"), "1 hour");
    await readTheBar();
    await click(within(row).getByRole("button", { name: "Change expiry" }));
    // An hour from the press, and the question took two readings to get there.
    expect(connection.callsNamed("siteAccessCodeSetExpiry")).toEqual([
      'builtin siteAccessCodeSetExpiry(codeId: "code-desk", expiresAt: "2026-10-30T13:00:00.800Z")',
    ]);
    await waitFor(() => expect(row.textContent).toContain("Expires in 1 hour"));
    expect(screen.queryByRole("button", { name: "Done" })).toBeNull();
    expect(connection.callsNamed("siteAccessCodeRegenerate")).toHaveLength(0);
    expect(document.activeElement).toBe(within(row).getByRole("button", { name: "Change expiry of Client reviewers" }));
  });

  it("revives an expired code, and says it is the same code", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ expiresAt: inMinutes(-5) })] });
    const row = await ask("Client reviewers", "Change expiry of");
    expect(row.textContent).toContain("same code");
    await click(within(row).getByRole("button", { name: "Change expiry" }));
    await waitFor(() => expect(connection.callsNamed("siteAccessCodeSetExpiry")).toHaveLength(1));
    await waitFor(() => expect(row.textContent).toContain("Expires in 1 day"));
    expect(within(row).queryByText("Expired")).toBeNull();
  });

  it("names the time zone behind Custom and holds the act until a date is given", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers()] });
    const row = await ask("Client reviewers", "Change expiry of");
    chooseOption(within(row).getByLabelText("Expires"), "Custom");
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    const date = within(row).getByLabelText(`Expires at · ${zone}`) as HTMLInputElement;
    const change = within(row).getByRole("button", { name: "Change expiry" }) as HTMLButtonElement;
    expect(change.disabled).toBe(true);
    await typeInto(date, "2026-11-02T09:30");
    await readTheBar();
    await click(change);
    const instant = new Date(2026, 10, 2, 9, 30).toISOString().replace(".000Z", "Z");
    expect(connection.callsNamed("siteAccessCodeSetExpiry")[0]).toContain(`expiresAt: "${instant}"`);
  });

  it("writes nothing and returns focus on Cancel", async () => {
    const { connection } = mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers()] });
    const row = await ask("Client reviewers", "Change expiry of");
    await click(within(row).getByRole("button", { name: "Cancel" }));
    expect(connection.callsNamed("siteAccessCodeSetExpiry")).toHaveLength(0);
    expect(within(row).queryByRole("button", { name: "Change expiry" })).toBeNull();
    expect(document.activeElement).toBe(within(row).getByRole("button", { name: "Change expiry of Client reviewers" }));
  });

  it("puts the first field in reach, and asking something else closes the question open", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] });
    const row = await ask("Client reviewers", "Change expiry of");
    expect(document.activeElement).toBe(within(row).getByLabelText("Expires"));
    const other = await ask("Pilot client", "Remove");
    expect(within(row).queryByRole("button", { name: "Change expiry" })).toBeNull();
    await click(within(other).getByRole("button", { name: "Keep it" }));
    expect(document.activeElement).toBe(within(other).getByRole("button", { name: "Remove Pilot client" }));
  });

  it("renders a refusal in place and keeps the choice", async () => {
    mountPanel({
      accessGates: [DASHBOARD],
      accessCodes: [reviewers()],
      accessSetExpiryError: refusedByEngine("siteAccessCodeSetExpiry", "That expiry is too soon; choose a time more than 1 minute from now."),
    });
    const row = await ask("Client reviewers", "Change expiry of");
    chooseOption(within(row).getByLabelText("Expires"), "10 minutes");
    await readTheBar();
    await click(within(row).getByRole("button", { name: "Change expiry" }));
    expect(await within(row).findByText("That expiry is too soon; choose a time more than 1 minute from now.")).toBeTruthy();
    expect(selectedLabel(within(row).getByLabelText("Expires"))).toBe("10 minutes");
  });
});

describe("a code whose gate is no longer declared still lists and says so", () => {
  it("lists it, says so, and says nothing of the kind about a gate still declared", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot({ gate: "reports" })] });
    const gone = await rowOf("Pilot client");
    expect(gone.textContent).toContain("no longer declares");
    expect(within(gone).getByRole("button", { name: "Remove Pilot client" })).toBeTruthy();
    expect((await rowOf("Client reviewers")).textContent).not.toContain("no longer declares");
  });

  it("claims nothing about a gate when the gates could not be read", async () => {
    mountPanel({
      accessGatesError: refusedByEngine("siteAccessGates", "The gates are not available."),
      accessCodes: [reviewers()],
    });
    const row = await rowOf("Client reviewers");
    await screen.findByText("The gates are not available.");
    expect(row.textContent).toContain("Unlocks dashboard");
    expect(row.textContent).not.toContain("no longer declares");
    expect(screen.queryByRole("button", { name: "Add an access code" })).toBeNull();
    expect(screen.queryByText(/@accessGate/)).toBeNull();
  });
});

describe("a code minted under another organization", () => {
  const ACME_SITE = siteRow({ ...DESK, id: "site-desk", accountId: "acme" } as never);
  const staleSeed = (): FakeSeed => ({ sites: [ACME_SITE], accessGates: [DASHBOARD], accessCodes: [reviewers({ accountId: "borden", expiresAt: inDays(5) })] });

  it("says it opens nothing and offers a cluster operator only Remove, the one act that helps, which lands", async () => {
    const { connection } = mountPanel(staleSeed(), { site: ACME_SITE });
    const row = await rowOf("Client reviewers");
    expect(row.textContent).toContain("opens nothing");
    expect(row.textContent).not.toContain("cluster operator");
    expect(within(row).queryByRole("button", { name: "Regenerate Client reviewers" })).toBeNull();
    expect(within(row).queryByRole("button", { name: "Change expiry of Client reviewers" })).toBeNull();
    expect(row.textContent).not.toContain("Expires in");

    await answerYes("Client reviewers", "Remove");
    await waitFor(() => expect(screen.queryByText("Client reviewers")).toBeNull());
    expect(connection.callsNamed("siteAccessCodeRemove")).toHaveLength(1);
  });

  it("says a cluster operator must remove it, and offers nothing, to somebody the gate would refuse", async () => {
    const { connection } = mountHolding(staleSeed(), ACME_SITE, "acme");
    const row = await rowOf("Client reviewers");
    expect(row.textContent).toContain("opens nothing");
    expect(row.textContent).toContain("A cluster operator must remove it");
    expect(within(row).queryAllByRole("button")).toEqual([]);
    expect(connection.callsNamed("siteAccessCodeRemove")).toHaveLength(0);
  });

  it("offers Remove to somebody who holds the part in the organization on the code, and it lands", async () => {
    mountHolding(staleSeed(), ACME_SITE, "acme", "borden");
    const row = await rowOf("Client reviewers");
    expect(row.textContent).not.toContain("cluster operator");
    await answerYes("Client reviewers", "Remove");
    await waitFor(() => expect(screen.queryByText("Client reviewers")).toBeNull());
    expect(document.body.textContent).not.toContain("capability_not_held");
  });

  it("does not print the engine's code when the part is taken away after the question opened", async () => {
    mountHolding(staleSeed(), ACME_SITE, "acme", "borden");
    const row = await ask("Client reviewers", "Remove");
    setEffectiveCapabilities(seededAccessFor("writer"), holdingIn("acme"));
    await click(within(row).getByRole("button", { name: "Remove" }));

    expect(await within(row).findByText(/you have not been granted/)).toBeTruthy();
    expect(row.textContent).toContain("Nothing was changed.");
    expect(document.body.textContent).not.toContain("capability_not_held");
    expect(document.body.textContent).not.toContain("siteAccessCodeRemove");
  });

  it("does not say removing it takes anyone out, because it already opens nothing", async () => {
    mountPanel(staleSeed(), { site: ACME_SITE });
    const row = await ask("Client reviewers", "Remove");
    const group = within(row).getByRole("group", { name: "Remove Client reviewers?" });
    expect(group.textContent).toContain("It already opens nothing.");
    expect(group.textContent).not.toContain("taken out");
    expect(group.textContent).not.toContain("stops working");
  });

  it("is the same organization however each side spells it", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ accountId: "acme" })] }, { site: siteRow({ ...DESK, id: "site-desk", accountId: "v1:accounts:account:acme" } as never) });
    const row = await rowOf("Client reviewers");
    expect(row.textContent).not.toContain("opens nothing");
    expect(within(row).getByRole("button", { name: "Regenerate Client reviewers" })).toBeTruthy();
  });

  it("is the same organization when neither side has one", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers()] });
    expect((await rowOf("Client reviewers")).textContent).not.toContain("opens nothing");
  });
});

describe("a deployable with no owner", () => {
  const OWNERLESS = siteRow({ ...DESK, id: "site-desk", ownerUserId: "" } as never);
  const ownerlessSeed = (codes: Row[]): FakeSeed => ({ sites: [OWNERLESS], accessGates: [DASHBOARD], accessCodes: codes });

  it("offers no Add and says why, whatever gates it declares", async () => {
    mountPanel(ownerlessSeed([]), { site: OWNERLESS });
    expect(await screen.findByText("This deployable has no owner")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add an access code" })).toBeNull();
    expect(screen.queryByText("No access codes yet")).toBeNull();
  });

  it("says once that its codes open nothing, offers only Remove, and keeps what each row knows", async () => {
    mountPanel(ownerlessSeed([reviewers({ expiresAt: inDays(5) }), pilot()]), { site: OWNERLESS });
    const row = await rowOf("Client reviewers");
    expect(screen.getAllByText(/has no owner, so its codes open nothing/)).toHaveLength(1);
    expect(row.textContent).toContain("Unlocks dashboard");
    expect(row.textContent).toContain("Expires in 5 days");
    expect(within(row).queryByRole("button", { name: "Regenerate Client reviewers" })).toBeNull();
    expect(within(row).queryByRole("button", { name: "Change expiry of Client reviewers" })).toBeNull();
    expect(within(row).getByRole("button", { name: "Remove Client reviewers" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/could not be read/)).toBeNull());
    expect(screen.queryByRole("button", { name: "Add an access code" })).toBeNull();
  });

  it("removes a code, and the question says it already opens nothing", async () => {
    const { connection } = mountPanel(ownerlessSeed([reviewers(), pilot()]), { site: OWNERLESS });
    const row = await ask("Pilot client", "Remove");
    const group = within(row).getByRole("group", { name: "Remove Pilot client?" });
    expect(group.textContent).toContain("It already opens nothing.");
    expect(group.textContent).not.toContain("taken out");
    await click(within(group).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(screen.queryByText("Pilot client")).toBeNull());
    expect(connection.callsNamed("siteAccessCodeRemove")).toHaveLength(1);
    expect(screen.getByText("Client reviewers")).toBeTruthy();
  });

  it("does not offer the gates' explanation's way forward over codes it could not add to", async () => {
    mountPanel({ sites: [OWNERLESS], accessGates: [], accessCodes: [reviewers()] }, { site: OWNERLESS });
    await rowOf("Client reviewers");
    await waitFor(() => expect(document.querySelector(".os-record-skeleton")).toBeNull());
    expect(screen.queryByText(/@accessGate/)).toBeNull();
  });

  it("offers everything again once the same row carries an owner", async () => {
    mountPanel(ownerlessSeed([reviewers()]), { site: DESK });
    const row = await rowOf("Client reviewers");
    expect(screen.queryByText(/has no owner/)).toBeNull();
    expect(within(row).getByRole("button", { name: "Regenerate Client reviewers" })).toBeTruthy();
    expect(await addControl()).toBeTruthy();
  });
});

describe("a removed code is not listed, and the list is in label order", () => {
  it("hides a removed code, and an empty list of them is empty", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ revokedAt: inMinutes(-1) })] });
    expect(await screen.findByText("No access codes yet")).toBeTruthy();
    expect(screen.queryByText("Client reviewers")).toBeNull();
  });

  it("orders by label, whatever order the rows arrived in and whatever was last written", async () => {
    mountPanel({
      accessGates: [DASHBOARD],
      accessCodes: [
        accessCodeRow({ id: "z", label: "Zebra", createdAt: "2026-10-30T11:59:00Z" }),
        accessCodeRow({ id: "a", label: "alpha", createdAt: "2026-10-01T00:00:00Z" }),
        accessCodeRow({ id: "m", label: "Mango", createdAt: "2026-10-15T00:00:00Z" }),
      ],
    });
    await screen.findByText("Zebra");
    const list = screen.getByRole("list", { name: "Access codes" });
    const names = within(list).getAllByRole("listitem").map((li) => li.querySelector(".os-row-name")?.textContent);
    expect(names).toEqual(["alpha", "Mango", "Zebra"]);
  });

  it("keeps a regenerated code where its label puts it", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers(), pilot()] });
    await answerYes("Client reviewers", "Regenerate");
    await screen.findByText(FIRST_CODE);
    const list = screen.getByRole("list", { name: "Access codes" });
    await waitFor(() => expect(list.textContent).toContain(`Ends in ${FIRST_CODE.slice(-4)}`));
    const names = within(list).getAllByRole("listitem").map((li) => li.querySelector(".os-row-name")?.textContent);
    expect(names).toEqual(["Client reviewers", "Pilot client"]);
  });
});

describe("lastOpenedAt reads as Last opened; its absence reads Not opened yet within 30 days of issuedAt, and Not opened in the last 30 days after, since sessions are retired at 30 days", () => {
  it.each<[string, string, string, string]>([
    ["opened two minutes ago", inMinutes(-2), inDays(-1), "Last opened 2 minutes ago"],
    ["opened an hour ago", inMinutes(-60), inDays(-1), "Last opened 1 hour ago"],
    ["opened three days ago", inDays(-3), inDays(-4), "Last opened 3 days ago"],
    ["opened a moment ago", inMinutes(0), inDays(-1), "Last opened just now"],
    ["opened ahead of this browser's clock", inMinutes(3), inDays(-1), "Last opened just now"],
    ["never opened, issued a day ago", "", inDays(-1), "Not opened yet"],
    ["never opened, issued exactly 30 days ago", "", inDays(-30), "Not opened yet"],
    ["no session left, issued 31 days ago", "", inDays(-31), "Not opened in the last 30 days"],
    ["opened before this secret was issued", inDays(-3), inDays(-1), "Not opened yet"],
    ["opened before an old secret was issued", inDays(-40), inDays(-35), "Not opened in the last 30 days"],
    ["no date readable at all", "", "", "Not opened in the last 30 days"],
    ["an unreadable date", "soon", inDays(-1), "Not opened yet"],
  ])("%s", (_case, lastOpenedAt, issuedAt, want) => {
    expect(lastOpenedWord(lastOpenedAt, issuedAt, NOW)).toBe(want);
  });

  it("draws the word on the row", async () => {
    mountPanel({ accessGates: [DASHBOARD], accessCodes: [reviewers({ issuedAt: inDays(-40) })] });
    expect((await rowOf("Client reviewers")).textContent).toContain("Not opened in the last 30 days");
  });
});

describe("the expiry word", () => {
  it.each<[string, string, string]>([
    ["eight minutes", inMinutes(8), "Expires in 8 minutes"],
    ["one minute", inMinutes(1), "Expires in 1 minute"],
    ["seconds left", new Date(NOW + 20_000).toISOString(), "Expires in under a minute"],
    ["just short of the hour", new Date(NOW + 59 * 60_000 + 40_000).toISOString(), "Expires in 1 hour"],
    ["three hours", inMinutes(180), "Expires in 3 hours"],
    ["just short of the day", inMinutes(23 * 60 + 40), "Expires in 1 day"],
    ["a week", inDays(7), "Expires in 7 days"],
    ["the very instant", new Date(NOW).toISOString(), "Expired"],
    ["a moment past", new Date(NOW - 1).toISOString(), "Expired"],
    ["a value that is not a date", "soon", "Expiry unknown"],
    ["no value", "", "Expiry unknown"],
  ])("%s", (_case, expiresAt, want) => {
    expect(expiryWord(expiresAt, NOW)).toBe(want);
  });
});

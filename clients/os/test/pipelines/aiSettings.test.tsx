import { act, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// Pipelines > Settings > AI (owner decision, 2026-10-29).
//
// ===========================================================================
// WHAT THIS PINS
// ===========================================================================
// Both switches are off unless the stored value turns them on, read the way
// the engine's `ParseAISettings` reads it; a toggle writes the WHOLE object
// through `setGlobalVariable` under the row's own id (or the seeder's); and a
// switch moves only on what the read-back answers, so a refused save never
// looks saved. The fake answers at `executeNamed`, under the generated
// builders, so the strings asserted below are the ones the engine parses.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { PipelinesApp } from "../../src/apps/pipelines/PipelinesApp";
import { PIPELINES_AI_OFF, PipelinesAISettings, parsePipelinesAI } from "../../src/apps/pipelines/PipelinesAISettings";
import { click, fakeConnection, rowsResult, withSession } from "../deployables/harness";

const READ = 'query globalVariable(name: "MEMQL_PIPELINES_AI")';
const DESCRIPTION = "Pipeline AI features: independent pull-request review and failed-run diagnosis. Off unless turned on here.";

/** One string argument of a rendered call, decoded the way the engine reads it. */
function arg(call: string, name: string): string | undefined {
  const match = new RegExp(`\\b${name}: ("(?:[^"\\\\]|\\\\.)*")`).exec(call);
  return match ? (JSON.parse(match[1]!) as string) : undefined;
}

/** The cluster's one variable row, behind the Deployables fake. */
function mount(initial: Row | null, faults: { refuse?: string; drop?: boolean; readError?: string; hold?: Promise<void> } = {}) {
  const fake = fakeConnection();
  const base = fake.query.executeNamed.bind(fake.query);
  let row = initial;
  const calls: string[] = [];
  (fake.query as unknown as { executeNamed: unknown }).executeNamed = async (name: string, call: string, opts?: unknown) => {
    if (call === READ) {
      calls.push(call);
      if (faults.readError) throw new Error(faults.readError);
      return rowsResult(row ? [row] : []);
    }
    if (call.startsWith("mutation setGlobalVariable(")) {
      calls.push(call);
      if (faults.hold) await faults.hold;
      if (faults.refuse) throw new Error(faults.refuse);
      if (!faults.drop) row = { id: arg(call, "id"), name: arg(call, "name"), value: arg(call, "value"), description: arg(call, "description") };
      return rowsResult([]);
    }
    return base(name, call, opts as never);
  };
  h.connection = fake;
  return {
    faults,
    /** Another browser's save: the stored row changes and its event arrives. */
    changeElsewhere: (next: Row) => {
      row = next;
      act(() => fake.subscriptions.emit("v1:platform:globalVariable", next));
    },
    reads: () => calls.filter((c) => c === READ),
    writes: () => calls.filter((c) => c.startsWith("mutation setGlobalVariable(")),
  };
}

async function switches() {
  const review = (await screen.findByRole("switch", { name: "AI review of pull requests" })) as HTMLInputElement;
  const diagnosis = screen.getByRole("switch", { name: "AI diagnosis of failed runs" }) as HTMLInputElement;
  return { review, diagnosis };
}

afterEach(() => {
  h.connection = null;
});

describe("reading the switches", () => {
  it("is off when the variable is absent, and loading writes nothing", async () => {
    const cluster = mount(null);
    render(<PipelinesAISettings />);
    const { review, diagnosis } = await switches();
    expect(review.checked).toBe(false);
    expect(diagnosis.checked).toBe(false);
    expect(review.disabled).toBe(false);
    expect(cluster.reads()).toHaveLength(1);
    expect(cluster.writes()).toEqual([]);
  });

  it("reads an unparseable value as off", async () => {
    mount({ id: "var-global-memql-pipelines-ai", name: "MEMQL_PIPELINES_AI", value: "review: yes" });
    render(<PipelinesAISettings />);
    const { review, diagnosis } = await switches();
    expect(review.checked).toBe(false);
    expect(diagnosis.checked).toBe(false);
  });

  it("says a failed read rather than drawing it as off", async () => {
    const cluster = mount(null, { readError: "permission denied" });
    render(<PipelinesAISettings />);
    await screen.findByText("AI settings could not be read.");
    expect(screen.getByText("permission denied")).toBeTruthy();
    expect(screen.queryByRole("switch")).toBeNull();
    cluster.faults.readError = undefined;
    await click(screen.getByRole("button", { name: "Try again" }));
    const { review } = await switches();
    expect(review.checked).toBe(false);
    expect(screen.queryByText("AI settings could not be read.")).toBeNull();
  });

  it("reads again when the variable changes elsewhere", async () => {
    const cluster = mount(null);
    render(<PipelinesAISettings />);
    const { review, diagnosis } = await switches();
    cluster.changeElsewhere({ id: "var-global-memql-pipelines-ai", name: "MEMQL_PIPELINES_AI", value: '{"review":false,"diagnosis":true}' });
    await waitFor(() => expect(diagnosis.checked).toBe(true));
    expect(review.checked).toBe(false);
    expect(cluster.reads()).toHaveLength(2);
    expect(cluster.writes()).toEqual([]);
  });

  it.each([
    ["", PIPELINES_AI_OFF],
    ["  ", PIPELINES_AI_OFF],
    ["not json", PIPELINES_AI_OFF],
    ["null", PIPELINES_AI_OFF],
    ["[]", PIPELINES_AI_OFF],
    ["true", PIPELINES_AI_OFF],
    ["{}", PIPELINES_AI_OFF],
    ['{"review":true}', { review: true, diagnosis: false }],
    ['{"review":true,"diagnosis":true}', { review: true, diagnosis: true }],
    // Go's decoder matches field names case-insensitively.
    ['{"Diagnosis":true}', { review: false, diagnosis: true }],
    // An unknown field, a non-boolean: the engine reads all off.
    ['{"review":true,"reviews":true}', PIPELINES_AI_OFF],
    ['{"review":"true","diagnosis":true}', PIPELINES_AI_OFF],
    ['{"review":1}', PIPELINES_AI_OFF],
    ['{"review":null,"diagnosis":true}', { review: false, diagnosis: true }],
  ])("parses %j as the engine does", (raw, expected) => {
    expect(parsePipelinesAI(raw)).toEqual(expected);
  });
});

describe("saving a switch", () => {
  it("writes the whole object under the seeder's id, then shows what the cluster holds", async () => {
    const cluster = mount(null);
    render(<PipelinesAISettings />);
    const { review, diagnosis } = await switches();
    await click(review);
    await waitFor(() => expect(review.checked).toBe(true));
    expect(diagnosis.checked).toBe(false);
    expect(cluster.writes()).toHaveLength(1);
    const call = cluster.writes()[0]!;
    expect(arg(call, "id")).toBe("var-global-memql-pipelines-ai");
    expect(arg(call, "name")).toBe("MEMQL_PIPELINES_AI");
    expect(arg(call, "value")).toBe('{"review":true,"diagnosis":false}');
    expect(arg(call, "description")).toBe(DESCRIPTION);
    expect(call).toContain("active: true");
    // Read back after the save, not assumed.
    expect(cluster.reads()).toHaveLength(2);
    expect(screen.queryByText("The change was not saved.")).toBeNull();
  });

  it("keeps the other switch and the existing row's id", async () => {
    const cluster = mount({ id: "var-global-pipelines-ai-legacy", name: "MEMQL_PIPELINES_AI", value: '{"review":true,"diagnosis":false}' });
    render(<PipelinesAISettings />);
    const { review, diagnosis } = await switches();
    expect(review.checked).toBe(true);
    await click(diagnosis);
    await waitFor(() => expect(diagnosis.checked).toBe(true));
    const call = cluster.writes()[0]!;
    expect(arg(call, "id")).toBe("var-global-pipelines-ai-legacy");
    expect(arg(call, "value")).toBe('{"review":true,"diagnosis":true}');
  });

  it("leaves a refused switch where the cluster has it, and Try again sends the same write", async () => {
    const cluster = mount(null, { refuse: "capability_not_held: write globalVariable" });
    render(<PipelinesAISettings />);
    const { review } = await switches();
    await click(review);
    const notice = (await screen.findByText("The change was not saved.")).closest(".os-notice") as HTMLElement;
    expect(within(notice).getByText("capability_not_held: write globalVariable")).toBeTruthy();
    expect(review.checked).toBe(false);
    await waitFor(() => expect(review.disabled).toBe(false));

    cluster.faults.refuse = undefined;
    await click(within(notice).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(review.checked).toBe(true));
    expect(cluster.writes()).toHaveLength(2);
    expect(cluster.writes()[1]).toBe(cluster.writes()[0]);
    expect(screen.queryByText("The change was not saved.")).toBeNull();
  });

  it("returns keyboard focus to the switch once the save settles", async () => {
    let release!: () => void;
    mount(null, { hold: new Promise<void>((done) => { release = done; }) });
    render(<PipelinesAISettings />);
    const { review, diagnosis } = await switches();
    review.focus();
    await click(review);
    expect(review.disabled).toBe(true);
    expect(diagnosis.disabled).toBe(true);
    // A browser hands focus to the page when the focused control is disabled.
    // jsdom keeps it there and ignores blur() on a disabled control, so the
    // page is given focus the way removal gives it.
    const throwaway = document.createElement("button");
    document.body.append(throwaway);
    throwaway.focus();
    throwaway.remove();
    expect(document.activeElement).toBe(document.body);
    release();
    await waitFor(() => expect(review.checked).toBe(true));
    await waitFor(() => expect(document.activeElement).toBe(review));
  });

  it("leaves focus where the person put it while the save ran", async () => {
    let release!: () => void;
    mount(null, { hold: new Promise<void>((done) => { release = done; }) });
    render(<PipelinesAISettings />);
    const { diagnosis } = await switches();
    const elsewhere = document.createElement("button");
    document.body.append(elsewhere);
    await click(diagnosis);
    elsewhere.focus();
    release();
    await waitFor(() => expect(diagnosis.checked).toBe(true));
    expect(document.activeElement).toBe(elsewhere);
    elsewhere.remove();
  });

  it("does not call a save the read-back contradicts saved", async () => {
    mount(null, { drop: true });
    render(<PipelinesAISettings />);
    const { review } = await switches();
    await click(review);
    await screen.findByText("The change was not saved.");
    expect(review.checked).toBe(false);
  });
});

describe("Pipelines > Settings", () => {
  it("offers AI beside Infrastructure and Connections", async () => {
    const cluster = mount(null);
    render(withSession(<PipelinesApp sectionId="settings" navigate={() => {}} askContext={() => {}} />));
    const tabs = screen.getByRole("navigation", { name: "Pipeline settings" });
    expect(within(tabs).getAllByRole("button").map((tab) => tab.textContent)).toEqual(["Infrastructure", "Connections", "AI"]);
    expect(screen.getByRole("region", { name: "Pipelines" })).toBeTruthy();
    expect(cluster.reads()).toEqual([]);

    await click(within(tabs).getByRole("button", { name: "AI" }));
    expect(within(tabs).getByRole("button", { name: "AI" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("region", { name: "AI settings" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Pipelines" })).toBeNull();
    const { review, diagnosis } = await switches();
    expect(review.checked).toBe(false);
    expect(diagnosis.checked).toBe(false);
  });
});

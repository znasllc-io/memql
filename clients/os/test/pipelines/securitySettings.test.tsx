import { act, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { PipelinesApp } from "../../src/apps/pipelines/PipelinesApp";
import { PipelinesSecuritySettings, parsePipelinesSecurity } from "../../src/apps/pipelines/PipelinesSecuritySettings";
import { parseCall } from "../callArgs";
import { click, fakeConnection, rowsResult, withSession } from "../deployables/harness";

const VARIABLE = "MEMQL_PIPELINES_SECURITY";
const ROW_ID = "var-global-memql-pipelines-security";
const storedRow = (enabled: boolean, id = ROW_ID): Row => ({ id, name: VARIABLE, value: JSON.stringify({ enabled }) });

// The switch is saved through `pipelinesSecuritySettingsSave`, which carries the
// switch alone: the engine owns the variable's row (its name, which row a save
// updates and the stored JSON: component/pipelinerun settings.go and its
// tests). The fake below does with a save what the engine does, and fails the
// test if the page ever writes the variable's row itself.

/** Exercise generated wire calls, with a cluster that can refuse or lose a save. */
function mount(initial: Row | null, faults: { refuse?: string; drop?: boolean; readError?: string; hold?: Promise<void>; duplicate?: Row } = {}) {
  const fake = fakeConnection();
  const base = fake.query.executeNamed.bind(fake.query);
  let row = initial;
  const writes: Record<string, unknown>[] = [];
  (fake.query as unknown as { executeNamed: unknown }).executeNamed = async (name: string, call: string, opts?: unknown) => {
    const parsed = parseCall(call);
    if (parsed.name === "globalVariable" && parsed.args["name"] === VARIABLE) {
      if (faults.readError) throw new Error(faults.readError);
      return rowsResult([...(row ? [row] : []), ...(faults.duplicate ? [faults.duplicate] : [])]);
    }
    if (parsed.name === "pipelinesSecuritySettingsSave") {
      writes.push(parsed.args);
      if (faults.hold) await faults.hold;
      if (faults.refuse) throw new Error(faults.refuse);
      if (!faults.drop) row = storedRow(parsed.args["enabled"] === true, (row?.["id"] as string | undefined) ?? ROW_ID);
      return rowsResult([]);
    }
    if (parsed.name === "setGlobalVariable") throw new Error("the page wrote the variable row itself: " + call);
    return base(name, call, opts as never);
  };
  h.connection = fake;
  return {
    faults, writes, row: () => row,
    changeElsewhere: (next: Row) => {
      row = next;
      act(() => fake.subscriptions.emit("v1:platform:globalVariable", next));
    },
  };
}

const switchControl = async () => await screen.findByRole("switch", { name: "Weekly security scans" }) as HTMLInputElement;
afterEach(() => { h.connection = null; });

describe("security opt-in", () => {
  it("defaults off without writing, enables, then disables and confirms the stored value", async () => {
    const cluster = mount(null);
    render(<PipelinesSecuritySettings />);
    const toggle = await switchControl();
    expect(toggle.checked).toBe(false);
    expect(cluster.writes).toEqual([]);
    await click(toggle);
    await waitFor(() => expect(toggle.checked).toBe(true));
    // The switch and nothing else: the row's name, id and JSON are the engine's.
    expect(cluster.writes[0]).toEqual({ enabled: true });
    await click(toggle);
    await waitFor(() => expect(toggle.checked).toBe(false));
    expect(cluster.row()?.["value"]).toBe('{"enabled":false}');
    expect(cluster.writes).toEqual([{ enabled: true }, { enabled: false }]);
  });

  it("turns an enabled setting off with the switch alone", async () => {
    const cluster = mount(storedRow(true, "existing-security-variable"));
    render(<PipelinesSecuritySettings />);
    const toggle = await switchControl();
    expect(toggle.checked).toBe(true);
    await click(toggle);
    await waitFor(() => expect(toggle.checked).toBe(false));
    expect(cluster.writes).toEqual([{ enabled: false }]);
  });

  it.each([
    ["", false], [" ", false], ["not json", false], ["null", false], ["[]", false], ["true", false],
    ["{}", false], ['{"enabled":true}', true], ['{"enabled":false}', false], ['{"Enabled":true}', true],
    ['{"enabled":null}', false], ['{"enabled":"true"}', false], ['{"enabled":1}', false],
    ['{"enabled":true,"unknown":false}', false], ['{"enabled":true} {}', false],
    ['{"enabled":true,"Enabled":null}', false], ['{"enabled":true,"enabled":null}', false],
    ['{"enabled":false,"enabled":true}', true],
  ])("reads %j as the engine does", (raw, expected) => {
    expect(parsePipelinesSecurity(raw)).toBe(expected);
  });

  it("shows a failed read without presenting it as disabled, and can recover", async () => {
    const cluster = mount(storedRow(true), { readError: "permission denied" });
    render(<PipelinesSecuritySettings />);
    await screen.findByText("Security settings could not be read.");
    expect(screen.queryByRole("switch")).toBeNull();
    cluster.faults.readError = undefined;
    await click(screen.getByRole("button", { name: "Try again" }));
    expect((await switchControl()).checked).toBe(true);
  });

  it("keeps a refused change off and permits an explicit retry", async () => {
    const refusal = "capability_not_held: \"pipelinesSecuritySettingsSave\" requires execute on app:pipelines/settings";
    const cluster = mount(null, { refuse: refusal });
    render(<PipelinesSecuritySettings />);
    const toggle = await switchControl();
    await click(toggle);
    await screen.findByText("The change was not saved.");
    expect(toggle.checked).toBe(false);
    expect(screen.getByText(refusal)).toBeTruthy();
    cluster.faults.refuse = undefined;
    await click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(toggle.checked).toBe(true));
    expect(cluster.writes[1]).toEqual(cluster.writes[0]);
  });

  it("does not choose or overwrite one of two active configuration rows", async () => {
    const cluster = mount(storedRow(true), { duplicate: storedRow(false, "duplicate-security-setting") });
    render(<PipelinesSecuritySettings />);
    await screen.findByText("Security settings could not be read.");
    expect(screen.getByText("More than one active security setting exists. Keep one configuration and try again.")).toBeTruthy();
    expect(screen.queryByRole("switch")).toBeNull();
    expect(cluster.writes).toEqual([]);
  });

  it("does not call an unconfirmed write saved", async () => {
    mount(null, { drop: true });
    render(<PipelinesSecuritySettings />);
    const toggle = await switchControl();
    await click(toggle);
    await screen.findByText("The change was not saved.");
    expect(toggle.checked).toBe(false);
  });

  it("rereads changes made elsewhere, including a disable", async () => {
    const cluster = mount(null);
    render(<PipelinesSecuritySettings />);
    const toggle = await switchControl();
    cluster.changeElsewhere(storedRow(true));
    await waitFor(() => expect(toggle.checked).toBe(true));
    cluster.changeElsewhere(storedRow(false));
    // The shared graph invalidator coalesces successive events over one second.
    await waitFor(() => expect(toggle.checked).toBe(false), { timeout: 2_000 });
    expect(cluster.writes).toEqual([]);
  });

  it("holds repeated presses during a save and restores keyboard focus", async () => {
    let release!: () => void;
    const cluster = mount(null, { hold: new Promise<void>(done => { release = done; }) });
    render(<PipelinesSecuritySettings />);
    const toggle = await switchControl();
    toggle.focus();
    await click(toggle);
    expect(toggle.disabled).toBe(true);
    expect(toggle.checked).toBe(false);
    await click(toggle);
    expect(cluster.writes).toHaveLength(1);
    // jsdom retains focus on disabled controls; mimic the browser's body fallback.
    const temporary = document.createElement("button");
    document.body.append(temporary);
    temporary.focus();
    temporary.remove();
    release();
    await waitFor(() => expect(toggle.checked).toBe(true));
    await waitFor(() => expect(document.activeElement).toBe(toggle));
  });
});

it("offers Security in Pipelines settings and reads only when opened", async () => {
  const cluster = mount(null);
  render(withSession(<PipelinesApp sectionId="settings" navigate={() => {}} askContext={() => {}} />));
  expect(screen.queryByRole("switch", { name: "Weekly security scans" })).toBeNull();
  const tabs = screen.getByRole("navigation", { name: "Pipeline settings" });
  await click(within(tabs).getByRole("button", { name: "Security" }));
  expect(within(tabs).getByRole("button", { name: "Security" }).getAttribute("aria-current")).toBe("page");
  expect((await switchControl()).checked).toBe(false);
  expect(cluster.writes).toEqual([]);
});

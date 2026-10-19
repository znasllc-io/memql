import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));
const { SharingGroup } = await import("../../src/apps/fleet/machines/SharingGroup");
const { OwnerFeaturesGroup } = await import("../../src/apps/fleet/machines/OwnerFeaturesGroup");
const { useMachineWrites } = await import("../../src/apps/fleet/machines/useMachineWrites");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { fakeConnection, machineRow, shareDirectoryRow, withSession } = await import("./harness");

afterEach(cleanup);
const owner = "v1:identity:user:olivia";
const audience = { mode: "people", userIds: ["ana"], groupIds: ["design"], pausedUserIds: ["ana"], pausedGroupIds: [] };
const features = { headless: true, apps: true, computerUse: true, updatedAt: "2026-10-17T02:00:00Z" };
function row(over: Partial<Row> = {}) {
  return machineRow({ id: "studio", ownerUserId: "olivia", sharing: audience, workbenchSharing: { ...audience, userIds: ["bo"], pausedUserIds: [] }, ownerFeatures: features, ...over });
}
function Panel({ value }: { value: Row }) {
  const writes = useMachineWrites(), machine = machineFromRow(value);
  return <>
    <OwnerFeaturesGroup machine={machine} writes={writes} />
    <section aria-label="Models"><SharingGroup machine={machine} writes={writes} /></section>
    <section aria-label="Workbenches"><SharingGroup machine={machine} writes={writes} resource="workbench" /></section>
  </>;
}
function mount(value = row(), userId = owner) {
  const conn = fakeConnection({ fleetShareDirectory: [shareDirectoryRow({ people: [{ id: "ana", name: "Ana", detail: "" }, { id: "bo", name: "Bo", detail: "" }], groups: [{ id: "design", name: "Design", members: 3 }] })] });
  h.connection = conn;
  const node = (next: Row) => withSession(<Panel value={next} />, { userId });
  const view = render(node(value));
  return { conn, rerender: (next: Row) => view.rerender(node(next)) };
}
async function settle() { await act(async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); }); }
async function toggle(name: string) { fireEvent.click(screen.getByRole("switch", { name })); await settle(); }

describe("independent machine feature controls", () => {
  it("pauses and resumes models with the audience and individual pauses preserved", async () => {
    const { conn } = mount(); await settle();
    await toggle("Share models");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ mode: "people", paused: true, userIds: ["ana"], groupIds: ["design"], pausedUserIds: ["ana"] }));
    expect((screen.getByRole("switch", { name: "Share fleet workbenches" }) as HTMLInputElement).checked).toBe(true);
    // Off hides the models audience; the workbench audience is its own.
    expect(within(screen.getByRole("region", { name: "Models" })).queryByText("Ana")).toBeNull();
    expect(within(screen.getByRole("region", { name: "Workbenches" })).getByText("Bo")).toBeTruthy();
    await toggle("Share models");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ paused: false, userIds: ["ana"], pausedUserIds: ["ana"] }));
    expect(within(screen.getByRole("region", { name: "Models" })).getByText("Ana")).toBeTruthy();
    expect(conn.query.fleetSetOwnerFeatures).not.toHaveBeenCalled();
  });
  it("uses a separate workbench audience and resource for directory reads and writes", async () => {
    const { conn } = mount(); await settle();
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledWith({ registrationId: "studio", resource: "workbench" }, expect.objectContaining({ signal: expect.any(AbortSignal) }));
    await toggle("Share fleet workbenches");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ resource: "workbench", paused: true, userIds: ["bo"], groupIds: ["design"] }));
    expect((screen.getByRole("switch", { name: "Share models" }) as HTMLInputElement).checked).toBe(true);
    await toggle("Share fleet workbenches");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ resource: "workbench", paused: false, userIds: ["bo"] }));
  });
  it("holds a private change pending, then adopts the live feed without modifying sharing", async () => {
    const { conn, rerender } = mount(); await settle();
    let finish!: () => void;
    conn.query.fleetSetOwnerFeatures.mockImplementationOnce(() => new Promise<void>(resolve => { finish = resolve; }));
    await toggle("Commands and files");
    expect(conn.query.fleetSetOwnerFeatures).toHaveBeenCalledExactlyOnceWith({ registrationId: "studio", headless: false, apps: true, computerUse: true, expectedUpdatedAt: features.updatedAt });
    expect((screen.getByRole("switch", { name: "Computer use" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => finish());
    rerender(row({ ownerFeatures: { ...features, headless: false, updatedAt: "2026-10-17T02:01:00Z" } })); await settle();
    expect((screen.getByRole("switch", { name: "Commands and files" }) as HTMLInputElement).checked).toBe(false);
    expect((screen.getByRole("switch", { name: "CLIs and apps" }) as HTMLInputElement).checked).toBe(true);
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });
  it("retries a failed private change, but drops the retry after a conflicting live edit", async () => {
    const { conn, rerender } = mount(); await settle();
    conn.query.fleetSetOwnerFeatures.mockRejectedValue(new Error("connection closed"));
    await toggle("Computer use");
    expect((screen.getByRole("switch", { name: "Computer use" }) as HTMLInputElement).checked).toBe(true);
    expect(screen.getByText("connection closed")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Try again" })); await settle();
    expect(conn.query.fleetSetOwnerFeatures).toHaveBeenCalledTimes(2);
    rerender(row({ ownerFeatures: { ...features, apps: false, updatedAt: "2026-10-17T02:02:00Z" } })); await settle();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await toggle("Computer use");
    expect(conn.query.fleetSetOwnerFeatures).toHaveBeenLastCalledWith({ registrationId: "studio", headless: true, apps: false, computerUse: false, expectedUpdatedAt: "2026-10-17T02:02:00Z" });
  });
  it("does not grant a cluster administrator control of somebody else's private features", async () => {
    const { conn } = mount(row(), "other-owner"); await settle();
    for (const name of ["Commands and files", "CLIs and apps", "Computer use"]) {
      expect((screen.getByRole("switch", { name }) as HTMLButtonElement).disabled).toBe(true);
      await toggle(name);
    }
    expect(conn.query.fleetSetOwnerFeatures).not.toHaveBeenCalled();
    expect(screen.queryByRole("switch", { name: "Share fleet workbenches" })).toBeNull();
  });
  it("reads legacy owner permissions and malformed explicit settings conservatively", () => {
    expect(machineFromRow(row({ ownerFeatures: undefined })).ownerFeatures).toEqual({ headless: true, apps: true, computerUse: true, updatedAt: "" });
    expect(machineFromRow(row({ ownerFeatures: [] })).ownerFeatures).toEqual({ headless: false, apps: false, computerUse: false, updatedAt: "" });
    expect(machineFromRow(row({ workbenchSharing: { mode: "cluster" } })).workbenchSharing?.mode).toBe("owner");
  });
});

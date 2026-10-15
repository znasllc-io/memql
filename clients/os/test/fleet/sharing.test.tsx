import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { type Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { SharingGroup } = await import("../../src/apps/fleet/machines/SharingGroup");
const { useMachineWrites } = await import("../../src/apps/fleet/machines/useMachineWrites");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { sharingActiveCalls } = await import("../../src/apps/fleet/machines/SharingEditor");
const { builtinReply, fakeConnection, machineRow, shareDirectoryRow, withSession } = await import("./harness");

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

type Conn = ReturnType<typeof fakeConnection>;

const OWNER = "v1:identity:user:olivia";
const MACHINE_ID = "v1:worker:registration:studio";
const LABEL = "Studio mini";

const ANA = { id: "ana", name: "Ana Ruiz", detail: "" };
const BO = { id: "bo", name: "Bo Chen", detail: "" };
const CY = { id: "cy", name: "Cy Diaz", detail: "" };
const DESIGN = { id: "design", name: "Design", members: 4 };
const OPS = { id: "ops", name: "Ops", members: 1 };

function studio(sharing?: unknown, over: Partial<Row> = {}): Row {
  return machineRow({
    id: MACHINE_ID,
    ownerUserId: "olivia",
    displayName: LABEL,
    ...(sharing === undefined ? {} : { sharing }),
    ...over,
  });
}

function directory(over: Partial<Row> = {}): Row {
  return shareDirectoryRow({ id: MACHINE_ID, machineId: MACHINE_ID, people: [ANA, BO, CY], groups: [DESIGN, OPS], ...over });
}

function Panel({ row }: { row: Row }) {
  const writes = useMachineWrites();
  return <SharingGroup machine={machineFromRow(row)} writes={writes} standalone />;
}

function mount(
  row: Row,
  opts: {
    conn?: Conn;
    directory?: Row;
    userId?: string;
  } = {},
) {
  const conn = opts.conn ?? fakeConnection({ fleetShareDirectory: [opts.directory ?? directory()] });
  h.connection = conn;
  const node = (next: Row) =>
    withSession(<Panel row={next} />, { userId: opts.userId ?? OWNER });
  const view = render(node(row));
  return { conn, rerender: (next: Row) => view.rerender(node(next)) };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i += 1) await Promise.resolve();
  });
}

const shared = (over: Record<string, unknown> = {}) => ({ mode: "people", userIds: ["ana"], groupIds: ["design"], ...over });
function searchBox() { return screen.getByRole("combobox", { name: "Search users and groups" }); }
async function click(label: string) { fireEvent.click(screen.getByRole("button", { name: label })); await settle(); }
async function findAndAdd(name: string) {
  fireEvent.focus(searchBox()); fireEvent.change(searchBox(), { target: { value: name } });
  fireEvent.click(screen.getByRole("option", { name })); await settle();
}

describe("the sharing access list", () => {
  it("only reads the directory when sharing is enabled, and saves the switch directly", async () => {
    const { conn } = mount(studio()); await settle();
    expect(conn.query.fleetShareDirectory).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("switch", { name: "Share models" })); await settle();
    expect(conn.query.fleetSetSharing).toHaveBeenCalledWith(expect.objectContaining({ mode: "people", userIds: [], groupIds: [] }));
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("region", { name: "Users and groups" })).toBeTruthy();
  });
  it("shows current users and groups as rows and searches only entries not yet added", async () => {
    mount(studio(shared())); await settle();
    const list = screen.getByRole("list", { name: "Shared users and groups" });
    expect(within(list).getByText("Ana Ruiz")).toBeTruthy();
    expect(within(list).getByText("Design")).toBeTruthy();
    fireEvent.focus(searchBox());
    expect(screen.queryByRole("option", { name: "Ana Ruiz" })).toBeNull();
    expect(screen.getByRole("option", { name: "Bo Chen" })).toBeTruthy();
  });
  it("adds a searched user immediately without a confirmation sentence", async () => {
    const { conn } = mount(studio(shared())); await settle(); await findAndAdd("Bo Chen");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ userIds: ["ana", "bo"], groupIds: ["design"] }));
    expect(screen.getByRole("button", { name: "Pause sharing with Bo Chen" })).toBeTruthy();
    expect(screen.queryByText(/Added Bo|Lent to/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });
  it("searches and adds with the keyboard", async () => {
    mount(studio(shared())); await settle();
    fireEvent.focus(searchBox()); fireEvent.change(searchBox(), { target: { value: "Ops" } });
    fireEvent.keyDown(searchBox(), { key: "Enter" }); await settle();
    expect(screen.getByRole("button", { name: "Pause sharing with Ops" })).toBeTruthy();
  });
  it("pauses then resumes a grant without deleting the recipient", async () => {
    const { conn } = mount(studio(shared())); await settle();
    await click("Pause sharing with Design");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ groupIds: ["design"], pausedGroupIds: ["design"] }));
    expect(screen.getByText("Paused")).toBeTruthy();
    await click("Resume sharing with Design");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ groupIds: ["design"], pausedGroupIds: [] }));
    expect(screen.queryByText("Paused")).toBeNull();
  });
  it("preserves the other paused grants when a user changes", async () => {
    const { conn } = mount(studio(shared({ pausedGroupIds: ["design"] }))); await settle();
    await click("Pause sharing with Ana Ruiz");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ pausedUserIds: ["ana"], pausedGroupIds: ["design"] }));
  });
  it("removes a paused entry and lets it be found and added again with sharing resumed", async () => {
    const { conn } = mount(studio(shared({ pausedGroupIds: ["design"] }))); await settle();
    await click("Remove sharing with Design");
    expect(screen.queryByRole("button", { name: /sharing with Design/ })).toBeNull();
    await findAndAdd("Design");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ groupIds: ["design"], pausedGroupIds: [] }));
    expect(screen.getByRole("button", { name: "Pause sharing with Design" })).toBeTruthy();
  });
  it("keeps the empty access list searchable after removing its final entry", async () => {
    const { conn } = mount(studio(shared({ groupIds: [] }))); await settle();
    await click("Remove sharing with Ana Ruiz");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ mode: "people", userIds: [], groupIds: [] }));
    expect(searchBox()).toBeTruthy(); await findAndAdd("Ana Ruiz");
    expect(screen.getByRole("button", { name: "Pause sharing with Ana Ruiz" })).toBeTruthy();
  });
  it("leaves the saved state intact on failure and retries the action", async () => {
    const { conn } = mount(studio(shared())); await settle();
    conn.query.fleetSetSharing.mockRejectedValueOnce(new Error("connection closed"));
    await click("Pause sharing with Design");
    expect(screen.getByRole("button", { name: "Pause sharing with Design" })).toBeTruthy();
    expect(screen.getByText("Sharing could not be updated.")).toBeTruthy();
    await click("Try again");
    expect(screen.getByRole("button", { name: "Resume sharing with Design" })).toBeTruthy();
    expect(screen.queryByText("Sharing could not be updated.")).toBeNull();
  });
  it("serializes rapid clicks while a write is pending", async () => {
    const { conn } = mount(studio(shared())); await settle();
    let finish!: () => void;
    conn.query.fleetSetSharing.mockImplementationOnce(() => new Promise((resolve) => { finish = () => resolve(builtinReply("fleetSetSharing", [])); }));
    fireEvent.click(screen.getByRole("button", { name: "Pause sharing with Design" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove sharing with Ana Ruiz" }));
    expect(conn.query.fleetSetSharing).toHaveBeenCalledTimes(1);
    expect((searchBox() as HTMLInputElement).disabled).toBe(true);
    await act(async () => finish()); await settle();
    expect(screen.getByRole("button", { name: "Resume sharing with Design" })).toBeTruthy();
  });
  it("keeps an acknowledged result through heartbeats, then follows a later sharing change", async () => {
    const { rerender } = mount(studio(shared())); await settle(); await click("Pause sharing with Design");
    rerender(studio(shared(), { activeCount: 3 })); await settle();
    expect(screen.getByRole("button", { name: "Resume sharing with Design" })).toBeTruthy();
    rerender(studio(shared({ pausedGroupIds: ["design"] }))); await settle();
    rerender(studio(shared({ userIds: ["bo"] }))); await settle();
    expect(screen.getByRole("button", { name: "Pause sharing with Bo Chen" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /sharing with Ana Ruiz/ })).toBeNull();
  });
  it("does not replay a failed edit over a newer live sharing decision", async () => {
    const { conn, rerender } = mount(studio(shared())); await settle();
    conn.query.fleetSetSharing.mockRejectedValueOnce(new Error("lost")); await click("Pause sharing with Design");
    rerender(studio(shared({ userIds: ["bo"] }))); await settle();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(screen.getByRole("button", { name: "Pause sharing with Bo Chen" })).toBeTruthy();
  });
  it("allows removing a stale recipient and retains the last directory on refresh failure", async () => {
    const { conn } = mount(studio(shared()), { directory: directory({ people: [], current: { people: [{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: false }], groups: [] } }) });
    await settle(); expect(screen.getByText("Unavailable")).toBeTruthy();
    conn.query.fleetShareDirectory.mockRejectedValueOnce(new Error("offline"));
    fireEvent(window, new Event("online")); await settle();
    expect(screen.getByRole("button", { name: "Remove sharing with Ana Ruiz" })).toBeTruthy();
    await click("Remove sharing with Ana Ruiz");
    expect(conn.query.fleetSetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ userIds: [] }));
  });
  it("exposes retry after a directory failure without claiming the list is empty", async () => {
    const conn = fakeConnection(); conn.query.fleetShareDirectory.mockRejectedValueOnce(new Error("lost"));
    mount(studio(shared()), { conn }); await settle();
    expect(screen.getByText("Users and groups could not be refreshed.")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Remove sharing with Person/ })).toBeTruthy();
  });
  it("hides write controls from another person and from removed machines", async () => {
    const { rerender, conn } = mount(studio(shared()), { userId: "someone-else" }); await settle();
    expect(screen.queryByRole("switch")).toBeNull();
    rerender(studio(shared(), { revokedAt: new Date().toISOString() })); await settle();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });
  it("does not show the weekly ledger or routine save and explanatory captions", async () => {
    mount(studio(shared())); await settle();
    for (const text of [/No calls have run/, /Changes take effect/, /Everyone you can choose/, /Added /, /Lent to /]) expect(screen.queryByText(text)).toBeNull();
  });
  it("refreshes live counts while paused and does not query activity separately", async () => {
    const report = { nodeId: "agent-1", observedAt: new Date().toISOString(), users: [{ id: "ana", activeCalls: 1 }], groups: [{ id: "design", activeCalls: 2 }] };
    const base = { connectedNodeId: "agent-1", lastSeenAt: new Date().toISOString() };
    const { conn, rerender } = mount(studio(shared({ pausedGroupIds: ["design"] }), { ...base, sharingActivity: report })); await settle();
    expect(screen.getByText("1 active call")).toBeTruthy(); expect(screen.getByText("2 active calls")).toBeTruthy();
    rerender(studio(shared({ pausedGroupIds: ["design"] }), { ...base, sharingActivity: { ...report, groups: [] } })); await settle();
    expect(screen.getByText("0 active calls")).toBeTruthy();
    expect(conn.query.fleetSharingLedger).not.toHaveBeenCalled();
  });
  it("distinguishes unknown, stale and disconnected counts from an observed zero", () => {
    const subject = { kind: "person" as const, id: "ana" };
    const now = Date.now(), base = { connectedNodeId: "agent-1", lastSeenAt: new Date(now).toISOString() };
    const m = machineFromRow(studio(shared(), base));
    expect(sharingActiveCalls(m, subject, now)).toBeNull();
    const report = { nodeId: "agent-1", observedAt: new Date(now).toISOString(), users: [], groups: [] };
    expect(sharingActiveCalls({ ...m, sharingActivity: report }, subject, now)).toBe(0);
    expect(sharingActiveCalls({ ...m, sharingActivity: { ...report, nodeId: "old-agent" } }, subject, now)).toBeNull();
    expect(sharingActiveCalls({ ...m, lastSeenAt: new Date(now + 50_000).toISOString(), sharingActivity: report }, subject, now + 50_000)).toBeNull();
    expect(sharingActiveCalls({ ...m, connectedNodeId: "" }, subject, now)).toBe(0);
  });
});

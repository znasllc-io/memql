import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { buildFleetSetSharing, type Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { SharingGroup } = await import("../../src/apps/fleet/machines/SharingGroup");
const { MachineDetail } = await import("../../src/apps/fleet/machines/MachineDetail");
const { useMachineWrites } = await import("../../src/apps/fleet/machines/useMachineWrites");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { sharingLinkWords } = await import("../../src/apps/fleet/machines/sharing");
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

/** A machine whose cockpit has agreed to serve people other than its owner. */
const willing = { capabilityDescriptor: { inferenceServe: "cluster" } };

function directory(over: Partial<Row> = {}): Row {
  return shareDirectoryRow({ id: MACHINE_ID, machineId: MACHINE_ID, people: [ANA, BO, CY], groups: [DESIGN, OPS], ...over });
}

function current(people: Row[] = [], groups: Row[] = []): Row {
  return { people, groups };
}

function Panel({ row, ledger }: { row: Row; ledger: { sentence: string; readable: boolean } | null }) {
  const writes = useMachineWrites();
  return <SharingGroup machine={machineFromRow(row)} writes={writes} ledger={ledger} standalone />;
}

function mount(
  row: Row,
  opts: {
    conn?: Conn;
    directory?: Row;
    userId?: string;
    ledger?: { sentence: string; readable: boolean } | null;
  } = {},
) {
  const conn = opts.conn ?? fakeConnection({ fleetShareDirectory: [opts.directory ?? directory()] });
  h.connection = conn;
  const node = (next: Row) =>
    withSession(<Panel row={next} ledger={opts.ledger ?? null} />, { userId: opts.userId ?? OWNER });
  const view = render(node(row));
  return { conn, rerender: (next: Row) => view.rerender(node(next)) };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i += 1) await Promise.resolve();
  });
}

async function openEditor(): Promise<HTMLElement> {
  const editor = screen.getByRole("region", { name: "Model sharing" });
  within(editor).getByRole("switch", { name: "Share models" }).focus();
  await settle();
  return editor;
}

async function setSharing(editor: HTMLElement, enabled: boolean): Promise<void> {
  const toggle = within(editor).getByRole("switch", { name: "Share models" }) as HTMLInputElement;
  if (toggle.checked !== enabled) fireEvent.click(toggle);
  await settle();
}

function canSave(editor: HTMLElement): boolean {
  const save = within(editor).queryByRole("button", { name: /^(Save|Saving\.\.\.)$/ }) as HTMLButtonElement | null;
  return save !== null && !save.disabled;
}

function saveButton(editor: HTMLElement): HTMLButtonElement {
  return within(editor).getByRole("button", { name: /^(Save|Saving\.\.\.)$/ }) as HTMLButtonElement;
}

function search(editor: HTMLElement): HTMLInputElement {
  return within(editor).getByRole("combobox", { name: "Search people and groups" }) as HTMLInputElement;
}

/** The chips the editor holds, as a person reads them. */
function chipTexts(editor: HTMLElement): string[] {
  const list = within(editor).getByRole("list", { name: "Chosen people and groups" });
  return within(list)
    .getAllByRole("listitem")
    .map((chip) => chip.textContent ?? "");
}

const MOVED = "Sharing changed somewhere else while this was open.";

describe("the inline sharing panel", () => {
  it("starts private, without a second view or an unnecessary directory read", () => {
    const { conn } = mount(studio());
    expect((screen.getByRole("switch", { name: "Share models" }) as HTMLInputElement).checked).toBe(false);
    expect(screen.getByText("Only you can use this machine’s models.")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("button", { name: "Change sharing" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.queryByText(/policy\.yaml/)).toBeNull();
    expect(conn.query.fleetShareDirectory).not.toHaveBeenCalled();
  });

  it("shows the saved audience directly and reads its names once", async () => {
    const { conn } = mount(studio({ mode: "people", userIds: ["ana"], groupIds: ["design"] }, willing));
    const editor = await openEditor();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz", "Design"]);
    expect((within(editor).getByRole("switch") as HTMLInputElement).checked).toBe(true);
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(1);
    expect(within(editor).queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("preserves an existing cluster share until the owner chooses a narrower audience", async () => {
    const { conn } = mount(studio({ mode: "cluster" }));
    const editor = await openEditor();
    expect(screen.getByText("Currently shared with everyone in this cluster.")).toBeTruthy();
    expect(canSave(editor)).toBe(false);
    fireEvent.click(within(editor).getByRole("button", { name: "Choose people and groups" }));
    await settle();
    expect(canSave(editor)).toBe(false);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({ registrationId: MACHINE_ID, mode: "people", userIds: ["ana"], groupIds: [] });
  });

  it("offers editing only to the owner, without reading another owner's directory", () => {
    const { conn } = mount(studio({ mode: "people", userIds: ["ana"] }), { userId: "someone-else" });
    expect(screen.queryByRole("switch")).toBeNull();
    expect(screen.getByText("Shared with 1 person")).toBeTruthy();
    expect(screen.getByText("The person who enrolled this machine needs to enable model sharing in Cockpit.")).toBeTruthy();
    expect(conn.query.fleetShareDirectory).not.toHaveBeenCalled();
  });

  it("offers no edit for a removed machine", () => {
    mount(studio({ mode: "cluster" }, { revokedAt: "2026-09-01T00:00:00Z" }));
    expect(screen.queryByRole("switch")).toBeNull();
    expect(screen.getByText("This machine was removed, so it cannot be shared.")).toBeTruthy();
  });
});

describe("the words on the way into Sharing", () => {
  const words = (block: unknown, serve?: string) =>
    sharingLinkWords(
      machineFromRow(studio(block, serve === undefined ? {} : { capabilityDescriptor: { inferenceServe: serve } })),
    );

  it("say what the machine DOES: the owner's mode and the cockpit's consent together", () => {
    expect(words(undefined, "cluster")).toBe("Personal inference");
    expect(words({ mode: "people", userIds: ["ana"] }, "cluster")).toBe("Shared with people");
    expect(words({ mode: "cluster" }, "cluster")).toBe("Shared with everyone");
    expect(words({ mode: "people", userIds: ["ana"] })).toBe("Personal inference");
    expect(words({ mode: "cluster" }, "owner")).toBe("Personal inference");
  });
});

describe("the week's ledger", () => {
  const WEEK = "Served 41 calls this week, 12 of them for 2 other people.";

  it.each([
    ["people", { mode: "people", userIds: ["ana"] }],
    ["cluster", { mode: "cluster" }],
  ])("is shown for a %s share the machine is serving", (_mode, block) => {
    mount(studio(block, willing), { ledger: { sentence: WEEK, readable: true } });
    expect(screen.getByText(WEEK)).toBeTruthy();
  });

  it("is not shown while the machine serves nobody else", () => {
    const first = mount(studio(undefined, willing), { ledger: { sentence: WEEK, readable: true } });
    expect(screen.queryByText(WEEK)).toBeNull();
    first.rerender(studio({ mode: "people", userIds: ["ana"] }));
    expect(screen.queryByText(WEEK)).toBeNull();
  });

  it("does not let a ledger that could not be read look like a count", () => {
    mount(studio({ mode: "cluster" }, willing), {
      ledger: {
        sentence: "This week's usage could not be read. It is not that nothing ran -- nobody looked.",
        readable: false,
      },
    });
    const notice = screen.getByText(/nobody looked/).closest(".os-notice");
    expect(notice?.getAttribute("data-tone")).toBe("warn");
  });

  it("reads the engine's sentence off the builtin's wire reply", async () => {
    const conn = fakeConnection({
      fleetSharingLedger: [{ id: MACHINE_ID, machineId: MACHINE_ID, sentence: WEEK, readable: true }],
      fleetShareDirectory: [directory()],
    });
    h.connection = conn;
    function Page() {
      const writes = useMachineWrites();
      return (
        <MachineDetail
          machine={machineFromRow(studio({ mode: "cluster" }, willing))}
          writes={writes}
          now={new Date()}
          view="sharing"
        />
      );
    }
    render(withSession(<Page />, { userId: OWNER }));
    expect(await screen.findByText(WEEK)).toBeTruthy();
  });
});

describe("the inline sharing editor", () => {
  it("reads names only after enabling sharing, reusing them through edits and discards", async () => {
    const { conn } = mount(studio());
    const editor = await openEditor();
    expect(conn.query.fleetShareDirectory).not.toHaveBeenCalled();
    await setSharing(editor, true);
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(1);
    expect(conn.query.fleetShareDirectory.mock.calls[0]?.[0]).toEqual({ registrationId: MACHINE_ID });
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    fireEvent.click(within(editor).getByRole("button", { name: "Discard changes" }));
    expect((within(editor).getByRole("switch") as HTMLInputElement).checked).toBe(false);
    await setSharing(editor, true);
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(1);
    expect(within(editor).queryByRole("button", { name: "Remove Ana Ruiz" })).toBeNull();
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });

  it("refreshes the visible directory without losing choices or focus, and pauses when hidden", async () => {
    const { conn } = mount(studio());
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    const box = search(editor);
    box.focus();
    conn.query.fleetShareDirectory.mockResolvedValueOnce(builtinReply("fleetShareDirectory", [directory({ groups: [...[DESIGN, OPS], { id: "new", name: "New group", members: 2 }] })]));
    fireEvent(window, new Event("focus"));
    await settle();
    expect(within(editor).getByRole("option", { name: /New group/ })).toBeTruthy();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(document.activeElement).toBe(box);
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(2);
    editor.hidden = true;
    vi.spyOn(Date, "now").mockReturnValue(Date.now() + 1_500);
    fireEvent(window, new Event("online"));
    await settle();
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(2);
    editor.hidden = false;
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });

  it("keeps the last directory and draft through a failed refresh, then retries inline", async () => {
    const { conn } = mount(studio());
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    const box = search(editor);
    box.focus();
    conn.query.fleetShareDirectory.mockRejectedValueOnce(new Error("stream closed"));
    fireEvent(window, new Event("focus"));
    await settle();
    expect(within(editor).getByText("People and groups may be out of date.")).toBeTruthy();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(document.activeElement).toBe(box);
    expect(within(editor).getByRole("option", { name: "Bo Chen" })).toBeTruthy();
    fireEvent.click(within(editor).getByRole("button", { name: "Try again" }));
    await settle();
    expect(within(editor).queryByText("People and groups may be out of date.")).toBeNull();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(3);
  });

  it("says, while a draft lends it, that a machine which has not agreed serves nobody else yet", async () => {
    mount(studio());
    const editor = await openEditor();
    expect(within(editor).queryByText("Model sharing is waiting for Cockpit.")).toBeNull();
    await setSharing(editor, true);
    expect(within(editor).getByText("Model sharing is waiting for Cockpit.")).toBeTruthy();
    expect(within(editor).getByText(/inference\.serve/)).toBeTruthy();
  });

  it("does not warn about a machine that has already agreed", async () => {
    mount(studio(undefined, willing));
    const editor = await openEditor();
    await setSharing(editor, true);
    expect(within(editor).queryByText("Model sharing is waiting for Cockpit.")).toBeNull();
  });

  it("searches under Specific people and groups, turns a pick into a removable chip, and saves exactly the draft", async () => {
    const { conn } = mount(studio());
    const editor = await openEditor();
    expect(canSave(editor)).toBe(false);
    expect(within(editor).queryByRole("combobox")).toBeNull();

    await setSharing(editor, true);
    expect(canSave(editor)).toBe(false);
    expect(within(editor).getByText("Choose at least one person or group.")).toBeTruthy();

    const box = search(editor);
    fireEvent.change(box, { target: { value: "des" } });
    const option = within(editor).getByRole("option", { name: /Design/ });
    expect(option.textContent).toContain("Group of 4 people");
    expect(within(editor).queryByRole("option", { name: /Ana Ruiz/ })).toBeNull();
    fireEvent.click(option);
    expect(within(editor).getByRole("button", { name: "Remove Design" })).toBeTruthy();
    expect(box.value).toBe("");

    fireEvent.change(box, { target: { value: "bo" } });
    expect(box.getAttribute("aria-activedescendant")).toBeTruthy();
    fireEvent.keyDown(box, { key: "Enter" });
    expect(within(editor).getByRole("button", { name: "Remove Bo Chen" })).toBeTruthy();

    const left = within(editor).getAllByRole("option");
    expect(left.map((o) => o.textContent)).toEqual(["OpsGroup of 1 person", "Ana Ruiz", "Cy Diaz"]);

    fireEvent.keyDown(box, { key: "ArrowDown" });
    fireEvent.keyDown(box, { key: "ArrowDown" });
    const active = document.getElementById(box.getAttribute("aria-activedescendant") ?? "");
    expect(active).toBe(left[1]);
    fireEvent.keyDown(box, { key: "Enter" });
    const removeAna = within(editor).getByRole("button", { name: "Remove Ana Ruiz" });

    removeAna.focus();
    fireEvent.click(removeAna);
    expect(within(editor).queryByRole("button", { name: "Remove Ana Ruiz" })).toBeNull();
    expect(editor.contains(document.activeElement)).toBe(true);
    expect(document.activeElement).toBe(within(editor).getByRole("button", { name: "Remove Bo Chen" }));

    expect(canSave(editor)).toBe(true);
    expect(within(editor).getByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(saveButton(editor));
    await settle();

    expect(conn.query.fleetSetSharing).toHaveBeenCalledTimes(1);
    const sent = conn.query.fleetSetSharing.mock.calls[0]?.[0];
    expect(sent).toEqual({ registrationId: MACHINE_ID, mode: "people", userIds: ["bo"], groupIds: ["design"] });
    expect(buildFleetSetSharing(sent)).toBe(
      'builtin fleetSetSharing(registrationId: "v1:worker:registration:studio", mode: "people", userIds: ["bo"], groupIds: ["design"])',
    );
  });

  it("sends empty lists under Only me, while keeping the people draft on a detour", async () => {
    const { conn } = mount(
      studio({ mode: "people", userIds: ["ana"] }),
      { directory: directory({ current: current([{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }]) }) },
    );
    await settle();
    const editor = await openEditor();
    await setSharing(editor, false);
    await setSharing(editor, true);
    expect(within(editor).getByRole("button", { name: "Remove Ana Ruiz" })).toBeTruthy();
    expect(canSave(editor)).toBe(false);
    await setSharing(editor, false);
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "owner",
      userIds: [],
      groupIds: [],
    });
  });

  it("keeps a subject that left the directory, marked and removable, and sends it back untouched", async () => {
    const { conn } = mount(studio({ mode: "people", userIds: ["ana", "gone"], groupIds: ["design"] }), {
      directory: directory({
        people: [BO, CY],
        current: current(
          [
            { id: "ana", name: "Ana Ruiz", known: true, inDirectory: false },
            { id: "gone", name: "", known: false, inDirectory: false },
          ],
          [{ id: "design", name: "Design", known: true, inDirectory: true }],
        ),
      }),
    });
    await settle();
    const editor = await openEditor();
    const chips = within(editor).getByRole("list", { name: "Chosen people and groups" });
    const ana = within(chips).getByText("Ana Ruiz").closest("[role='listitem']") as HTMLElement;
    expect(within(ana).getByText("No longer available to pick")).toBeTruthy();
    const gone = within(chips).getByText("Unknown person").closest("[role='listitem']") as HTMLElement;
    expect(within(gone).getByText("No longer available to pick")).toBeTruthy();
    expect(within(editor).getByRole("button", { name: "Remove Ana Ruiz" })).toBeTruthy();
    expect(within(editor).getByRole("button", { name: "Remove Unknown person" })).toBeTruthy();
    fireEvent.change(search(editor), { target: { value: "ana" } });
    expect(within(editor).queryByRole("option", { name: /Ana Ruiz/ })).toBeNull();

    fireEvent.change(search(editor), { target: { value: "cy" } });
    fireEvent.keyDown(search(editor), { key: "Enter" });
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "people",
      userIds: ["ana", "gone", "cy"],
      groupIds: ["design"],
    });
  });

  it("names a deleted group as unknown", async () => {
    mount(studio({ mode: "people", groupIds: ["old"] }), {
      directory: directory({ current: current([], [{ id: "old", name: "", known: false, inDirectory: false }]) }),
    });
    await settle();
    const editor = await openEditor();
    expect(within(editor).getByRole("button", { name: "Remove Unknown group" })).toBeTruthy();
  });

  it("says why a non-admin has nobody to pick, and how to get somebody", async () => {
    mount(studio(), { directory: shareDirectoryRow({ id: MACHINE_ID }) });
    const editor = await openEditor();
    await setSharing(editor, true);
    expect(
      within(editor).getByText("Nobody shares a group with you yet. Ask an admin to add you to a group."),
    ).toBeTruthy();
  });

  it("shows an admin the email they already see in Users, and searches it", async () => {
    mount(studio(), {
      directory: directory({ everyone: true, people: [{ id: "ana", name: "Ana Ruiz", detail: "ana@example.com" }] }),
    });
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.change(search(editor), { target: { value: "example.com" } });
    const option = within(editor).getByRole("option", { name: /Ana Ruiz/ });
    expect(option.textContent).toContain("ana@example.com");
  });

  it("says so when a read fails, rather than showing nobody, and reads again on request", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    conn.query.fleetShareDirectory.mockRejectedValueOnce(new Error("stream closed"));
    mount(studio(), { conn });
    const editor = await openEditor();
    await setSharing(editor, true);
    expect(within(editor).getByText("Names could not be loaded.")).toBeTruthy();
    expect(within(editor).getByText("stream closed")).toBeTruthy();
    expect(within(editor).queryByText(/Nobody shares a group with you/)).toBeNull();
    fireEvent.click(within(editor).getByRole("button", { name: "Try again" }));
    await settle();
    expect(within(editor).getByRole("option", { name: /Ana Ruiz/ })).toBeTruthy();
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(2);
  });

  it("keeps the editor and the draft on a refusal, in the engine's own words", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    const refusal =
      "some of the people or groups chosen are not ones you can lend this machine to; choose from the people and groups offered";
    conn.query.fleetSetSharing.mockRejectedValueOnce(new Error(refusal));
    mount(studio(), { conn });
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.change(search(editor), { target: { value: "ana" } });
    fireEvent.keyDown(search(editor), { key: "Enter" });
    fireEvent.click(saveButton(editor));
    await settle();

    expect(screen.getByRole("region", { name: "Model sharing" })).toBe(editor);
    expect(within(editor).getByText("That change was not saved.")).toBeTruthy();
    expect(within(editor).getByText(refusal)).toBeTruthy();
    expect(document.activeElement).toBe(within(editor).getByText("That change was not saved.").closest(".fleet-share-refusal"));
    expect(within(editor).getByText("Not saved.")).toBeTruthy();
    expect(within(editor).getByText("Your choices are still here.")).toBeTruthy();
    expect(within(editor).queryByText(/exactly as it was/)).toBeNull();
    expect(within(editor).getByRole("button", { name: "Remove Ana Ruiz" })).toBeTruthy();
    expect((within(editor).getByRole("switch") as HTMLInputElement).checked).toBe(true);
    expect(screen.queryByText(/^Lent to/)).toBeNull();

  });

  it("shows success only once the write has landed, and says what the engine did", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    let land: (value: unknown) => void = () => {};
    conn.query.fleetSetSharing.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          land = resolve;
        }),
    );
    const { rerender } = mount(studio(), { conn });
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.change(search(editor), { target: { value: "ana" } });
    fireEvent.keyDown(search(editor), { key: "Enter" });
    fireEvent.click(saveButton(editor));
    await settle();

    expect(screen.getByRole("region", { name: "Model sharing" })).toBe(editor);
    expect(saveButton(editor).textContent).toBe("Saving...");
    expect(canSave(editor)).toBe(false);
    expect(screen.queryByText(/^Lent to/)).toBeNull();

    await act(async () => {
      land(
        builtinReply("fleetSetSharing", [
          { id: MACHINE_ID, machineId: MACHINE_ID, mode: "people", people: 1, groups: 0, sentence: "Lent to 1 person." },
        ]),
      );
    });
    await settle();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText("Lent to 1 person.")).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("switch", { name: "Share models" }));

    // The receipt may beat the registration broadcast. A heartbeat with the
    // old audience must not turn the acknowledged save back into a dirty edit.
    rerender(studio(undefined, { lastSeenAt: new Date().toISOString() }));
    await settle();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(within(editor).queryByRole("button", { name: "Save" })).toBeNull();
    await setSharing(editor, false);
    await setSharing(editor, true);
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(within(editor).queryByRole("button", { name: "Save" })).toBeNull();
    const reads = conn.query.fleetShareDirectory.mock.calls.length;
    rerender(studio({ mode: "people", userIds: ["ana"] }));
    await settle();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(screen.getByText("Lent to 1 person.")).toBeTruthy();
    expect(conn.query.fleetShareDirectory.mock.calls.length).toBe(reads);
    // Another session selects someone else, with the same count. The old
    // receipt no longer describes that audience and must disappear.
    rerender(studio({ mode: "people", userIds: ["bo"] }));
    await settle();
    expect(chipTexts(editor)).toEqual(["Bo Chen"]);
    expect(screen.queryByText("Lent to 1 person.")).toBeNull();
  });

  it("Escape clears search without discarding the inline draft", async () => {
    const { conn } = mount(studio());
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    const box = search(editor);
    box.focus();
    fireEvent.change(box, { target: { value: "bo" } });
    fireEvent.keyDown(box, { key: "Escape" });
    expect(box.value).toBe("");
    fireEvent.keyDown(box, { key: "Escape" });
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(document.activeElement).toBe(box);
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });

  it("retires the receipt when the machine's own consent changes under it", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    conn.query.fleetSetSharing.mockResolvedValueOnce(
      builtinReply("fleetSetSharing", [
        {
          id: MACHINE_ID,
          machineId: MACHINE_ID,
          mode: "people", userIds: ["ana"],
          people: 1,
          groups: 0,
          sentence:
            "Offered to everyone on this cluster, and to the cluster's own work. The machine itself has to agree too -- set `inference.serve: cluster` in its policy.yaml -- and until it does, nothing runs on it for anybody else.",
        },
      ]),
    );
    const { rerender } = mount(studio(), { conn });
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    fireEvent.click(saveButton(editor));
    await settle();
    rerender(studio({ mode: "people", userIds: ["ana"] }));
    expect(screen.getByText(/The machine itself has to agree too/)).toBeTruthy();
    rerender(studio({ mode: "people", userIds: ["ana"] }, willing));
    expect(screen.queryByText(/The machine itself has to agree too/)).toBeNull();
  });

  it("keeps focus in the search after the last one is picked", async () => {
    mount(studio(), { directory: directory({ people: [ANA], groups: [] }) });
    const editor = await openEditor();
    await setSharing(editor, true);
    const box = search(editor);
    box.focus();
    fireEvent.change(box, { target: { value: "ana" } });
    fireEvent.keyDown(box, { key: "Enter" });
    await settle();
    expect(document.activeElement).toBe(search(editor));
    expect(within(editor).getByText("Everyone you can choose is already on the list.")).toBeTruthy();
  });

  it("does not let Escape or Discard changes walk away from a save in flight", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    let fail: (err: unknown) => void = () => {};
    conn.query.fleetSetSharing.mockImplementationOnce(
      () =>
        new Promise((_, reject) => {
          fail = reject;
        }),
    );
    mount(studio(), { conn });
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    fireEvent.click(saveButton(editor));
    await settle();
    fireEvent.keyDown(editor, { key: "Escape" });
    fireEvent.click(within(editor).getByRole("button", { name: "Discard changes" }));
    await settle();
    expect(screen.getByRole("region", { name: "Model sharing" })).toBe(editor);
    await act(async () => {
      fail(new Error("stream closed"));
    });
    await settle();
    expect(within(editor).getByText("That change was not saved.")).toBeTruthy();
  });

  it("keeps the editor usable under StrictMode", async () => {
    h.connection = fakeConnection({ fleetShareDirectory: [directory()] });
    render(<StrictMode>{withSession(<Panel row={studio()} ledger={null} />, { userId: OWNER })}</StrictMode>);
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("discards on Discard changes too, with nothing written", async () => {
    const { conn } = mount(studio());
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    fireEvent.click(within(editor).getByRole("button", { name: "Discard changes" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();

  });
});

describe("a share changed somewhere else while the editor is open", () => {
  it("says so before Save can write over it, keeps the draft, and replaces the new share only when asked", async () => {
    const { conn, rerender } = mount(studio());
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    saveButton(editor).focus();

    rerender(studio({ mode: "people", userIds: ["bo"] }));
    await settle();

    expect(within(editor).getByText(MOVED)).toBeTruthy();
    expect(within(editor).getByText("Shared with Bo Chen")).toBeTruthy();
    expect((within(editor).getByRole("switch") as HTMLInputElement).checked).toBe(true);
    expect(canSave(editor)).toBe(false);
    expect(within(editor).getByText("Choose which sharing to keep.")).toBeTruthy();
    expect(document.activeElement).toBe(within(editor).getByRole("group", { name: MOVED }));
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();

    fireEvent.click(within(editor).getByRole("button", { name: "Keep my changes" }));
    await settle();
    expect(within(editor).queryByText(MOVED)).toBeNull();
    expect(document.activeElement).toBe(saveButton(editor));
    expect(within(editor).getByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing).toHaveBeenCalledTimes(1);
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "people",
      userIds: ["ana"],
      groupIds: [],
    });
  });

  it("takes the new share on request, and drops nothing but the draft it replaces", async () => {
    const { conn, rerender } = mount(studio());
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    rerender(studio({ mode: "people", userIds: ["bo"] }));
    await settle();

    fireEvent.click(within(editor).getByRole("button", { name: "Use the new sharing" }));
    await settle();
    expect(within(editor).queryByText(MOVED)).toBeNull();
    expect((within(editor).getByRole("switch") as HTMLInputElement).checked).toBe(true);
    expect(chipTexts(editor)).toEqual(["Bo Chen"]);
    expect(within(editor).queryByText("Unsaved changes")).toBeNull();
    expect(canSave(editor)).toBe(false);
    expect(document.activeElement).toBe(within(editor).getByRole("switch"));
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });

  it("lets a draft nobody has touched follow the new share, and says so", async () => {
    const { rerender } = mount(studio());
    const editor = await openEditor();
    rerender(studio({ mode: "cluster" }));
    await settle();

    expect((within(editor).getByRole("switch") as HTMLInputElement).checked).toBe(true);
    expect(within(editor).getByText(MOVED)).toBeTruthy();
    expect(within(editor).getByText("This shows the new sharing.")).toBeTruthy();
    expect(within(editor).queryByText("Unsaved changes")).toBeNull();
    expect(canSave(editor)).toBe(false);
    expect(document.activeElement).toBe(within(editor).getByRole("switch"));

    await setSharing(editor, false);
    expect(within(editor).queryByText("This shows the new sharing.")).toBeNull();
    expect(within(editor).getByText("Unsaved changes")).toBeTruthy();
  });

  it("leaves focus where the person put it when a follow does not touch it", async () => {
    const stored = { mode: "people", userIds: ["ana"] };
    const { rerender } = mount(studio(stored), {
      directory: directory({ current: current([{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }]) }),
    });
    await settle();
    const editor = await openEditor();
    const box = search(editor);
    box.focus();
    rerender(studio({ ...stored, userIds: ["ana", "bo"] }));
    await settle();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz", "Bo Chen"]);
    expect(within(editor).getByText("This shows the new sharing.")).toBeTruthy();
    expect(document.activeElement).toBe(box);
  });

  it("asks nothing when the draft already says what is now stored", async () => {
    const { rerender } = mount(studio());
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    rerender(studio({ mode: "people", userIds: ["ana"] }));
    await settle();
    expect(within(editor).queryByText(MOVED)).toBeNull();
    expect(within(editor).queryByText("Unsaved changes")).toBeNull();
    await setSharing(editor, false);
    expect(within(editor).queryByText(MOVED)).toBeNull();
    expect(canSave(editor)).toBe(true);
  });

  it("keeps an inactive tab's draft without moving focus when a broadcast conflicts", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    h.connection = conn;
    const node = (row: Row, hidden: boolean) => withSession(<>
      <button type="button">Other tab action</button>
      <div hidden={hidden}><Panel row={row} ledger={null} /></div>
    </>, { userId: OWNER });
    const view = render(node(studio(), false));
    const editor = await openEditor();
    await setSharing(editor, true);
    fireEvent.click(within(editor).getByRole("option", { name: "Ana Ruiz" }));
    view.rerender(node(studio(), true));
    const outside = screen.getByRole("button", { name: "Other tab action" });
    outside.focus();
    view.rerender(node(studio({ mode: "people", userIds: ["bo"] }), true));
    await settle();
    expect(document.activeElement).toBe(outside);
    view.rerender(node(studio({ mode: "people", userIds: ["bo"] }), false));
    expect(chipTexts(editor)).toEqual(["Ana Ruiz"]);
    expect(within(editor).getByText(MOVED)).toBeTruthy();
    expect(canSave(editor)).toBe(false);
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });

  it("does not count a heartbeat, or an id respelled bare, as a change made elsewhere", async () => {
    const stored = { mode: "people", userIds: ["v1:identity:user:ana"], groupIds: ["design"] };
    const { rerender } = mount(studio(stored), {
      directory: directory({
        current: current(
          [{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }],
          [{ id: "design", name: "Design", known: true, inDirectory: true }],
        ),
      }),
    });
    await settle();
    const editor = await openEditor();
    fireEvent.change(search(editor), { target: { value: "bo" } });
    fireEvent.keyDown(search(editor), { key: "Enter" });
    rerender(
      studio({ ...stored, userIds: ["ana"] }, { lastSeenAt: new Date(Date.now() + 15_000).toISOString() }),
    );
    await settle();
    expect(within(editor).queryByText(MOVED)).toBeNull();
    expect(canSave(editor)).toBe(true);
    expect(chipTexts(editor)).toEqual(["Ana Ruiz", "Design", "Bo Chen"]);
  });
});

describe("a share directory that could not be read", () => {
  it("keeps who the machine is lent to on the list, removable, while names load and after they fail", async () => {
    let fail: (err: unknown) => void = () => {};
    const pending = new Promise<never>((_, reject) => {
      fail = reject;
    });
    const conn = fakeConnection();
    conn.query.fleetShareDirectory.mockImplementation(() => pending);
    mount(studio({ mode: "people", userIds: ["ana", "bo"], groupIds: ["design"] }), { conn });
    const editor = await openEditor();

    expect(editor.querySelectorAll(".fleet-share-chip")).toHaveLength(3);
    expect(within(editor).queryByRole("button", { name: /^Remove / })).toBeNull();
    expect(within(editor).queryByText(/Unknown/)).toBeNull();

    await act(async () => {
      fail(new Error("stream closed"));
    });
    await settle();

    expect(chipTexts(editor)).toEqual(["Person 1", "Person 2", "Group 1"]);
    expect(within(editor).getByText("Names could not be loaded.")).toBeTruthy();
    expect(within(editor).getByText("stream closed")).toBeTruthy();
    expect(within(editor).getByRole("button", { name: "Try again" })).toBeTruthy();
    expect(within(editor).queryByRole("combobox")).toBeNull();

    fireEvent.click(within(editor).getByRole("button", { name: "Remove Person 1" }));
    expect(chipTexts(editor)).toEqual(["Person 2", "Group 1"]);
    expect(document.activeElement).toBe(within(editor).getByRole("button", { name: "Remove Person 2" }));
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "people",
      userIds: ["bo"],
      groupIds: ["design"],
    });
  });

  it("names the list once a second read lands", async () => {
    const conn = fakeConnection();
    conn.query.fleetShareDirectory.mockRejectedValue(new Error("stream closed"));
    mount(studio({ mode: "people", userIds: ["ana"], groupIds: ["design"] }), { conn });
    const editor = await openEditor();
    expect(chipTexts(editor)).toEqual(["Person 1", "Group 1"]);

    conn.query.fleetShareDirectory.mockImplementation(async () =>
      builtinReply("fleetShareDirectory", [
        directory({
          current: current(
            [{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }],
            [{ id: "design", name: "Design", known: true, inDirectory: true }],
          ),
        }),
      ]),
    );
    fireEvent.click(within(editor).getByRole("button", { name: "Try again" }));
    await settle();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz", "Design"]);
    expect(within(editor).queryByText("Names could not be loaded.")).toBeNull();
    expect(search(editor)).toBeTruthy();
  });

  it("still stops lending to anyone with Only me", async () => {
    const conn = fakeConnection();
    conn.query.fleetShareDirectory.mockRejectedValue(new Error("stream closed"));
    mount(studio({ mode: "people", userIds: ["ana"], groupIds: ["design"] }), { conn });
    const editor = await openEditor();
    await setSharing(editor, false);
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "owner",
      userIds: [],
      groupIds: [],
    });
  });
});

describe("a share stored with canonical ids", () => {
  it("matches the directory's bare ids in the editor, and sends each id back spelled as stored", async () => {
    const { conn } = mount(
      studio({ mode: "people", userIds: ["v1:identity:user:ana"], groupIds: ["v1:identity:group:design"] }),
      {
        directory: directory({
          current: current(
            [{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }],
            [{ id: "design", name: "Design", known: true, inDirectory: true }],
          ),
        }),
      },
    );
    await settle();
    const editor = await openEditor();
    expect(chipTexts(editor)).toEqual(["Ana Ruiz", "Design"]);
    expect(within(editor).queryByText("No longer available to pick")).toBeNull();
    expect(within(editor).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "OpsGroup of 1 person",
      "Bo Chen",
      "Cy Diaz",
    ]);
    expect(canSave(editor)).toBe(false);

    fireEvent.change(search(editor), { target: { value: "bo" } });
    fireEvent.keyDown(search(editor), { key: "Enter" });
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "people",
      userIds: ["v1:identity:user:ana", "bo"],
      groupIds: ["v1:identity:group:design"],
    });
  });

  it("keeps a canonical id's spelling when the names could not be read", async () => {
    const conn = fakeConnection();
    conn.query.fleetShareDirectory.mockRejectedValue(new Error("stream closed"));
    mount(studio({ mode: "people", userIds: ["v1:identity:user:ana", "bo"] }), { conn });
    const editor = await openEditor();
    fireEvent.click(within(editor).getByRole("button", { name: "Remove Person 2" }));
    fireEvent.click(saveButton(editor));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "people",
      userIds: ["v1:identity:user:ana"],
      groupIds: [],
    });
  });
});

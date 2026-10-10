import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
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

// Sharing a machine with people and groups (epic memql#5344, design sections 5
// and 6 and rulings G2, G10 and G15; plan Task 8).
//
// ===========================================================================
// WHAT THIS FILE HOLDS THE SURFACE TO
// ===========================================================================
// Three promises, each made to somebody deciding whether to lend their
// machine:
//
//   - WHAT IS TRUE IS SAID, AND NOTHING ELSE. Two consents, both always on
//     screen with their own repair; the week's usage as the engine's own
//     sentence; a subject that left the directory still named on the list it
//     is on. A read that failed is never shown as an empty answer.
//   - A PROPOSAL IS NOT A RESULT. The dialog is a draft until the engine
//     answers; success is the engine's receipt, shown only after the write
//     landed, and a refusal keeps every choice the person made.
//   - THE KEYBOARD REACHES EVERYTHING, and Escape takes the person back to the
//     control they came from with nothing written.
//
// Every builtin answers in its WIRE shape (the harness's builtinReply), so a
// surface that read a builtin's reply as flat rows fails here rather than in
// front of somebody.

afterEach(cleanup);

// jsdom implements <dialog> as an element with no behaviour: no showModal, no
// close. These stand in for the ATTRIBUTE half of the platform's and nothing
// more -- focus containment, the top layer and Escape's close request are the
// browser's, and the dialog asks nothing else of them.
const dialogProto = HTMLDialogElement.prototype as unknown as {
  showModal?: () => void;
  close?: () => void;
};
beforeAll(() => {
  dialogProto.showModal = function showModal(this: HTMLDialogElement) {
    this.setAttribute("open", "");
  };
  dialogProto.close = function close(this: HTMLDialogElement) {
    this.removeAttribute("open");
  };
});
afterAll(() => {
  delete dialogProto.showModal;
  delete dialogProto.close;
});

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
    // BARE, as the engine sends a row's ids on egress, against a session that
    // carries the canonical spelling: the two must still be one person.
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
  // The REAL writes hook: the call the dialog makes, the busy state and the
  // refusal all run through the path production uses.
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

/** Open the dialog the way a keyboard does: focus the act, then press it. */
async function openDialog(): Promise<HTMLElement> {
  const opener = screen.getByRole("button", { name: "Change sharing" });
  opener.focus();
  fireEvent.click(opener);
  const dialog = await screen.findByRole("dialog");
  await settle();
  return dialog;
}

function saveButton(dialog: HTMLElement): HTMLButtonElement {
  return within(dialog).getByRole("button", { name: /^(Save|Saving\.\.\.)$/ }) as HTMLButtonElement;
}

function search(dialog: HTMLElement): HTMLInputElement {
  return within(dialog).getByRole("combobox", { name: "Search people and groups" }) as HTMLInputElement;
}

function radio(dialog: HTMLElement, name: RegExp): HTMLElement {
  return within(dialog).getByRole("radio", { name });
}

/** The chips the dialog holds, as a person reads them. */
function chipTexts(dialog: HTMLElement): string[] {
  const list = within(dialog).getByRole("list", { name: "Chosen people and groups" });
  return within(list)
    .getAllByRole("listitem")
    .map((chip) => chip.textContent ?? "");
}

const MOVED = "Sharing changed somewhere else while this was open.";

// ---------------------------------------------------------------------------
// The panel
// ---------------------------------------------------------------------------

describe("the sharing panel", () => {
  it("says who can use a private machine, with both consents, and no repair for a share nobody asked for", () => {
    mount(studio());
    expect(screen.getByText("Only you")).toBeTruthy();
    // TWO LINES, NOT ONE DERIVED FLAG: the owner's repair is an act on this
    // page and the cockpit's is a line in a file on that machine's disk, and a
    // single "not shared" sends half the people to the wrong place.
    expect(screen.getByText("You have not shared it.")).toBeTruthy();
    // BUT A REPAIR IS FOR A DECISION SOMEBODY MADE. A private machine's
    // cockpit line states its position; telling the owner to edit
    // policy.yaml for a share they have not chosen is noise -- the repair
    // appears the moment a share does (below, and in the dialog).
    expect(screen.getByText("Its cockpit serves only you.")).toBeTruthy();
    expect(screen.queryByText(/policy\.yaml/)).toBeNull();
  });

  it("names up to two people and groups it is lent to", async () => {
    const { conn } = mount(studio({ mode: "people", userIds: ["ana"], groupIds: ["design"], sharedAt: "2026-09-20T10:00:00Z" }, willing), {
      directory: directory({
        current: current(
          [{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }],
          [{ id: "design", name: "Design", known: true, inDirectory: true }],
        ),
      }),
    });
    expect(await screen.findByText("Shared with Ana Ruiz and Design")).toBeTruthy();
    // Names are asked for once, of the directory the owner already sees.
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(1);
    expect(screen.getByText(/^You shared it on /)).toBeTruthy();
    expect(screen.getByText("Its cockpit has agreed to serve anyone you share it with.")).toBeTruthy();
  });

  it("names two and counts the rest", async () => {
    mount(studio({ mode: "people", userIds: ["ana", "bo"], groupIds: ["design", "ops"] }), {
      directory: directory({
        current: current(
          [
            { id: "ana", name: "Ana Ruiz", known: true, inDirectory: true },
            { id: "bo", name: "Bo Chen", known: true, inDirectory: true },
          ],
          [
            { id: "design", name: "Design", known: true, inDirectory: true },
            { id: "ops", name: "Ops", known: true, inDirectory: true },
          ],
        ),
      }),
    });
    expect(await screen.findByText("Shared with Ana Ruiz, Bo Chen and 2 more")).toBeTruthy();
  });

  it("counts rather than guesses when the names cannot be read", async () => {
    // A failed read is not an empty list, and it is not a licence to show an
    // id either: the count is the true sentence the panel still has.
    const conn = fakeConnection();
    conn.query.fleetShareDirectory.mockRejectedValue(new Error("stream closed"));
    mount(studio({ mode: "people", userIds: ["ana", "bo"], groupIds: ["design"] }), { conn });
    await settle();
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Shared with 2 people and 1 group")).toBeTruthy();
  });

  it("says everyone for a machine shared with the whole cluster", () => {
    mount(studio({ mode: "cluster" }));
    expect(screen.getByText("Everyone in this cluster")).toBeTruthy();
    expect(screen.getByText(/^You shared it/)).toBeTruthy();
    expect(screen.getByText(/Its cockpit has not agreed to serve anyone but you/)).toBeTruthy();
  });

  it("offers the act to the owner and to nobody else", () => {
    // ABSENT rather than disabled (DESIGN.md rule 12), and the engine refuses a
    // machine that is not the caller's anyway. The directory is never asked
    // about somebody else's machine: it would refuse, and the answer would be
    // a count either way.
    const { conn } = mount(studio({ mode: "people", userIds: ["ana"] }), {
      userId: "v1:identity:user:someone-else",
    });
    expect(screen.queryByRole("button", { name: "Change sharing" })).toBeNull();
    expect(screen.getByText("Only this machine's owner can change who it is shared with.")).toBeTruthy();
    expect(screen.getByText("Shared with 1 person")).toBeTruthy();
    // The repair is the OWNER'S to make: a file on their machine's disk. A
    // non-owner is told the state and not handed an instruction they cannot
    // carry out.
    expect(screen.getByText("Its cockpit has not agreed to serve anyone but its owner.")).toBeTruthy();
    expect(screen.queryByText(/policy\.yaml/)).toBeNull();
    expect(conn.query.fleetShareDirectory).not.toHaveBeenCalled();
  });

  it("offers nothing to change on a machine that has been removed", () => {
    mount(studio({ mode: "cluster" }, { revokedAt: "2026-09-01T00:00:00Z" }));
    expect(screen.queryByRole("button", { name: "Change sharing" })).toBeNull();
    expect(screen.getByText("This machine was removed, so it cannot be shared.")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The way in, from a machine's Equipment view
// ---------------------------------------------------------------------------

describe("the words on the way into Sharing", () => {
  const words = (block: unknown, serve?: string) =>
    sharingLinkWords(
      machineFromRow(studio(block, serve === undefined ? {} : { capabilityDescriptor: { inferenceServe: serve } })),
    );

  it("say what the machine DOES: the owner's mode and the cockpit's consent together", () => {
    expect(words(undefined, "cluster")).toBe("Personal inference");
    expect(words({ mode: "people", userIds: ["ana"] }, "cluster")).toBe("Shared with people");
    expect(words({ mode: "cluster" }, "cluster")).toBe("Shared with everyone");
    // Lent, but the cockpit has not agreed: nobody else's call can land on it
    // yet, so it is still personal -- the Sharing view says which half is missing.
    expect(words({ mode: "people", userIds: ["ana"] })).toBe("Personal inference");
    expect(words({ mode: "cluster" }, "owner")).toBe("Personal inference");
  });
});

// ---------------------------------------------------------------------------
// The ledger
// ---------------------------------------------------------------------------

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
    // Owner-only, and shared by the owner but refused by the cockpit: in both
    // nobody else's call can land on it, so a count would describe nothing.
    const first = mount(studio(undefined, willing), { ledger: { sentence: WEEK, readable: true } });
    expect(screen.queryByText(WEEK)).toBeNull();
    first.rerender(studio({ mode: "people", userIds: ["ana"] }));
    expect(screen.queryByText(WEEK)).toBeNull();
  });

  it("does not let a ledger that could not be read look like a count", () => {
    // The engine answers `readable: false` with a sentence of its own rather
    // than zero. A quiet caption saying nothing ran and one saying nobody
    // looked read alike, and only one of them is a measurement.
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
    // Through the machine page and the real read, in the builtin wire shape --
    // the one path a flat-row fake could never have caught.
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

// ---------------------------------------------------------------------------
// The dialog
// ---------------------------------------------------------------------------

describe("the share dialog", () => {
  it("reads the directory once per opening, and nothing before it opens", async () => {
    const { conn } = mount(studio());
    expect(conn.query.fleetShareDirectory).not.toHaveBeenCalled();
    const dialog = await openDialog();
    expect(within(dialog).getByText("Change sharing")).toBeTruthy();
    expect(within(dialog).getByText(LABEL)).toBeTruthy();
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(1);
    expect(conn.query.fleetShareDirectory.mock.calls[0]?.[0]).toEqual({ registrationId: MACHINE_ID });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await openDialog();
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(2);
  });

  it("offers the three choices in the owner's words, and the terms of the offer", async () => {
    mount(studio());
    const dialog = await openDialog();
    const choices = within(dialog).getAllByRole("radio");
    expect(choices.map((c) => c.textContent)).toEqual([
      "Only meNobody else's work runs on it.",
      "Specific people and groupsThe people you choose, and the members of the groups you choose.",
      "Everyone in this clusterAnyone signed in to this cluster, and the cluster's own automations.",
    ]);
    expect(choices[0]?.getAttribute("aria-checked")).toBe("true");
    expect(within(dialog).getByText("Changes take effect on the next call. A call already running finishes.")).toBeTruthy();
    fireEvent.click(choices[2]!);
    expect(
      within(dialog).getByText(
        "You see how many calls ran and for how many people. You never see what anybody asked or what the model answered.",
      ),
    ).toBeTruthy();
  });

  it("says, while a draft lends it, that a machine which has not agreed serves nobody else yet", async () => {
    // THE CONSEQUENCE AT THE MOMENT OF DECIDING (SUPERVISED-VISUAL-COMPOSITION:
    // "explicit consequences"). Saving a share on a machine whose policy.yaml
    // still says owner changes nothing anybody can use, and the owner should
    // learn that before pressing Save, not from a receipt after it.
    mount(studio());
    const dialog = await openDialog();
    expect(within(dialog).queryByText("This machine has not agreed to serve anyone else yet.")).toBeNull();
    fireEvent.click(within(dialog).getAllByRole("radio")[2]!);
    expect(within(dialog).getByText("This machine has not agreed to serve anyone else yet.")).toBeTruthy();
    expect(within(dialog).getByText(/inference\.serve/)).toBeTruthy();
  });

  it("does not warn about a machine that has already agreed", async () => {
    mount(studio(undefined, willing));
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getAllByRole("radio")[2]!);
    expect(within(dialog).queryByText("This machine has not agreed to serve anyone else yet.")).toBeNull();
  });

  it("searches under Specific people and groups, turns a pick into a removable chip, and saves exactly the draft", async () => {
    const { conn } = mount(studio());
    const dialog = await openDialog();
    const save = saveButton(dialog);
    // Nothing differs from what is stored yet.
    expect(save.disabled).toBe(true);
    expect(within(dialog).queryByRole("combobox")).toBeNull();

    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    // Choosing the mode is not a share: it names nobody yet.
    expect(save.disabled).toBe(true);
    expect(within(dialog).getByText("Choose at least one person or group.")).toBeTruthy();

    // People and groups together, a group marked with its member count.
    const box = search(dialog);
    fireEvent.change(box, { target: { value: "des" } });
    const option = within(dialog).getByRole("option", { name: /Design/ });
    expect(option.textContent).toContain("Group of 4 people");
    expect(within(dialog).queryByRole("option", { name: /Ana Ruiz/ })).toBeNull();
    fireEvent.click(option);
    expect(within(dialog).getByRole("button", { name: "Remove Design" })).toBeTruthy();
    // A pick clears the search, so the next name starts from the whole list.
    expect(box.value).toBe("");

    // The keyboard's way: type, the first match is the active one, Enter adds it.
    fireEvent.change(box, { target: { value: "bo" } });
    expect(box.getAttribute("aria-activedescendant")).toBeTruthy();
    fireEvent.keyDown(box, { key: "Enter" });
    expect(within(dialog).getByRole("button", { name: "Remove Bo Chen" })).toBeTruthy();

    // What is left to pick, GROUPS FIRST and each kind by name: a group is the
    // unit most shares are about, and the way to lend a machine to more.
    const left = within(dialog).getAllByRole("option");
    expect(left.map((o) => o.textContent)).toEqual(["OpsGroup of 1 person", "Ana Ruiz", "Cy Diaz"]);

    // Arrow keys move the active option; Enter takes the one they landed on.
    fireEvent.keyDown(box, { key: "ArrowDown" });
    fireEvent.keyDown(box, { key: "ArrowDown" });
    const active = document.getElementById(box.getAttribute("aria-activedescendant") ?? "");
    expect(active).toBe(left[1]);
    fireEvent.keyDown(box, { key: "Enter" });
    const removeAna = within(dialog).getByRole("button", { name: "Remove Ana Ruiz" });

    // Chips are real buttons, so the keyboard removes them; focus moves to a
    // neighbour rather than falling to the page.
    removeAna.focus();
    fireEvent.click(removeAna);
    expect(within(dialog).queryByRole("button", { name: "Remove Ana Ruiz" })).toBeNull();
    expect(dialog.contains(document.activeElement)).toBe(true);
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Remove Bo Chen" }));

    expect(save.disabled).toBe(false);
    expect(within(dialog).getByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(save);
    await settle();

    expect(conn.query.fleetSetSharing).toHaveBeenCalledTimes(1);
    const sent = conn.query.fleetSetSharing.mock.calls[0]?.[0];
    expect(sent).toEqual({ registrationId: MACHINE_ID, mode: "people", userIds: ["bo"], groupIds: ["design"] });
    // And what reaches the wire is MemQL the engine parses, from the real
    // generated builder -- a fake that only recorded arguments never ran it.
    expect(buildFleetSetSharing(sent)).toBe(
      'builtin fleetSetSharing(registrationId: "v1:worker:registration:studio", mode: "people", userIds: ["bo"], groupIds: ["design"])',
    );
  });

  it("sends empty lists under Only me and Everyone, whatever the draft held", async () => {
    // G10: a list saved under another mode is residue a later reader could
    // honour by mistake. The draft keeps the chips while the dialog is open;
    // the write does not carry them.
    const { conn } = mount(
      studio({ mode: "people", userIds: ["ana"] }),
      { directory: directory({ current: current([{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }]) }) },
    );
    await settle();
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Everyone in this cluster/ }));
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    // Back where it started: the chips survived the detour, and nothing differs.
    expect(within(dialog).getByRole("button", { name: "Remove Ana Ruiz" })).toBeTruthy();
    expect(saveButton(dialog).disabled).toBe(true);
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Everyone in this cluster/ }));
    fireEvent.click(saveButton(dialog));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "cluster",
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
    const dialog = await openDialog();
    const chips = within(dialog).getByRole("list", { name: "Chosen people and groups" });
    const ana = within(chips).getByText("Ana Ruiz").closest("[role='listitem']") as HTMLElement;
    expect(within(ana).getByText("No longer available to pick")).toBeTruthy();
    const gone = within(chips).getByText("Unknown person").closest("[role='listitem']") as HTMLElement;
    expect(within(gone).getByText("No longer available to pick")).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Remove Ana Ruiz" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Remove Unknown person" })).toBeTruthy();
    // Not offered again: somebody who left the owner's groups cannot be picked.
    fireEvent.change(search(dialog), { target: { value: "ana" } });
    expect(within(dialog).queryByRole("option", { name: /Ana Ruiz/ })).toBeNull();

    // Touch something else and save: the stale two go back as they were stored.
    fireEvent.change(search(dialog), { target: { value: "cy" } });
    fireEvent.keyDown(search(dialog), { key: "Enter" });
    fireEvent.click(saveButton(dialog));
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
    const dialog = await openDialog();
    expect(within(dialog).getByRole("button", { name: "Remove Unknown group" })).toBeTruthy();
  });

  it("says why a non-admin has nobody to pick, and how to get somebody", async () => {
    mount(studio(), { directory: shareDirectoryRow({ id: MACHINE_ID }) });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    expect(
      within(dialog).getByText("Nobody shares a group with you yet. Choose Everyone, or ask an admin to add you to a group."),
    ).toBeTruthy();
  });

  it("shows an admin the email they already see in Users, and searches it", async () => {
    mount(studio(), {
      directory: directory({ everyone: true, people: [{ id: "ana", name: "Ana Ruiz", detail: "ana@example.com" }] }),
    });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    fireEvent.change(search(dialog), { target: { value: "example.com" } });
    const option = within(dialog).getByRole("option", { name: /Ana Ruiz/ });
    expect(option.textContent).toContain("ana@example.com");
  });

  it("says so when a read fails, rather than showing nobody, and reads again on request", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    conn.query.fleetShareDirectory.mockRejectedValueOnce(new Error("stream closed"));
    mount(studio(), { conn });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    expect(within(dialog).getByText("Names could not be loaded.")).toBeTruthy();
    expect(within(dialog).getByText("stream closed")).toBeTruthy();
    expect(within(dialog).queryByText(/Nobody shares a group with you/)).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
    await settle();
    expect(within(dialog).getByRole("option", { name: /Ana Ruiz/ })).toBeTruthy();
    expect(conn.query.fleetShareDirectory).toHaveBeenCalledTimes(2);
  });

  it("keeps the dialog and the draft on a refusal, in the engine's own words", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    const refusal =
      "some of the people or groups chosen are not ones you can lend this machine to; choose from the people and groups offered";
    conn.query.fleetSetSharing.mockRejectedValueOnce(new Error(refusal));
    mount(studio(), { conn });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    fireEvent.change(search(dialog), { target: { value: "ana" } });
    fireEvent.keyDown(search(dialog), { key: "Enter" });
    fireEvent.click(saveButton(dialog));
    await settle();

    expect(screen.getByRole("dialog")).toBe(dialog);
    expect(within(dialog).getByText("That change was not saved.")).toBeTruthy();
    expect(within(dialog).getByText(refusal)).toBeTruthy();
    // THE REASON IS WHERE THE PERSON IS LOOKING. The notice is the last thing
    // in a scrolling body, below the fold at common sizes, and the busy Save
    // dropped focus to the page: so the notice takes focus (which scrolls it
    // into view) and the floor says the save did not happen.
    expect(document.activeElement).toBe(within(dialog).getByText("That change was not saved.").closest(".fleet-share-refusal"));
    expect(within(dialog).getByText("Not saved.")).toBeTruthy();
    // Honest about what it knows: a transport error may have landed, so it
    // promises only that the choices are kept; the panel's live line says
    // what is stored.
    expect(within(dialog).getByText("Your choices are still here.")).toBeTruthy();
    expect(within(dialog).queryByText(/exactly as it was/)).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Remove Ana Ruiz" })).toBeTruthy();
    expect(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }).getAttribute("aria-checked")).toBe(
      "true",
    );
    // Nothing on the panel claims a change happened.
    expect(screen.queryByText(/^Lent to/)).toBeNull();
    expect(screen.getByText("Only you")).toBeTruthy();
  });

  it("shows success only once the write has landed, then closes and says what the engine did", async () => {
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    let land: (value: unknown) => void = () => {};
    conn.query.fleetSetSharing.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          land = resolve;
        }),
    );
    const { rerender } = mount(studio(), { conn });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    fireEvent.change(search(dialog), { target: { value: "ana" } });
    fireEvent.keyDown(search(dialog), { key: "Enter" });
    fireEvent.click(saveButton(dialog));
    await settle();

    // In flight: still the draft, still the dialog, and no success anywhere.
    expect(screen.getByRole("dialog")).toBe(dialog);
    expect(saveButton(dialog).textContent).toBe("Saving...");
    expect(saveButton(dialog).disabled).toBe(true);
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
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Change sharing" }));

    // The row arrives on the subscription, and the names the dialog already
    // read are enough to say who: nothing is read again for it.
    const reads = conn.query.fleetShareDirectory.mock.calls.length;
    rerender(studio({ mode: "people", userIds: ["ana"] }));
    await settle();
    expect(screen.getByText("Shared with Ana Ruiz")).toBeTruthy();
    expect(screen.getByText("Lent to 1 person.")).toBeTruthy();
    expect(conn.query.fleetShareDirectory.mock.calls.length).toBe(reads);
  });

  it("discards the draft on Escape, writes nothing, and puts focus back on Change sharing", async () => {
    const { conn } = mount(studio());
    const opener = screen.getByRole("button", { name: "Change sharing" });
    const dialog = await openDialog();
    // Focus went INTO the dialog, onto the choice that is in force.
    expect(document.activeElement?.getAttribute("aria-checked")).toBe("true");
    expect(dialog.contains(document.activeElement)).toBe(true);

    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    const box = search(dialog);
    box.focus();
    fireEvent.change(box, { target: { value: "ana" } });
    // The first Escape clears the search and leaves the dialog where it is.
    fireEvent.keyDown(box, { key: "Escape" });
    expect(box.value).toBe("");
    expect(screen.getByRole("dialog")).toBe(dialog);
    // The second closes it.
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();

    // Reopening starts from what is stored, not from the draft thrown away.
    const again = await openDialog();
    expect(within(again).getByRole("radio", { name: /^Only me/ }).getAttribute("aria-checked")).toBe("true");
  });

  it("retires the receipt when the machine's own consent changes under it", async () => {
    // The receipt told the owner to set inference.serve; once the machine has
    // agreed, that sentence contradicts the consent line right above it.
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    conn.query.fleetSetSharing.mockResolvedValueOnce(
      builtinReply("fleetSetSharing", [
        {
          id: MACHINE_ID,
          machineId: MACHINE_ID,
          mode: "cluster",
          people: 0,
          groups: 0,
          sentence:
            "Offered to everyone on this cluster, and to the cluster's own work. The machine itself has to agree too -- set `inference.serve: cluster` in its policy.yaml -- and until it does, nothing runs on it for anybody else.",
        },
      ]),
    );
    const { rerender } = mount(studio(), { conn });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getAllByRole("radio")[2]!);
    fireEvent.click(saveButton(dialog));
    await settle();
    rerender(studio({ mode: "cluster" }));
    expect(screen.getByText(/The machine itself has to agree too/)).toBeTruthy();
    rerender(studio({ mode: "cluster" }, willing));
    expect(screen.queryByText(/The machine itself has to agree too/)).toBeNull();
  });

  it("keeps focus in the search after the last one is picked", async () => {
    // Picking the last candidate used to unmount the focused search, dropping
    // keyboard focus to the page behind a modal.
    mount(studio(), { directory: directory({ people: [ANA], groups: [] }) });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Specific people and groups/ }));
    const box = search(dialog);
    box.focus();
    fireEvent.change(box, { target: { value: "ana" } });
    fireEvent.keyDown(box, { key: "Enter" });
    await settle();
    expect(document.activeElement).toBe(search(dialog));
    expect(within(dialog).getByText("Everyone you can choose is already on the list.")).toBeTruthy();
  });

  it("does not let Escape or Cancel walk away from a save in flight", async () => {
    // Its answer is either the receipt, which closes the dialog anyway, or a
    // refusal -- and a refusal arriving after the dialog had gone would leave
    // the person believing a change that was never made.
    const conn = fakeConnection({ fleetShareDirectory: [directory()] });
    let fail: (err: unknown) => void = () => {};
    conn.query.fleetSetSharing.mockImplementationOnce(
      () =>
        new Promise((_, reject) => {
          fail = reject;
        }),
    );
    mount(studio(), { conn });
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getAllByRole("radio")[2]!);
    fireEvent.click(saveButton(dialog));
    await settle();
    fireEvent.keyDown(dialog, { key: "Escape" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await settle();
    expect(screen.getByRole("dialog")).toBe(dialog);
    await act(async () => {
      fail(new Error("stream closed"));
    });
    await settle();
    expect(within(dialog).getByText("That change was not saved.")).toBeTruthy();
  });

  it("stays open under StrictMode, whose rehearsal cleanup queues a close the platform delivers late", async () => {
    // The shell renders under StrictMode, which runs the dialog's open effect,
    // its cleanup and the effect again. The platform fires `close` from a
    // QUEUED task, so the close that rehearsal cleanup asked for arrives after
    // the dialog is open again -- and a handler that took it at its word shut
    // the dialog the moment it opened, in every dev build.
    const plainClose = dialogProto.close;
    dialogProto.close = function close(this: HTMLDialogElement) {
      this.removeAttribute("open");
      setTimeout(() => this.dispatchEvent(new Event("close")), 0);
    };
    try {
      h.connection = fakeConnection({ fleetShareDirectory: [directory()] });
      render(<StrictMode>{withSession(<Panel row={studio()} ledger={null} />, { userId: OWNER })}</StrictMode>);
      const dialog = await openDialog();
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 20));
      });
      expect(screen.getByRole("dialog")).toBe(dialog);
      expect(dialog.hasAttribute("open")).toBe(true);
    } finally {
      dialogProto.close = plainClose;
    }
  });

  it("discards on Cancel too, with nothing written", async () => {
    const { conn } = mount(studio());
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Everyone in this cluster/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
    expect(screen.getByText("Only you")).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The share moving under an open dialog (memql#5659, item 1)
// ---------------------------------------------------------------------------
//
// The draft is copied from the row once, when the dialog opens, and the row
// keeps arriving on the subscription while it is open. "Moved" is the STORED
// share differing from the draft's STARTING value -- never the draft differing
// from what is stored, which it does the moment anybody picks. Measuring the
// second made a change from another tab look like the owner's own unsaved
// edit, and Save wrote the old draft back over it.

describe("a share changed somewhere else while the dialog is open", () => {
  it("says so before Save can write over it, keeps the draft, and replaces the new share only when asked", async () => {
    const { conn, rerender } = mount(studio());
    const dialog = await openDialog();
    fireEvent.click(radio(dialog, /^Everyone in this cluster/));
    // The person's hand is on Save when the other change lands.
    saveButton(dialog).focus();

    // Another tab lends the machine to Bo while this draft is open.
    rerender(studio({ mode: "people", userIds: ["bo"] }));
    await settle();

    expect(within(dialog).getByText(MOVED)).toBeTruthy();
    // What it is NOW, in the panel's own words, so the choice is between two
    // things the person can see.
    expect(within(dialog).getByText("Shared with Bo Chen")).toBeTruthy();
    // The draft is the person's, and it stays.
    expect(radio(dialog, /^Everyone in this cluster/).getAttribute("aria-checked")).toBe("true");
    // Save cannot put the old draft back over the new share without a decision.
    expect(saveButton(dialog).disabled).toBe(true);
    expect(within(dialog).getByText("Choose which sharing to keep.")).toBeTruthy();
    // The disabled Save lost the keyboard; it went to the notice -- a named
    // group, inside the modal -- and never to a control whose Enter would
    // decide for them.
    expect(document.activeElement).toBe(within(dialog).getByRole("group", { name: MOVED }));
    fireEvent.click(saveButton(dialog));
    await settle();
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Keep my changes" }));
    await settle();
    expect(within(dialog).queryByText(MOVED)).toBeNull();
    // The choice leads to the act it enables.
    expect(document.activeElement).toBe(saveButton(dialog));
    expect(within(dialog).getByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(saveButton(dialog));
    await settle();
    expect(conn.query.fleetSetSharing).toHaveBeenCalledTimes(1);
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "cluster",
      userIds: [],
      groupIds: [],
    });
  });

  it("takes the new share on request, and drops nothing but the draft it replaces", async () => {
    const { conn, rerender } = mount(studio());
    const dialog = await openDialog();
    fireEvent.click(radio(dialog, /^Everyone in this cluster/));
    rerender(studio({ mode: "people", userIds: ["bo"] }));
    await settle();

    fireEvent.click(within(dialog).getByRole("button", { name: "Use the new sharing" }));
    await settle();
    expect(within(dialog).queryByText(MOVED)).toBeNull();
    expect(radio(dialog, /^Specific people and groups/).getAttribute("aria-checked")).toBe("true");
    expect(chipTexts(dialog)).toEqual(["Bo Chen"]);
    // The dialog now says what is stored, so there is nothing to save...
    expect(within(dialog).getByText("Nothing changed yet.")).toBeTruthy();
    expect(saveButton(dialog).disabled).toBe(true);
    // ...and focus is back on the choice in force, not on the page behind.
    expect(document.activeElement).toBe(radio(dialog, /^Specific people and groups/));
    expect(conn.query.fleetSetSharing).not.toHaveBeenCalled();
  });

  it("lets a draft nobody has touched follow the new share, and says so", async () => {
    const { rerender } = mount(studio());
    const dialog = await openDialog();
    rerender(studio({ mode: "cluster" }));
    await settle();

    // Nothing of the person's was at stake, so the dialog shows the truth --
    // where it used to keep "Only me" and offer a Save that would have
    // reverted the other change.
    expect(radio(dialog, /^Everyone in this cluster/).getAttribute("aria-checked")).toBe("true");
    expect(within(dialog).getByText(MOVED)).toBeTruthy();
    expect(within(dialog).getByText("This shows the new sharing.")).toBeTruthy();
    expect(within(dialog).getByText("Nothing changed yet.")).toBeTruthy();
    expect(saveButton(dialog).disabled).toBe(true);
    // Focus was on the choice in force, and it goes with it: a ring left on
    // "Only me" beside a selection on Everyone would point at two answers.
    expect(document.activeElement).toBe(radio(dialog, /^Everyone in this cluster/));

    // The first edit makes the draft the person's, and the note goes with it:
    // "this shows the new sharing" would no longer be true.
    fireEvent.click(radio(dialog, /^Only me/));
    expect(within(dialog).queryByText("This shows the new sharing.")).toBeNull();
    expect(within(dialog).getByText("Unsaved changes")).toBeTruthy();
  });

  it("leaves focus where the person put it when a follow does not touch it", async () => {
    const stored = { mode: "people", userIds: ["ana"] };
    const { rerender } = mount(studio(stored), {
      directory: directory({ current: current([{ id: "ana", name: "Ana Ruiz", known: true, inDirectory: true }]) }),
    });
    await settle();
    const dialog = await openDialog();
    const box = search(dialog);
    box.focus();
    rerender(studio({ ...stored, userIds: ["ana", "bo"] }));
    await settle();
    expect(chipTexts(dialog)).toEqual(["Ana Ruiz", "Bo Chen"]);
    expect(within(dialog).getByText("This shows the new sharing.")).toBeTruthy();
    expect(document.activeElement).toBe(box);
  });

  it("asks nothing when the draft already says what is now stored", async () => {
    const { rerender } = mount(studio());
    const dialog = await openDialog();
    fireEvent.click(radio(dialog, /^Everyone in this cluster/));
    rerender(studio({ mode: "cluster" }));
    await settle();
    expect(within(dialog).queryByText(MOVED)).toBeNull();
    expect(within(dialog).getByText("Nothing changed yet.")).toBeTruthy();
    // An edit after it is an ordinary edit, measured from the new share.
    fireEvent.click(radio(dialog, /^Only me/));
    expect(within(dialog).queryByText(MOVED)).toBeNull();
    expect(saveButton(dialog).disabled).toBe(false);
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
    const dialog = await openDialog();
    fireEvent.change(search(dialog), { target: { value: "bo" } });
    fireEvent.keyDown(search(dialog), { key: "Enter" });
    rerender(
      studio({ ...stored, userIds: ["ana"] }, { lastSeenAt: new Date(Date.now() + 15_000).toISOString() }),
    );
    await settle();
    expect(within(dialog).queryByText(MOVED)).toBeNull();
    expect(saveButton(dialog).disabled).toBe(false);
    expect(chipTexts(dialog)).toEqual(["Ana Ruiz", "Design", "Bo Chen"]);
  });
});

// ---------------------------------------------------------------------------
// A directory that could not be read (memql#5659, item 2)
// ---------------------------------------------------------------------------
//
// WHO THE MACHINE IS LENT TO IS ON THE ROW; ONLY THEIR NAMES ARE IN THE
// DIRECTORY. The chips used to be drawn only once the directory answered, so a
// failed read left the owner unable to see or change who could use their
// machine. The list comes from the row now, and the directory only names it.

describe("a share directory that could not be read", () => {
  it("keeps who the machine is lent to on the list, removable, while names load and after they fail", async () => {
    let fail: (err: unknown) => void = () => {};
    const pending = new Promise<never>((_, reject) => {
      fail = reject;
    });
    const conn = fakeConnection();
    conn.query.fleetShareDirectory.mockImplementation(() => pending);
    mount(studio({ mode: "people", userIds: ["ana", "bo"], groupIds: ["design"] }), { conn });
    const dialog = await openDialog();

    // While the read is out: the list's own shape, as long as the row says,
    // with nothing to focus and no name guessed.
    expect(dialog.querySelectorAll(".fleet-share-chip")).toHaveLength(3);
    expect(within(dialog).queryByRole("button", { name: /^Remove / })).toBeNull();
    expect(within(dialog).queryByText(/Unknown/)).toBeNull();

    await act(async () => {
      fail(new Error("stream closed"));
    });
    await settle();

    // Failed: still all three, by kind and place on the list -- never an id,
    // never "unknown", which would be a claim about the person.
    expect(chipTexts(dialog)).toEqual(["Person 1", "Person 2", "Group 1"]);
    expect(within(dialog).getByText("Names could not be loaded.")).toBeTruthy();
    expect(within(dialog).getByText("stream closed")).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Try again" })).toBeTruthy();
    // Nobody can be added from a directory nobody read.
    expect(within(dialog).queryByRole("combobox")).toBeNull();

    // Still the owner's list to change: remove one, and the rest go back
    // exactly as stored.
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove Person 1" }));
    // A label is a stable handle on its entry, not a count that renumbers.
    expect(chipTexts(dialog)).toEqual(["Person 2", "Group 1"]);
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Remove Person 2" }));
    fireEvent.click(saveButton(dialog));
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
    const dialog = await openDialog();
    expect(chipTexts(dialog)).toEqual(["Person 1", "Group 1"]);

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
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
    await settle();
    expect(chipTexts(dialog)).toEqual(["Ana Ruiz", "Design"]);
    expect(within(dialog).queryByText("Names could not be loaded.")).toBeNull();
    expect(search(dialog)).toBeTruthy();
  });

  it("still stops lending to anyone with Only me", async () => {
    const conn = fakeConnection();
    conn.query.fleetShareDirectory.mockRejectedValue(new Error("stream closed"));
    mount(studio({ mode: "people", userIds: ["ana"], groupIds: ["design"] }), { conn });
    const dialog = await openDialog();
    fireEvent.click(radio(dialog, /^Only me/));
    fireEvent.click(saveButton(dialog));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "owner",
      userIds: [],
      groupIds: [],
    });
  });
});

// ---------------------------------------------------------------------------
// A stored canonical id (memql#5659, item 3)
// ---------------------------------------------------------------------------
//
// The engine stores bare ids and sends the directory bare, but a row written
// with the canonical spelling is still one person. `sameEntityId` and
// `bareShortId` are tested as helpers; this is the dialog path end to end.

describe("a share stored with canonical ids", () => {
  it("matches the directory's bare ids in the dialog, and sends each id back spelled as stored", async () => {
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
    const dialog = await openDialog();
    // Named, and not marked as having left the directory.
    expect(chipTexts(dialog)).toEqual(["Ana Ruiz", "Design"]);
    expect(within(dialog).queryByText("No longer available to pick")).toBeNull();
    // Not offered a second time under the other spelling.
    expect(within(dialog).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "OpsGroup of 1 person",
      "Bo Chen",
      "Cy Diaz",
    ]);
    // And the same list as the one stored, so nothing has changed yet.
    expect(saveButton(dialog).disabled).toBe(true);

    fireEvent.change(search(dialog), { target: { value: "bo" } });
    fireEvent.keyDown(search(dialog), { key: "Enter" });
    fireEvent.click(saveButton(dialog));
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
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove Person 2" }));
    fireEvent.click(saveButton(dialog));
    await settle();
    expect(conn.query.fleetSetSharing.mock.calls[0]?.[0]).toEqual({
      registrationId: MACHINE_ID,
      mode: "people",
      userIds: ["v1:identity:user:ana"],
      groupIds: [],
    });
  });
});

import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { intersect } from "../intersectionObserver";
import { readTheBarNow, useFakeDate } from "../readTheBar";
import { installSeededAccess, seededAccessFor, seededAccessWithout } from "../seededAccess";
import { chooseOption } from "../selectControl";
import { setEffectiveCapabilities } from "../../src/system/roles";

// THE DATA TAB'S ROWS, AND MOVING THEM (design record 2026-11-12-account-export,
// sections I and K). A kind's line opens its rows as a page under the trail;
// choosing rows brings one act, Move to account, which opens the wizard "Move
// rows". On the primary account, the rows awaiting a decision and the kinds in
// no account. Every control here belongs to `execute app:accounts/move`, and
// is absent without it.
//
// The four calls are the engine's (`accountRows`, `accountUnplaced`,
// `accountRowsMove`, `accountConflictLeaveOut`), called by name; the
// connection is faked as the rest of the accounts suite fakes it.

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
const { SELF_ACCOUNT_ID } = await import("../../src/apps/accounts/rows");
const { accountRow, fakeConnection, openAccountTab, rowsResult, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;
type Answer = Row[] | Error | Promise<Row[]>;
type Handler = (call: string) => Answer;

const MOVE = { verb: "execute", resource: "app:accounts/move", effect: "allow" as const, source: "role" as const };

function holdMovePart() {
  setEffectiveCapabilities([...seededAccessFor("owner"), MOVE]);
}

const INVENTORY = [
  { kind: "concept", concept: "v1:library:artifact", namespace: "library", name: "artifact", rows: 3, versions: 3 },
  { kind: "concept", concept: "v1:notes:note", namespace: "notes", name: "note", rows: 1, versions: 1 },
  { kind: "files", bytes: 0 },
];

const row = (id: string, primary: string, over: Record<string, unknown> = {}) => ({
  kind: "row", id, concept: "v1:library:artifact", primary, secondary: "", tertiary: "", status: "",
  ownerUserId: "", createdAt: "2026-11-01T00:00:00Z", updatedAt: "2026-11-01T00:00:00Z", followsParent: false, ...over,
});

const ACCOUNTS = [
  accountRow({ id: SELF_ACCOUNT_ID, name: "Our Studio", createdAt: "2026-08-03T00:00:00Z" }),
  accountRow({ id: "a1", name: "Acme Consulting", createdAt: "2026-08-02T00:00:00Z" }),
  accountRow({ id: "b2", name: "Borden Ltd", domain: "borden.test", createdAt: "2026-08-01T00:00:00Z" }),
];

/** A connection whose four move calls answer through `handlers`. */
function connection(handlers: Partial<Record<"accountRows" | "accountUnplaced" | "accountRowsMove" | "accountConflictLeaveOut", Handler>>, seed: Parameters<typeof fakeConnection>[0] = {}) {
  const conn = fakeConnection({ clientAccountsAll: ACCOUNTS, accountInventory: INVENTORY, accountAssignmentStatus: [{ kind: "switch", enabled: true, held: false }], ...seed });
  const original = conn.query.executeNamed.getMockImplementation()!;
  conn.query.executeNamed.mockImplementation(async (name: string, call: string) => {
    const handler = handlers[name as keyof typeof handlers];
    if (handler === undefined) return original(name, call);
    const answer = await handler(call);
    if (answer instanceof Error) throw answer;
    return rowsResult(answer as never);
  });
  return conn;
}

/** The calls made to one builtin, as written. */
function callsTo(conn: Conn, name: string): string[] {
  return conn.query.executeNamed.mock.calls.filter((c) => c[0] === name).map((c) => c[1] as string);
}

function mount(conn: Conn) {
  h.connection = conn;
  const bag = new Map<string, string>();
  const store = new LocalAccountsSettingsStore({ getItem: (k: string) => bag.get(k) ?? null, setItem: (k: string, v: string) => void bag.set(k, v) });
  return render(withSession(<AccountsApp sectionId="accounts" navigate={vi.fn()} askContext={() => {}} store={store} />));
}

async function openData(conn: Conn, account = "Acme Consulting") {
  mount(conn);
  fireEvent.click(await screen.findByText(account));
  return openAccountTab("Data");
}

/** Open the Files area on the Data tab and its artifact kind. */
async function openArtifacts(conn: Conn) {
  const data = await openData(conn);
  fireEvent.click(await within(data).findByRole("button", { name: /^Files, / }));
  fireEvent.click(within(data).getByRole("button", { name: "artifact, 3 rows" }));
  return screen.findByRole("heading", { name: "artifact" });
}

/** The list item that holds a row's visible name. */
const lineOf = (name: string) => screen.getAllByRole("listitem").find((li) => within(li).queryByText(name) !== null)!;

const crumbs = () => within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((li) => li.textContent);

/** The bar on the window's bottom edge. */
const bar = () => screen.queryByRole("group", { name: "What you can do with this" });

beforeEach(holdMovePart);
afterEach(() => installSeededAccess("owner"));

describe("a kind's rows", () => {
  it("opens from the inventory as a page under the trail, and Back returns to the Data tab", async () => {
    const conn = connection({ accountRows: () => [row("r1", "Brief.pdf"), { kind: "page", nextCursor: "" }] });
    await openArtifacts(conn);
    expect(crumbs()).toEqual(["Accounts", "Acme Consulting", "Data", "Files", "artifact"]);
    expect(await screen.findByText("Brief.pdf")).toBeTruthy();
    expect(callsTo(conn, "accountRows")).toEqual(['builtin accountRows(accountId: "a1", concept: "v1:library:artifact", limit: 50)']);
    // The tabs are behind the page, not beside it.
    expect(screen.queryByRole("navigation", { name: "Account views" })).toBeNull();

    fireEvent.click(within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getByRole("button", { name: "Data" }));
    const data = await screen.findByRole("region", { name: "Data" });
    expect(within(data).getByRole("region", { name: "Inventory" })).toBeTruthy();
    // The inventory was not read again for the way back.
    expect(callsTo(conn, "accountInventory")).toHaveLength(1);
  });

  it("draws the shape of the list while it reads, and says so when the kind has none here", async () => {
    let release: (rows: Row[]) => void = () => {};
    const conn = connection({ accountRows: () => new Promise<Row[]>((resolve) => { release = resolve; }) });
    await openArtifacts(conn);
    expect(screen.getByText("Loading rows")).toBeTruthy();
    expect(screen.queryByText("No rows of this kind here.")).toBeNull();
    await act(async () => release([{ kind: "page", nextCursor: "" }]));
    expect(await screen.findByText("No rows of this kind here.")).toBeTruthy();
    // No search over no content.
    expect(screen.queryByRole("button", { name: "Search rows of artifact" })).toBeNull();
  });

  it("loads the next page as the end comes into view, passing the cursor back", async () => {
    const conn = connection({
      accountRows: (call) => call.includes('after: "c2"')
        ? [row("r2", "Contract.docx"), { kind: "page", nextCursor: "" }]
        : [row("r1", "Brief.pdf"), { kind: "page", nextCursor: "c2" }],
    });
    await openArtifacts(conn);
    await screen.findByText("Brief.pdf");
    // The sentinel has no role or text of its own; it is found by its class.
    const sentinel = document.querySelector(".os-next-page-sentinel")!;
    await act(async () => intersect(sentinel));
    expect(await screen.findByText("Contract.docx")).toBeTruthy();
    expect(screen.getByText("Brief.pdf")).toBeTruthy();
    expect(callsTo(conn, "accountRows")[1]).toBe('builtin accountRows(accountId: "a1", concept: "v1:library:artifact", limit: 50, after: "c2")');
    expect(screen.getByText("End of list")).toBeTruthy();
  });

  it("searches through Refine, by asking the engine", async () => {
    const conn = connection({
      accountRows: (call) => call.includes('search: "con"')
        ? [row("r2", "Contract.docx"), { kind: "page", nextCursor: "" }]
        : [row("r1", "Brief.pdf"), row("r2", "Contract.docx"), { kind: "page", nextCursor: "" }],
    });
    await openArtifacts(conn);
    await screen.findByText("Brief.pdf");
    fireEvent.click(screen.getByRole("button", { name: "Search rows of artifact" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "con" } });
    await waitFor(() => expect(screen.queryByText("Brief.pdf")).toBeNull());
    expect(screen.getByText("Contract.docx")).toBeTruthy();
    expect(callsTo(conn, "accountRows").at(-1)).toBe('builtin accountRows(accountId: "a1", concept: "v1:library:artifact", limit: 50, search: "con")');
  });

  it("shows a refusal in the engine's words, and retries in place", async () => {
    let refuse = true;
    const conn = connection({ accountRows: () => refuse ? new Error("this kind is not account-scoped") : [row("r1", "Brief.pdf"), { kind: "page", nextCursor: "" }] });
    await openArtifacts(conn);
    expect(await screen.findByText("this kind is not account-scoped")).toBeTruthy();
    refuse = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Brief.pdf")).toBeTruthy();
  });
});

describe("choosing rows", () => {
  const ROWS = () => [
    row("r1", "Brief.pdf", { secondary: "PDF", status: "active" }),
    row("r2", "Contract.docx"),
    row("r3", "Brief.pdf, page 2", { followsParent: true }),
    { kind: "page", nextCursor: "" },
  ];

  it("brings one bar with the count in words and one act", async () => {
    const conn = connection({ accountRows: ROWS });
    await openArtifacts(conn);
    const first = await screen.findByRole("button", { name: /Brief\.pdf/ });
    expect(bar()).toBeNull();
    fireEvent.click(first);
    expect(first.getAttribute("aria-pressed")).toBe("true");
    expect(within(bar()!).getByText("1 row selected")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Contract\.docx/ }));
    expect(within(bar()!).getByText("2 rows selected")).toBeTruthy();
    // One act, and the quiet way to let go of everything.
    expect(within(bar()!).getAllByRole("button").map((b) => b.textContent)).toEqual(["Clear", "Move to account"]);
    // Choosing again lets go.
    fireEvent.click(first);
    fireEvent.click(screen.getByRole("button", { name: /Contract\.docx/ }));
    expect(bar()).toBeNull();
  });

  it("does not let a row that follows a parent be chosen, and says so on the row", async () => {
    const conn = connection({ accountRows: ROWS });
    await openArtifacts(conn);
    const follower = await screen.findByText("Brief.pdf, page 2");
    expect(follower.closest("button")).toBeNull();
    expect(within(lineOf("Brief.pdf, page 2")).getByText("Moves with the row it belongs to")).toBeTruthy();
    fireEvent.click(follower);
    expect(bar()).toBeNull();
  });

  it("names the kind a follower moves with, when the answer says", async () => {
    const conn = connection({ accountRows: () => [
      row("r3", "Brief.pdf, page 2", { followsParent: true, parentConcept: "v1:work:goal" }),
      { kind: "page", nextCursor: "" },
    ] });
    await openArtifacts(conn);
    await screen.findByText("Brief.pdf, page 2");
    const line = lineOf("Brief.pdf, page 2");
    expect(within(line).getByText("Moves with its goal")).toBeTruthy();
    expect(within(line).queryByText("Moves with the row it belongs to")).toBeNull();
    // The concept id itself is never on screen.
    expect(line.textContent).not.toContain("v1:work:goal");
  });

  it("keeps the choice when the next page loads", async () => {
    const conn = connection({
      accountRows: (call) => call.includes('after: "c2"')
        ? [row("r4", "Invoice.pdf"), { kind: "page", nextCursor: "" }]
        : [row("r1", "Brief.pdf"), { kind: "page", nextCursor: "c2" }],
    });
    await openArtifacts(conn);
    fireEvent.click(await screen.findByRole("button", { name: /Brief\.pdf/ }));
    // The sentinel has no role or text of its own; it is found by its class.
    await act(async () => intersect(document.querySelector(".os-next-page-sentinel")!));
    fireEvent.click(await screen.findByRole("button", { name: /Invoice\.pdf/ }));
    expect(screen.getByRole("button", { name: /Brief\.pdf/ }).getAttribute("aria-pressed")).toBe("true");
    expect(within(bar()!).getByText("2 rows selected")).toBeTruthy();
  });

  it("keeps the choice across a search, says how many are not shown, and moves them all", async () => {
    const conn = connection({
      accountRows: (call) => call.includes('search: "con"')
        ? [row("r2", "Contract.docx"), { kind: "page", nextCursor: "" }]
        : ROWS(),
    });
    await openArtifacts(conn);
    fireEvent.click(await screen.findByRole("button", { name: /Brief\.pdf/ }));
    fireEvent.click(screen.getByRole("button", { name: /Contract\.docx/ }));
    fireEvent.click(screen.getByRole("button", { name: "Search rows of artifact" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "con" } });
    await waitFor(() => expect(screen.queryByText("Brief.pdf")).toBeNull());
    expect(within(bar()!).getByText("2 rows selected, 1 not shown")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Contract\.docx/ }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(within(bar()!).getByRole("button", { name: "Move to account" }));
    expect(await screen.findByRole("heading", { name: "Move rows" })).toBeTruthy();
    expect(screen.getByText("2 rows of artifact")).toBeTruthy();
  });

  it("empties the choice with Clear, shown or not", async () => {
    const conn = connection({
      accountRows: (call) => call.includes('search: "con"')
        ? [row("r2", "Contract.docx"), { kind: "page", nextCursor: "" }]
        : ROWS(),
    });
    await openArtifacts(conn);
    fireEvent.click(await screen.findByRole("button", { name: /Brief\.pdf/ }));
    fireEvent.click(screen.getByRole("button", { name: "Search rows of artifact" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "con" } });
    await waitFor(() => expect(screen.queryByText("Brief.pdf")).toBeNull());
    expect(within(bar()!).getByText("1 row selected, 1 not shown")).toBeTruthy();
    fireEvent.click(within(bar()!).getByRole("button", { name: "Clear" }));
    expect(bar()).toBeNull();
  });

  it("says at most 500 rows move at once, and offers no act past it", async () => {
    const many = Array.from({ length: 501 }, (_, i) => row(`m${i}`, `Item ${i}`));
    const conn = connection({ accountRows: () => [...many, { kind: "page", nextCursor: "" }] });
    await openArtifacts(conn);
    await screen.findByText("Item 0");
    // Found once: the rows keep their elements as the choice changes, and a
    // role query over 501 buttons per click takes minutes.
    const items = screen.getAllByRole("button", { name: /^Item \d+$/ });
    expect(items).toHaveLength(501);
    for (let i = 0; i < 500; i += 1) fireEvent.click(items[i]!);
    expect(within(bar()!).getByText("500 rows selected")).toBeTruthy();
    expect(within(bar()!).getByRole("button", { name: "Move to account" })).toBeTruthy();
    fireEvent.click(items[500]!);
    expect(within(bar()!).getByText("501 rows selected")).toBeTruthy();
    expect(within(bar()!).getByText(/^At most 500 rows/)).toBeTruthy();
    expect(within(bar()!).queryByRole("button", { name: "Move to account" })).toBeNull();
    // The way out stays.
    expect(within(bar()!).getByRole("button", { name: "Clear" })).toBeTruthy();
  }, 30_000);
});

describe("the wizard", () => {
  useFakeDate();

  const PREVIEW = [
    { kind: "wouldMove", concept: "v1:library:artifact", rows: 2 },
    { kind: "follows", concept: "v1:library:file" },
    { kind: "refused", concept: "v1:library:artifact", id: "r9", reason: "states_own_account" },
    { kind: "summary", moved: 2, followerKinds: 1, refused: 1 },
  ];
  const MOVED = [
    { kind: "moved", concept: "v1:library:artifact", rows: 2 },
    { kind: "follows", concept: "v1:library:file" },
    { kind: "summary", moved: 2, followerKinds: 1, refused: 0 },
  ];

  async function startMove(handlers: Parameters<typeof connection>[0]) {
    const conn = connection({
      accountRows: () => [row("r1", "Brief.pdf"), row("r2", "Contract.docx"), { kind: "page", nextCursor: "" }],
      ...handlers,
    });
    await openArtifacts(conn);
    fireEvent.click(await screen.findByRole("button", { name: /Brief\.pdf/ }));
    fireEvent.click(screen.getByRole("button", { name: /Contract\.docx/ }));
    fireEvent.click(within(bar()!).getByRole("button", { name: "Move to account" }));
    await screen.findByRole("heading", { name: "Move rows" });
    return conn;
  }

  const pressInBar = (name: string) => {
    readTheBarNow();
    fireEvent.click(within(bar()!).getByRole("button", { name }));
  };

  it("offers every account but the one the rows are in, previews, moves, and says followers follow", async () => {
    const conn = await startMove({ accountRowsMove: (call) => (call.includes("preview: true") ? PREVIEW : MOVED) });
    const picker = screen.getByRole("combobox", { name: "Destination account" });
    fireEvent.click(picker);
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options).toContain("Borden Ltd");
    expect(options).toContain("Our Studio");
    expect(options).not.toContain("Acme Consulting");
    chooseOption(picker, "Borden Ltd");

    // Review: the preview, per kind, in words. Nothing is written.
    expect(await screen.findByText("Moves")).toBeTruthy();
    expect(callsTo(conn, "accountRowsMove")).toEqual([
      'builtin accountRowsMove(rows: [{concept: "v1:library:artifact", id: "r1"}, {concept: "v1:library:artifact", id: "r2"}], toAccountId: "b2", preview: true)',
    ]);
    // The kinds that follow, named, under one sentence; never counted.
    expect(screen.getByText("What hangs off these rows follows in the background, and an export waits until it has.")).toBeTruthy();
    expect(screen.getByText("Kinds that follow: file")).toBeTruthy();
    expect(screen.getByText("1 row. This kind states its own account and is changed in its own app.")).toBeTruthy();

    const before = callsTo(conn, "accountRows").length;
    pressInBar("Move");
    await waitFor(() => expect(within(bar()!).getByText("Moved")).toBeTruthy());
    expect(callsTo(conn, "accountRowsMove")[1]).toContain("preview: false");
    expect(screen.getByText("What hangs off the rows that moved follows in the background, and an export waits until it has.")).toBeTruthy();
    expect(screen.getByText("Kinds that follow: file")).toBeTruthy();

    // Done returns to the rows, read again behind the wizard once the move
    // returned, with nothing chosen.
    pressInBar("Done");
    expect(await screen.findByRole("heading", { name: "artifact" })).toBeTruthy();
    expect(bar()).toBeNull();
    expect(callsTo(conn, "accountRows").length).toBeGreaterThan(before);
  });

  it("names a kind that follows and gives no number, even if a count arrives", async () => {
    await startMove({
      accountRowsMove: () => [
        { kind: "wouldMove", concept: "v1:library:artifact", rows: 2 },
        { kind: "follows", concept: "v1:work:goal", rows: 37 },
        { kind: "summary", moved: 2, followerKinds: 1, refused: 0 },
      ],
    });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    const named = await screen.findByText("Kinds that follow: goal");
    expect(named.textContent).not.toMatch(/\d/);
    expect(screen.queryByText(/37/)).toBeNull();
    // The bar's total is of the rows chosen and moving, never of followers.
    expect(within(bar()!).getByText("2 rows to Borden Ltd")).toBeTruthy();
  });

  it("names every refusal in a sentence", async () => {
    const reasons = ["cluster_wide", "undeclared", "stays", "follows_parent", "states_own_account", "same_account", "unknown_account", "not_found"];
    await startMove({
      accountRowsMove: () => reasons.map((reason, i) => ({ kind: "refused", concept: `v1:x:kind${i}`, id: `r${i}`, reason })),
    });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    expect(await screen.findByText("None of these rows can move to this account.")).toBeTruthy();
    for (let i = 0; i < reasons.length; i += 1) {
      const fact = screen.getByText(`kind${i}`).closest("div")!;
      // A sentence, never the code.
      expect(fact.textContent).not.toContain(reasons[i]!);
      expect(fact.textContent).toMatch(/^kind\d1 row\. [A-Z].+\.$/);
    }
    // Nothing can move, so there is no Move.
    expect(within(bar()!).queryByRole("button", { name: "Move" })).toBeNull();
  });

  it("keeps a failed move on screen in the engine's words, with a retry", async () => {
    let fail = true;
    const conn = await startMove({
      accountRowsMove: (call) => call.includes("preview: true") ? PREVIEW : fail ? new Error("the destination account is archived") : MOVED,
    });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    await screen.findByText("Moves");
    pressInBar("Move");
    expect(await screen.findByText("the destination account is archived")).toBeTruthy();
    expect(screen.getByText("The rows were not moved.")).toBeTruthy();
    // The choice behind the wizard is untouched by a move that failed.
    pressInBar("Cancel");
    expect(await screen.findByRole("heading", { name: "artifact" })).toBeTruthy();
    expect(within(bar()!).getByText("2 rows selected")).toBeTruthy();
    fireEvent.click(within(bar()!).getByRole("button", { name: "Move to account" }));
    await screen.findByRole("heading", { name: "Move rows" });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    await screen.findByText("Moves");
    pressInBar("Move");
    expect(await screen.findByText("the destination account is archived")).toBeTruthy();
    fail = false;
    pressInBar("Try again");
    await waitFor(() => expect(within(bar()!).getByText("Moved")).toBeTruthy());
    expect(callsTo(conn, "accountRowsMove").filter((c) => c.includes("preview: false"))).toHaveLength(3);
  });

  it("keeps the refused rows chosen after a move, and lets the moved ones go", async () => {
    await startMove({
      accountRowsMove: (call) => call.includes("preview: true")
        ? [{ kind: "wouldMove", concept: "v1:library:artifact", rows: 1 }, { kind: "refused", concept: "v1:library:artifact", id: "r2", reason: "same_account" }]
        : [{ kind: "moved", concept: "v1:library:artifact", rows: 1 }, { kind: "refused", concept: "v1:library:artifact", id: "r2", reason: "same_account" }],
    });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    await screen.findByText("Moves");
    pressInBar("Move");
    await waitFor(() => expect(within(bar()!).getByText("Moved")).toBeTruthy());
    pressInBar("Done");
    expect(await screen.findByRole("heading", { name: "artifact" })).toBeTruthy();
    await waitFor(() => expect(within(bar()!).getByText("1 row selected")).toBeTruthy());
    expect(screen.getByRole("button", { name: /Contract\.docx/ }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("button", { name: /Brief\.pdf/ }).getAttribute("aria-pressed")).toBe("false");
  });

  it("is not left by its back while the move is in flight", async () => {
    let finish: (rows: Row[]) => void = () => {};
    await startMove({
      accountRowsMove: (call) => call.includes("preview: true") ? PREVIEW : new Promise<Row[]>((resolve) => { finish = resolve; }),
    });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    await screen.findByText("Moves");
    pressInBar("Move");
    await waitFor(() => expect(within(bar()!).getByText("Moving rows")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Back to artifact" }));
    fireEvent.click(within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getByRole("button", { name: "artifact" }));
    expect(screen.getByRole("heading", { name: "Move rows" })).toBeTruthy();
    await act(async () => finish(MOVED as never));
    await waitFor(() => expect(within(bar()!).getByText("Moved")).toBeTruthy());
  });

  it("keeps a failed preview on screen with a retry, and offers no Move", async () => {
    let fail = true;
    await startMove({ accountRowsMove: () => (fail ? new Error("too many rows in one move") : PREVIEW) });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    expect(await screen.findByText("too many rows in one move")).toBeTruthy();
    expect(within(bar()!).queryByRole("button", { name: "Move" })).toBeNull();
    fail = false;
    pressInBar("Try again");
    expect(await screen.findByText("Moves")).toBeTruthy();
  });

  it("goes back to the rows with their choice on Cancel, having written nothing", async () => {
    const conn = await startMove({ accountRowsMove: () => PREVIEW });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    await screen.findByText("Moves");
    pressInBar("Cancel");
    expect(await screen.findByRole("heading", { name: "artifact" })).toBeTruthy();
    expect(within(bar()!).getByText("2 rows selected")).toBeTruthy();
    // Only the preview was asked; nothing was moved.
    expect(callsTo(conn, "accountRowsMove")).toHaveLength(1);
    expect(callsTo(conn, "accountRowsMove").every((c) => c.includes("preview: true"))).toBe(true);
  });

  it("names an unknown refusal plainly, never by its code", async () => {
    await startMove({ accountRowsMove: () => [{ kind: "refused", concept: "v1:x:thing", id: "r1", reason: "brand_new_reason" }] });
    chooseOption(screen.getByRole("combobox", { name: "Destination account" }), "Borden Ltd");
    expect(await screen.findByText("1 row. It cannot be moved.")).toBeTruthy();
    expect(screen.queryByText(/brand_new_reason/)).toBeNull();
    // No raw concept id behind a fact either.
    expect(document.querySelector('[title="v1:x:thing"]')).toBeNull();
  });
});

describe("on the primary account", () => {
  useFakeDate();

  const UNPLACED = [
    { kind: "unplaced", concept: "v1:shopify:store", namespace: "shopify", name: "store", rows: 2 },
    { kind: "conflict", concept: "v1:shopify:store", rowId: "store-7", conflict: "store_bound_twice", detail: "", candidates: ["b2"], foundAt: "2026-11-12T09:00:00Z" },
  ];

  it("lists what awaits a decision and what is in no account, from one call", async () => {
    const conn = connection({ accountUnplaced: () => UNPLACED });
    const data = await openData(conn, "Our Studio");
    const awaiting = await within(data).findByRole("region", { name: "Awaiting a decision" });
    expect(await within(awaiting).findByText("store-7")).toBeTruthy();
    expect(within(awaiting).getByText("Its store is bound to more than one account.")).toBeTruthy();
    const none = within(data).getByRole("region", { name: "In no account" });
    expect(within(none).getByRole("button", { name: "store, 2 rows in no account" })).toBeTruthy();
    expect(callsTo(conn, "accountUnplaced")).toEqual(["builtin accountUnplaced()"]);
    // Said once: the existing-data panel does not list the conflict again.
    expect(within(data).queryByText("Needs a decision")).toBeNull();
  });

  it("places a conflict through the wizard, its candidates offered first", async () => {
    const conn = connection({
      accountUnplaced: () => UNPLACED,
      accountRowsMove: (call) => call.includes("preview: true")
        ? [{ kind: "wouldMove", concept: "v1:shopify:store", rows: 1 }]
        : [{ kind: "moved", concept: "v1:shopify:store", rows: 1 }],
    });
    const data = await openData(conn, "Our Studio");
    fireEvent.click(await within(data).findByRole("button", { name: "Place store-7 in an account" }));
    expect(await screen.findByRole("heading", { name: "Place in an account" })).toBeTruthy();
    const picker = screen.getByRole("combobox", { name: "Destination account" });
    fireEvent.click(picker);
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    // The prompt, then the candidate, then every other account.
    expect(options.slice(1, 2)).toEqual(["Borden Ltd"]);
    expect(options).toContain("Our Studio");
    chooseOption(picker, "Borden Ltd");
    await screen.findByText("Moves");
    readTheBarNow();
    fireEvent.click(within(bar()!).getByRole("button", { name: "Move" }));
    await waitFor(() => expect(within(bar()!).getByText("Moved")).toBeTruthy());
    expect(callsTo(conn, "accountRowsMove")[1]).toBe('builtin accountRowsMove(rows: [{concept: "v1:shopify:store", id: "store-7"}], toAccountId: "b2", preview: false)');
    // The decisions are read again once it moved.
    await waitFor(() => expect(callsTo(conn, "accountUnplaced").length).toBeGreaterThan(1));
  });

  it("leaves a conflict out after asking in place, naming the row", async () => {
    const conn = connection({ accountUnplaced: () => UNPLACED, accountConflictLeaveOut: () => [{ kind: "conflict", leftOutAt: "2026-11-12T10:00:00Z" }] });
    const data = await openData(conn, "Our Studio");
    fireEvent.click(await within(data).findByRole("button", { name: "Leave out store-7" }));
    const ask = within(data).getByRole("group", { name: "Leave out store-7?" });
    expect(within(ask).getByText(/^Leave out store-7\?/)).toBeTruthy();
    // The second click of the double-click that asked answers nothing.
    fireEvent.click(within(ask).getByRole("button", { name: "Leave out" }));
    expect(callsTo(conn, "accountConflictLeaveOut")).toHaveLength(0);
    readTheBarNow();
    fireEvent.click(within(ask).getByRole("button", { name: "Leave out" }));
    await waitFor(() => expect(callsTo(conn, "accountConflictLeaveOut")).toEqual([
      'builtin accountConflictLeaveOut(concept: "v1:shopify:store", rowId: "store-7", conflict: "store_bound_twice")',
    ]));
    await waitFor(() => expect(callsTo(conn, "accountUnplaced").length).toBeGreaterThan(1));
  });

  it("names an unknown conflict plainly, never by its code", async () => {
    const conn = connection({ accountUnplaced: () => [
      { kind: "conflict", concept: "v1:shopify:store", rowId: "store-9", conflict: "brand_new_conflict", detail: "", candidates: [], foundAt: "" },
    ] });
    const data = await openData(conn, "Our Studio");
    const awaiting = await within(data).findByRole("region", { name: "Awaiting a decision" });
    expect(await within(awaiting).findByText("It needs a decision.")).toBeTruthy();
    expect(within(awaiting).queryByText(/brand_new_conflict/)).toBeNull();
  });

  it("keeps a refused leave-out in the engine's words, with a retry", async () => {
    const conn = connection({ accountUnplaced: () => UNPLACED, accountConflictLeaveOut: () => new Error("the conflict was already resolved") });
    const data = await openData(conn, "Our Studio");
    fireEvent.click(await within(data).findByRole("button", { name: "Leave out store-7" }));
    readTheBarNow();
    fireEvent.click(within(data).getByRole("button", { name: "Leave out" }));
    expect(await within(data).findByText("the conflict was already resolved")).toBeTruthy();
    expect(within(data).getByRole("button", { name: "Try again" })).toBeTruthy();
  });

  it("opens a kind's rows in no account, to place them", async () => {
    const conn = connection({ accountUnplaced: () => UNPLACED, accountRows: () => [row("s1", "Main store", { concept: "v1:shopify:store" }), { kind: "page", nextCursor: "" }] });
    const data = await openData(conn, "Our Studio");
    fireEvent.click(await within(data).findByRole("button", { name: "store, 2 rows in no account" }));
    expect(await screen.findByRole("heading", { name: "store" })).toBeTruthy();
    expect(crumbs()).toEqual(["Accounts", "Our Studio", "Data", "In no account", "store"]);
    expect(await screen.findByText("Main store")).toBeTruthy();
    expect(callsTo(conn, "accountRows")).toEqual(['builtin accountRows(accountId: "", concept: "v1:shopify:store", limit: 50)']);
  });

  it("says a refusal of the read in the engine's words, with a retry", async () => {
    const conn = connection({ accountUnplaced: () => new Error("the assignment has not started") });
    const data = await openData(conn, "Our Studio");
    expect(await within(data).findByText("the assignment has not started")).toBeTruthy();
    expect(within(data).queryByRole("region", { name: "In no account" })).toBeNull();
  });
});

describe("without the move part", () => {
  beforeEach(() => setEffectiveCapabilities(seededAccessWithout("owner", "app:accounts/move")));

  it("keeps the inventory as counts: a kind's line does not open", async () => {
    const conn = connection({ accountRows: () => [] });
    const data = await openData(conn);
    fireEvent.click(await within(data).findByRole("button", { name: /^Files, / }));
    expect(within(data).getByText("artifact")).toBeTruthy();
    expect(within(data).queryByRole("button", { name: "artifact, 3 rows" })).toBeNull();
    expect(callsTo(conn, "accountRows")).toHaveLength(0);
  });

  it("draws nothing that awaits a decision or is in no account, and asks nothing", async () => {
    const conn = connection({ accountUnplaced: () => [] }, {
      accountAssignmentStatus: [
        { kind: "switch", enabled: true, held: false },
        { kind: "conflict", concept: "v1:shopify:store", rowId: "store-7", conflict: "store_bound_twice", detail: "Bound twice", foundAt: "2026-11-12T09:00:00Z" },
      ],
    });
    const data = await openData(conn, "Our Studio");
    // The existing-data panel keeps listing the conflict, without acts.
    expect(await within(data).findByText("Needs a decision")).toBeTruthy();
    expect(within(data).queryByRole("region", { name: "Awaiting a decision" })).toBeNull();
    expect(within(data).queryByRole("region", { name: "In no account" })).toBeNull();
    expect(within(data).queryByRole("button", { name: /^Place / })).toBeNull();
    expect(within(data).queryByRole("button", { name: /^Leave out / })).toBeNull();
    expect(callsTo(conn, "accountUnplaced")).toHaveLength(0);
  });
});

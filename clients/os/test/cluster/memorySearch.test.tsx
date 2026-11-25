import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ModelAssignmentBoard } from "../../src/apps/fleet/machines/ModelAssignmentBoard";
import { assignmentProposal, modelSetupFromRow } from "../../src/apps/fleet/machines/modelSetup";
import { ConnectionStatusContext } from "../../src/chrome/connection";
import { sectionsFor } from "../../src/system/registry";
import { readTheBar } from "../readTheBar";
import { installSeededAccess } from "../seededAccess";

// Cluster > Settings > Memory search (design record 2026-11-23, section 8):
// the model memory search uses, whose machines index it, and the owner's acts
// on it -- change, cancel, switch anyway, switch back, confirm, take over. The
// reads and acts are the six memorySearch* builtins
// (component/memql/memory_search_builtins.go); this suite answers them with
// rows in the wire's own shape and presses what a person presses.

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));
vi.mock("../../src/kit", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../src/kit")>()),
  useAppReach: () => ({ open: vi.fn(), sections: [] }),
}));

const { ClusterApp } = await import("../../src/apps/cluster/ClusterApp");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { rowsResult, withSession } = await import("./harness");

const QWEN = "fleet:qwen3-embedding:0.6b";
const NOMIC = "fleet:nomic-embed-text";
const VENDOR = "embedding3Small";

/** The status row as the engine answers it: indexedBy is BARE on the wire,
 *  the session's id canonical, so "your machines" needs the two compared. */
function statusRow(over: Partial<Row> = {}): Row {
  return {
    id: "current",
    providerRef: QWEN,
    kind: "machine",
    label: "qwen3-embedding:0.6b",
    indexedBy: "me",
    indexedByName: "Ana Owner",
    chosenBy: "me",
    needsConfirmation: false,
    previousRef: "",
    takeOver: false,
    indexed: 8310,
    backlog: 0,
    backlogCountedAt: "",
    waitingReason: "",
    switch: null,
    retained: [],
    ...over,
  };
}

function switchRun(over: Partial<Row> = {}): Row {
  return {
    state: "indexing",
    targetRef: NOMIC,
    targetLabel: "nomic-embed-text",
    counts: { total: 1300, done: 1200, skipped: 40, unsourced: 0, failed: 60 },
    startedAt: "2026-11-24T09:00:00Z",
    startedBy: "me",
    waitingReason: "",
    estimatedTotal: 8310,
    indexedBy: "me",
    indexedByName: "Ana Owner",
    takeOver: false,
    ...over,
  };
}

const CHOICES: Row[] = [
  { id: QWEN, providerRef: QWEN, kind: "machine", label: "qwen3-embedding:0.6b", machines: 2, paidByCluster: false, available: true, hint: "2 machines offering it are online" },
  { id: NOMIC, providerRef: NOMIC, kind: "machine", label: "nomic-embed-text", machines: 1, paidByCluster: false, available: true, hint: "1 machine offering it is online" },
  { id: VENDOR, providerRef: VENDOR, kind: "vendor", label: "text-embedding-3-small", machines: 0, paidByCluster: true, available: true, hint: "Paid by the cluster's vendor account" },
];

/** The six builtins. Each act answers the status after it, as memorySearchAct does. */
function memorySearchQuery(first: Row, after: Row = first) {
  let current = first;
  const answer = () => rowsResult([current]);
  const act = () => vi.fn(async (_args?: unknown) => { current = after; return answer(); });
  return {
    memorySearchStatus: vi.fn(async (_args?: unknown, _opts?: unknown) => answer()),
    memorySearchChoices: vi.fn(async (_args?: unknown, _opts?: unknown) => rowsResult(CHOICES)),
    memorySearchSwitch: act(),
    memorySearchCancel: act(),
    memorySearchSwitchAnyway: act(),
    memorySearchConfirm: act(),
    /** What the cluster holds from now on: somebody else acted, or the job moved on. */
    becomes: (next: Row) => { current = next; },
  };
}

/** The clock a test that waits for reads runs on: timers and Date, so the press hold (Date) moves with them. */
function fakeClock() {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "Date"] });
  return (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
}

type Query = ReturnType<typeof memorySearchQuery>;

function mount(section: string, query: Query, role = "owner") {
  h.connection = { query, subscriptions: null };
  return render(withSession(
    <ConnectionStatusContext.Provider value="connected">
      <ClusterApp sectionId={section} navigate={vi.fn()} askContext={vi.fn()} />
    </ConnectionStatusContext.Provider>,
    { role },
  ));
}

const settle = () => act(async () => { for (let i = 0; i < 6; i += 1) await Promise.resolve(); });

beforeEach(() => {
  localStorage.clear();
  installSeededAccess("owner");
});
afterEach(() => {
  cleanup();
  installSeededAccess("owner");
});

describe("Cluster > Settings > Memory search", () => {
  it("only an owner sees Memory search", async () => {
    const cluster = OS_REGISTRY.apps.find((app) => app.id === "cluster")!;
    expect(sectionsFor(cluster).map((s) => s.id)).toContain("memorysearch");

    // Settings links to it and shows the model in use; the Start page keeps
    // its own choices, which do not grow a Memory search.
    const owner = memorySearchQuery(statusRow());
    mount("settings", owner);
    const link = await screen.findByRole("button", { name: "Memory search: qwen3-embedding:0.6b" });
    expect(within(link).getByText("Indexed on your machines")).toBeTruthy();
    fireEvent.click(screen.getByRole("combobox", { name: "Open Cluster on" }));
    const starts = screen.getAllByRole("option").map((o) => o.textContent);
    expect(starts).toContain("Settings");
    expect(starts).not.toContain("Memory search");
    cleanup();

    // Nobody else holds the part: no section, no link, and nothing is read.
    installSeededAccess("developer");
    expect(sectionsFor(cluster).map((s) => s.id)).not.toContain("memorysearch");
    const developer = memorySearchQuery(statusRow());
    mount("settings", developer, "developer");
    await settle();
    expect(screen.queryByText("Memory search")).toBeNull();
    cleanup();
    // Reached anyway, by an old link, it says who chooses and reads nothing.
    mount("memorysearch", developer, "developer");
    await settle();
    expect(screen.getByText("A cluster owner chooses memory search's model.")).toBeTruthy();
    expect(developer.memorySearchStatus).not.toHaveBeenCalled();
    expect(developer.memorySearchChoices).not.toHaveBeenCalled();
  });

  it("it shows the model, the indexer and the backlog", async () => {
    mount("memorysearch", memorySearchQuery(statusRow({
      backlog: 37,
      waitingReason: "none of the indexer's machines offering qwen3-embedding:0.6b is online",
    })));
    const inUse = await screen.findByRole("region", { name: "In use" });
    expect(within(inUse).getByText("qwen3-embedding:0.6b")).toBeTruthy();
    expect(within(inUse).getByText("Indexed on your machines")).toBeTruthy();
    expect(within(inUse).getByText("8,310 items indexed")).toBeTruthy();
    expect(within(inUse).getByText("37 items waiting: none of your machines offering qwen3-embedding:0.6b is online")).toBeTruthy();
    cleanup();

    // Another owner's machines, named; a vendor model, paid by the cluster.
    mount("memorysearch", memorySearchQuery(statusRow({ indexedBy: "ana", indexedByName: "Ana Owner", backlog: 1,
      waitingReason: "none of the indexer's machines offering qwen3-embedding:0.6b is online" })));
    expect(await screen.findByText("Indexed on Ana Owner's machines")).toBeTruthy();
    expect(screen.getByText("1 item waiting: none of Ana Owner's machines offering qwen3-embedding:0.6b is online")).toBeTruthy();
    cleanup();
    mount("memorysearch", memorySearchQuery(statusRow({ providerRef: VENDOR, kind: "vendor", label: "text-embedding-3-small",
      indexedBy: "", indexedByName: "" })));
    expect(within(await screen.findByRole("region", { name: "In use" })).getByText("Paid by the cluster's vendor account")).toBeTruthy();
  });

  it("a change asks first, with the vendor cost line for a vendor model", async () => {
    const query = memorySearchQuery(statusRow({ backlog: 37 }));
    mount("memorysearch", query);
    const models = await screen.findByRole("radiogroup", { name: "Memory search model" });
    expect(within(models).getByRole("radio", { name: /qwen3-embedding:0\.6b/ }).getAttribute("aria-checked")).toBe("true");
    expect(within(models).getByRole("radio", { name: /nomic-embed-text/ }).textContent).toContain("1 machine");
    expect(within(models).getByRole("radio", { name: /text-embedding-3-small/ }).textContent).toContain("Paid by the cluster's vendor account");

    fireEvent.click(within(models).getByRole("radio", { name: /nomic-embed-text/ }));
    expect(screen.getByText("Everything is re-indexed in the background on your machines. New items are indexed the same way from now on. Search keeps using qwen3-embedding:0.6b until the rebuild finishes, then switches by itself. The current index is kept for 30 days in case you switch back.")).toBeTruthy();
    expect(query.memorySearchSwitch).not.toHaveBeenCalled();

    fireEvent.click(within(models).getByRole("radio", { name: /text-embedding-3-small/ }));
    expect(screen.getByText("Everything is re-indexed in the background on the cluster's vendor account, about 8,347 items. New items are indexed the same way from now on. Search keeps using qwen3-embedding:0.6b until the rebuild finishes, then switches by itself. The current index is kept for 30 days in case you switch back.")).toBeTruthy();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Keep qwen3-embedding:0.6b" }));
    expect(screen.queryByText(/Everything is re-indexed/)).toBeNull();
    expect(query.memorySearchSwitch).not.toHaveBeenCalled();

    fireEvent.click(within(models).getByRole("radio", { name: /nomic-embed-text/ }));
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Switch to nomic-embed-text" }));
    await settle();
    expect(query.memorySearchSwitch).toHaveBeenCalledTimes(1);
    expect(query.memorySearchSwitch).toHaveBeenCalledWith({ provider: NOMIC });
  });

  it("progress shows while a switch runs, and Cancel cancels", async () => {
    const query = memorySearchQuery(statusRow({ switch: switchRun() }), statusRow({ switch: switchRun({ state: "cancelled" }) }));
    mount("memorysearch", query);
    expect(await screen.findByText("Re-indexing 1,240 of 8,310 items. Search uses qwen3-embedding:0.6b until this finishes.")).toBeTruthy();
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("15");
    expect(screen.getByText("nomic-embed-text")).toBeTruthy();
    // One switch at a time: no other model is offered while this one runs.
    expect(screen.queryByRole("radiogroup", { name: "Memory search model" })).toBeNull();

    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await settle();
    expect(query.memorySearchCancel).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(await screen.findByRole("radiogroup", { name: "Memory search model" })).toBeTruthy();
  });

  it("never shows more done than the estimated total", async () => {
    mount("memorysearch", memorySearchQuery(statusRow({ switch: switchRun({
      state: "catchingUp", counts: { total: 9000, done: 700, skipped: 8300, unsourced: 0, failed: 0 }, estimatedTotal: 8310,
    }) })));
    expect(await screen.findByText("Re-indexing 8,310 of 8,310 items. Search uses qwen3-embedding:0.6b until this finishes.")).toBeTruthy();
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("100");
  });

  it("a held switch offers Switch anyway and Cancel", async () => {
    const query = memorySearchQuery(statusRow({ switch: switchRun({
      state: "needsDecision", counts: { total: 8310, done: 8305, skipped: 0, unsourced: 2, failed: 3 },
    }) }));
    mount("memorysearch", query);
    expect(await screen.findByText("5 items could not be indexed (3 failed, 2 cannot be rebuilt).")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(screen.queryByRole("progressbar")).toBeNull();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Switch anyway" }));
    await settle();
    expect(query.memorySearchSwitchAnyway).toHaveBeenCalledTimes(1);
    expect(query.memorySearchCancel).not.toHaveBeenCalled();
  });

  it("Switch back appears while the previous index is kept", async () => {
    const kept = { providerRef: NOMIC, retireAt: "2026-12-24T12:00:00Z" };
    const done = switchRun({ state: "done", targetRef: QWEN, targetLabel: "qwen3-embedding:0.6b" });
    const query = memorySearchQuery(statusRow({ previousRef: NOMIC, retained: [kept], switch: done }));
    mount("memorysearch", query);
    expect(await screen.findByText(/^The previous model's index is kept until Dec 24, 2026\.$/)).toBeTruthy();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Switch back" }));
    expect(screen.getByText(/^Everything is re-indexed in the background on your machines\./)).toBeTruthy();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Switch to nomic-embed-text" }));
    await settle();
    expect(query.memorySearchSwitch).toHaveBeenCalledWith({ provider: NOMIC });
    cleanup();

    // A cancelled switch's partial index is kept too, and was never in use.
    mount("memorysearch", memorySearchQuery(statusRow({ previousRef: "", retained: [kept], switch: switchRun({ state: "cancelled" }) })));
    await screen.findByRole("radiogroup", { name: "Memory search model" });
    expect(screen.queryByText(/previous model's index/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Switch back" })).toBeNull();
    cleanup();

    // While another switch runs, switching back is not on offer.
    mount("memorysearch", memorySearchQuery(statusRow({ previousRef: NOMIC, retained: [kept], switch: switchRun({ targetRef: VENDOR, targetLabel: "text-embedding-3-small" }) })));
    await screen.findByRole("progressbar");
    expect(screen.queryByRole("button", { name: "Switch back" })).toBeNull();
  });

  it("a vendor binding picked before asks once", async () => {
    const vendor = { providerRef: VENDOR, kind: "vendor", label: "text-embedding-3-small", indexedBy: "", indexedByName: "", chosenBy: "" };
    const query = memorySearchQuery(statusRow({ ...vendor, needsConfirmation: true }), statusRow({ ...vendor, chosenBy: "me", needsConfirmation: false }));
    mount("memorysearch", query);
    expect(await screen.findByText("This cluster uses text-embedding-3-small, paid by the cluster's vendor account. Keep it, or choose a model on your machines.")).toBeTruthy();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Keep" }));
    await settle();
    expect(query.memorySearchConfirm).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/Keep it, or choose a model on your machines/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Keep" })).toBeNull();
  });

  it("offers the take-over only when the status says choosing again takes over", async () => {
    const query = memorySearchQuery(statusRow({ indexedBy: "ana", indexedByName: "Ana Owner", takeOver: true }),
      statusRow({ indexedBy: "me", takeOver: false }));
    mount("memorysearch", query);
    // Said without blaming the machines: a demoted indexer is a cause too.
    expect(await screen.findByText("Ana Owner can no longer index it, so search and new items wait.")).toBeTruthy();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Index on my machines" }));
    await settle();
    expect(query.memorySearchSwitch).toHaveBeenCalledWith({ provider: QWEN });
    expect(screen.queryByRole("button", { name: "Index on my machines" })).toBeNull();
    expect(screen.getByText("Indexed on your machines")).toBeTruthy();
    cleanup();

    // A switch in progress still offers it: search embeds as the binding's indexer until the flip.
    mount("memorysearch", memorySearchQuery(statusRow({ indexedBy: "ana", indexedByName: "Ana Owner", takeOver: true, switch: switchRun() })));
    expect(await screen.findByRole("button", { name: "Index on my machines" })).toBeTruthy();
    cleanup();

    mount("memorysearch", memorySearchQuery(statusRow({ indexedBy: "ana", indexedByName: "Ana Owner", takeOver: false })));
    await screen.findByText("Indexed on Ana Owner's machines");
    expect(screen.queryByRole("button", { name: "Index on my machines" })).toBeNull();
  });

  it("a switch says its wait as the reader, naming whose machines the rebuild waits for", async () => {
    // The job writes "the indexer's machines"; the rebuild embeds as the RUN's
    // indexer, who need not be the binding's (statusRow's is the reader).
    const offline = "none of the indexer's machines offering nomic-embed-text is online";
    mount("memorysearch", memorySearchQuery(statusRow({ switch: switchRun({ state: "waiting", waitingReason: offline, indexedBy: "me", indexedByName: "Ana Owner" }) })));
    expect(await screen.findByText("Waiting: none of your machines offering nomic-embed-text is online.")).toBeTruthy();
    cleanup();

    mount("memorysearch", memorySearchQuery(statusRow({ switch: switchRun({ state: "waiting", waitingReason: offline, indexedBy: "bo", indexedByName: "Bo Owner" }) })));
    expect(await screen.findByText("Waiting: none of Bo Owner's machines offering nomic-embed-text is online.")).toBeTruthy();
  });

  it("a switch whose own indexer cannot index offers the take-over: choosing its target again", async () => {
    const stuck = switchRun({ state: "waiting", indexedBy: "bo", indexedByName: "Bo Owner", takeOver: true,
      waitingReason: "the owner who chose this model is no longer an owner; an owner can choose again" });
    const query = memorySearchQuery(statusRow({ switch: stuck }),
      statusRow({ switch: switchRun({ indexedBy: "me", indexedByName: "Ana Owner", takeOver: false }) }));
    mount("memorysearch", query);
    expect(await screen.findByText("Bo Owner can no longer index nomic-embed-text, so the switch waits.")).toBeTruthy();
    // The binding's own take-over is a different act, and is not offered here.
    expect(screen.queryByRole("button", { name: "Index on my machines" })).toBeNull();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Re-index on my machines" }));
    await settle();
    expect(query.memorySearchSwitch).toHaveBeenCalledTimes(1);
    expect(query.memorySearchSwitch).toHaveBeenCalledWith({ provider: NOMIC });
    expect(screen.queryByRole("button", { name: "Re-index on my machines" })).toBeNull();
    // Cancel is still the switch's way out.
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
    cleanup();

    // Only where the status says so.
    mount("memorysearch", memorySearchQuery(statusRow({ switch: switchRun({ indexedBy: "bo", indexedByName: "Bo Owner", takeOver: false }) })));
    await screen.findByRole("progressbar");
    expect(screen.queryByRole("button", { name: "Re-index on my machines" })).toBeNull();
  });

  it("reads the status every 5 s while a switch runs, and every 30 s otherwise", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    const advance = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
    try {
      const running = memorySearchQuery(statusRow({ switch: switchRun() }));
      mount("memorysearch", running);
      await advance(0);
      const seen = running.memorySearchStatus.mock.calls.length;
      await advance(5_000);
      expect(running.memorySearchStatus.mock.calls.length).toBe(seen + 1);
      cleanup();

      const idle = memorySearchQuery(statusRow());
      mount("memorysearch", idle);
      await advance(0);
      const start = idle.memorySearchStatus.mock.calls.length;
      await advance(5_000);
      expect(idle.memorySearchStatus.mock.calls.length).toBe(start);
      await advance(25_000);
      expect(idle.memorySearchStatus.mock.calls.length).toBe(start + 1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("says a refusal in the engine's words and reads the status again", async () => {
    const query = memorySearchQuery(statusRow());
    query.memorySearchSwitch.mockRejectedValueOnce(new Error("memorySearchSwitch: memory search changed meanwhile, try again"));
    mount("memorysearch", query);
    fireEvent.click(within(await screen.findByRole("radiogroup", { name: "Memory search model" })).getByRole("radio", { name: /nomic-embed-text/ }));
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Switch to nomic-embed-text" }));
    expect(await screen.findByText("memory search changed meanwhile, try again")).toBeTruthy();
    await waitFor(() => expect(query.memorySearchStatus.mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  // A switch started elsewhere, then cancelled: the reads a page left open
  // through an afternoon can bring.
  const elsewhere = switchRun({ targetRef: VENDOR, targetLabel: "text-embedding-3-small" });
  const cancelled = switchRun({ state: "cancelled", targetRef: VENDOR, targetLabel: "text-embedding-3-small" });
  const question = /^Everything is re-indexed/;

  it("a question the cluster has moved past is dropped, and does not come back with the choices", async () => {
    const advance = fakeClock();
    try {
      const query = memorySearchQuery(statusRow());
      mount("memorysearch", query);
      await advance(0);
      fireEvent.click(within(screen.getByRole("radiogroup", { name: "Memory search model" })).getByRole("radio", { name: /nomic-embed-text/ }));
      expect(screen.getByText(question)).toBeTruthy();

      query.becomes(statusRow({ switch: elsewhere }));
      await advance(30_000);
      expect(screen.getByRole("progressbar")).toBeTruthy();
      expect(screen.queryByText(question)).toBeNull();

      query.becomes(statusRow({ switch: cancelled }));
      await advance(5_000);
      const models = screen.getByRole("radiogroup", { name: "Memory search model" });
      expect(within(models).getByRole("radio", { name: /qwen3-embedding:0\.6b/ }).getAttribute("aria-checked")).toBe("true");
      expect(screen.queryByText(question)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("a refusal stands while the state it answered does, and goes with it", async () => {
    const advance = fakeClock();
    try {
      const query = memorySearchQuery(statusRow());
      query.memorySearchSwitch.mockRejectedValueOnce(new Error("memorySearchSwitch: memory search changed meanwhile, try again"));
      mount("memorysearch", query);
      await advance(0);
      fireEvent.click(within(screen.getByRole("radiogroup", { name: "Memory search model" })).getByRole("radio", { name: /nomic-embed-text/ }));
      await readTheBar();
      fireEvent.click(screen.getByRole("button", { name: "Switch to nomic-embed-text" }));
      await advance(0);
      // The read the refusal asks for finds the same state: the refusal stays, with its question.
      expect(screen.getByText("memory search changed meanwhile, try again")).toBeTruthy();
      expect(screen.getByText(question)).toBeTruthy();

      query.becomes(statusRow({ switch: elsewhere }));
      await advance(30_000);
      expect(screen.queryByText("memory search changed meanwhile, try again")).toBeNull();

      query.becomes(statusRow({ switch: cancelled }));
      await advance(5_000);
      expect(screen.queryByText("memory search changed meanwhile, try again")).toBeNull();
      expect(screen.queryByText(question)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("Change model is one place: a refusal is said there whichever of its states it arrives in", async () => {
    const advance = fakeClock();
    try {
      const flipped = statusRow({ providerRef: NOMIC, label: "nomic-embed-text", switch: switchRun({ state: "finishing" }) });
      const query = memorySearchQuery(statusRow({ switch: switchRun() }));
      let refuse: (e: Error) => void = () => {};
      query.memorySearchCancel.mockImplementationOnce(() => new Promise((_resolve, reject) => { refuse = reject; }));
      mount("memorysearch", query);
      await advance(0);
      await readTheBar();
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
      // The switch flips while the Cancel is out: the section shows the choices.
      query.becomes(flipped);
      await advance(5_000);
      expect(screen.getByRole("radiogroup", { name: "Memory search model" })).toBeTruthy();
      const why = "the switch to nomic-embed-text has already happened and search uses it now; switching back is a new switch, and the previous model's index is kept for 30 days";
      await act(async () => { refuse(new Error(`memorySearchCancel: ${why}`)); });
      await advance(0);
      expect(screen.getByText(why)).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("Fleet's model board", () => {
  const GiB = 2 ** 30;
  const view = modelSetupFromRow({
    id: "machine", revision: "one", usableBytes: 32 * GiB, boundEmbeddingModel: "embed",
    choices: { embeddings: "embed" }, suggestedChoices: { embeddings: "embed" },
    catalog: [
      { modelId: "embed", installed: true, memoryBytes: 2 * GiB, purposes: ["embeddings"] },
      { modelId: "new-embed", installed: true, memoryBytes: 3 * GiB, purposes: ["embeddings"] },
    ],
  });
  const board = () => render(<ModelAssignmentBoard view={view} choices={view.choices} saving={false} canSave onChange={vi.fn()} onSave={vi.fn()} onDiscard={vi.fn()} />);

  it("the Fleet board points to Cluster settings", () => {
    const sentence = "Memory search keeps embed. Change it in Cluster > Settings > Memory search.";
    board();
    expect(screen.getByText(sentence)).toBeTruthy();
    expect(assignmentProposal(view.choices, "new-embed", ["embeddings"], view).refusal).toBe(sentence);
  });

  it("points there only for whoever can open it; anybody else is told who can change it (ruling R15)", () => {
    installSeededAccess("developer");
    const sentence = "Memory search keeps embed. A cluster owner can change it.";
    board();
    expect(screen.getByText(sentence)).toBeTruthy();
    expect(screen.queryByText(/Cluster > Settings > Memory search/)).toBeNull();
    expect(assignmentProposal(view.choices, "new-embed", ["embeddings"], view).refusal).toBe(sentence);
  });
});

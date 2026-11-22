import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { readTheBarNow } from "../readTheBar";

// Fleet > API keys > a key's page (design record
// 2026-11-21-vendor-accounts-and-api-keys, U3): what the cluster knows about
// the key, what its owner may change, who it is lent to, and the state with
// Check now in the one action bar. An act a person may not take is absent,
// never disabled. Every key here is a fixture.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { activityRow, directoryRow, droppedConnection, keyCard, keysConnection, lentCard, renderKeys, settle } = await import("./keysFixtures");

afterEach(cleanup);
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-22T12:00:00Z"));
});
afterEach(() => {
  vi.useRealTimers();
});

type Seed = Parameters<typeof keysConnection>[0];

function use(seed: Seed = {}) {
  const made = keysConnection(seed);
  h.connection = made.connection;
  return made;
}

const bar = () => screen.getByRole("group", { name: "What you can do with this" });
const barActs = () => within(bar()).queryAllByRole("button").map((b) => b.textContent?.trim() ?? "");

async function pressBar(name: string | RegExp) {
  readTheBarNow();
  fireEvent.click(within(bar()).getByRole("button", { name }));
  await settle();
}

async function open(name: string) {
  fireEvent.click(within(screen.getByRole("list", { name: "API keys" })).getByRole("button", { name: new RegExp(`^Open ${name}\\b`) }));
  await settle();
}

/** A fact's value, by its label. */
function fact(label: string): string {
  const term = screen.getAllByText(label).find((node) => node.tagName === "DT");
  if (term === undefined) throw new Error(`no fact labelled ${label}`);
  return term.parentElement?.querySelector("dd")?.textContent?.trim() ?? "";
}

const section = (name: string) => screen.getByRole("region", { name });

/** Whether a switch is on. A switch is a native checkbox, so its state is its `checked`. */
const isOn = (name: string) => (screen.getByRole("switch", { name }) as HTMLInputElement).checked;

describe("a person's own key", () => {
  it("opens in place of the list, under the trail back to it", async () => {
    use({ mine: [keyCard()] });
    await renderKeys("developer");
    await open("Personal");
    expect(screen.getByRole("heading", { name: "Personal" })).toBeTruthy();
    expect(screen.queryByRole("list", { name: "API keys" })).toBeNull();
    // Outside a window the heading draws its own way back.
    fireEvent.click(screen.getAllByRole("button", { name: "API keys" })[0]!);
    await settle();
    expect(screen.getByRole("list", { name: "API keys" })).toBeTruthy();
  });

  it("draws what the cluster knows about it, and never a value", async () => {
    use({
      mine: [keyCard({ lastUsedAt: "2026-11-22T11:00:00Z", checkedAt: "2026-11-22T04:40:00Z" })],
      activity: { k1: activityRow({ keyId: "k1", calls: 12, estimatedCost: 1.2, priced: true }) },
    });
    const view = await renderKeys("developer");
    await open("Personal");
    expect(fact("Vendor")).toBe("Anthropic");
    expect(fact("Fingerprint")).toContain("...a1b2");
    expect(fact("Added")).not.toBe("--");
    expect(fact("Last used")).toContain("1h ago");
    expect(fact("Last checked")).toContain("accepted");
    // The cost is this cluster's arithmetic, and says so.
    expect(fact("Estimated cost this month")).toBe("$1.20");
    expect(screen.getByText(/MemQL's estimate/)).toBeTruthy();
    expect(view.container.querySelector("input[type=password], .os-input-concealed")).toBeNull();
  });

  it("says a key nothing has used was not used, and one checked by use is checked by its calls", async () => {
    use({ mine: [keyCard({ vendor: "openai", lastUsedAt: "", checkedAt: "", checkedBy: "use", state: "unchecked" })] });
    await renderKeys("developer");
    await open("Personal");
    expect(fact("Last used")).toBe("Not used yet");
    expect(fact("Last checked")).toBe("Checked by the calls it pays for");
    expect(fact("Estimated cost this month")).toBe("Nothing yet");
    // Listing models is what a check is, and this key may not: no Check now.
    expect(barActs()).toEqual([]);
  });

  it("renames the key, and keeps the old name on a refusal", async () => {
    const made = use({ mine: [keyCard()] });
    await renderKeys("developer");
    await open("Personal");
    const name = screen.getByLabelText("Name for Personal") as HTMLInputElement;
    expect(within(section("Settings")).getByRole("button", { name: "Rename" }).hasAttribute("disabled")).toBe(true);
    fireEvent.change(name, { target: { value: "Work" } });
    fireEvent.click(within(section("Settings")).getByRole("button", { name: "Rename" }));
    await settle();
    expect(made.query.vendorKeyUpdate).toHaveBeenLastCalledWith({ keyId: "k1", label: "Work" });
    expect(screen.getByRole("heading", { name: "Work" })).toBeTruthy();

    made.fail("vendorKeyUpdate", "vendorKeyUpdate: You already have a key with that name.");
    fireEvent.change(screen.getByLabelText("Name for Work"), { target: { value: "Other" } });
    fireEvent.click(within(section("Settings")).getByRole("button", { name: "Rename" }));
    await settle();
    expect(screen.getByText("You already have a key with that name.")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Work" })).toBeTruthy();
  });

  it("offers the default only when there is another key at the vendor to choose between", async () => {
    use({ mine: [keyCard()] });
    const one = await renderKeys("developer");
    await open("Personal");
    expect(within(section("Settings")).queryByText("Default")).toBeNull();
    one.unmount();

    const made = use({ mine: [keyCard(), keyCard({ id: "k2", label: "Work", isDefault: false })] });
    await renderKeys("developer");
    await open("Work");
    fireEvent.click(within(section("Settings")).getByRole("button", { name: "Make this the default" }));
    await settle();
    expect(made.query.vendorKeyUpdate).toHaveBeenLastCalledWith({ keyId: "k2", isDefault: true });
    // It is the default now, which is said and is not a second button.
    expect(within(section("Settings")).queryByRole("button", { name: "Make this the default" })).toBeNull();
    expect(within(section("Settings")).getByText(/default for Anthropic/)).toBeTruthy();
  });

  it("leaves Auto off for a new key, and turns it on only when its owner does", async () => {
    const made = use({ mine: [keyCard()] });
    await renderKeys("developer");
    await open("Personal");
    expect(isOn("Let Auto use this key")).toBe(false);
    fireEvent.click(screen.getByRole("switch", { name: "Let Auto use this key" }));
    await settle();
    expect(made.query.vendorKeyUpdate).toHaveBeenLastCalledWith({ keyId: "k1", autoUse: true });
    expect(isOn("Let Auto use this key")).toBe(true);
  });

  it("removes the key only after asking, and says to delete it at the vendor too", async () => {
    const made = use({ mine: [keyCard()] });
    await renderKeys("developer");
    await open("Personal");
    fireEvent.click(screen.getByRole("button", { name: "Remove this key" }));
    const asking = screen.getByRole("group", { name: "Removing Personal" });
    expect(within(asking).getByText(/delete it at Anthropic too/i)).toBeTruthy();
    expect(made.query.vendorKeyRemove).not.toHaveBeenCalled();

    // Keeping it puts the control back and removes nothing.
    readTheBarNow();
    fireEvent.click(within(asking).getByRole("button", { name: "Keep it" }));
    expect(screen.queryByRole("group", { name: "Removing Personal" })).toBeNull();
    expect(made.query.vendorKeyRemove).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Remove this key" }));
    readTheBarNow();
    fireEvent.click(within(screen.getByRole("group", { name: "Removing Personal" })).getByRole("button", { name: "Remove Personal" }));
    await settle();
    expect(made.query.vendorKeyRemove).toHaveBeenLastCalledWith({ keyId: "k1" });
    // Gone, and back at the list.
    expect(screen.getByRole("region", { name: "No API keys" })).toBeTruthy();
  });

  it("keeps the question open with the reason when a removal is refused", async () => {
    const made = use({ mine: [keyCard()] });
    made.fail("vendorKeyRemove", "vendorKeyRemove: Your keys are being changed from somewhere else at this moment. Try again.");
    await renderKeys("developer");
    await open("Personal");
    fireEvent.click(screen.getByRole("button", { name: "Remove this key" }));
    readTheBarNow();
    fireEvent.click(within(screen.getByRole("group", { name: "Removing Personal" })).getByRole("button", { name: "Remove Personal" }));
    await settle();
    expect(screen.getByText("Your keys are being changed from somewhere else at this moment. Try again.")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Personal" })).toBeTruthy();
  });
});

describe("the state, and checking now", () => {
  it("stands in the one action bar with Check now, and records what the vendor said", async () => {
    const made = use({ mine: [keyCard({ state: "refused", detail: "invalid x-api-key" })] });
    await renderKeys("developer");
    await open("Personal");
    expect(within(bar()).getByText("Refused")).toBeTruthy();
    expect(within(bar()).getByText(/invalid x-api-key/)).toBeTruthy();
    expect(barActs()).toEqual(["Check now"]);

    made.state.nextChecked = { state: "good", detail: "" };
    await pressBar("Check now");
    expect(made.query.vendorKeyCheck).toHaveBeenLastCalledWith({ keyId: "k1" });
    expect(within(bar()).getByText("Working")).toBeTruthy();
    expect(within(bar()).queryByText(/invalid x-api-key/)).toBeNull();
  });

  it("draws the check as the cluster's turn, with no act to press", async () => {
    const made = use({ mine: [keyCard()] });
    made.hold("vendorKeyCheck");
    await renderKeys("developer");
    await open("Personal");
    await pressBar("Check now");
    expect(within(bar()).getByText("Checking with Anthropic")).toBeTruthy();
    expect(barActs()).toEqual([]);
    await made.release("vendorKeyCheck");
    expect(barActs()).toEqual(["Check now"]);
  });

  it("says when a check decided nothing, and changes nothing it knew", async () => {
    const made = use({ mine: [keyCard()] });
    made.state.nextChecked = { state: "good", detail: "", note: "Anthropic could not be reached to check the key. Try again in a moment." };
    await renderKeys("developer");
    await open("Personal");
    await pressBar("Check now");
    expect(within(bar()).getByText("Working")).toBeTruthy();
    expect(screen.getByText("Anthropic could not be reached to check the key. Try again in a moment.")).toBeTruthy();
  });

  it("says a key out of credit is waiting for credit, in the vendor's words", async () => {
    use({ mine: [keyCard({ state: "credit", detail: "Your credit balance is too low", heldUntil: "2026-11-22T12:40:00Z" })] });
    await renderKeys("developer");
    await open("Personal");
    expect(within(bar()).getByText("Out of credit")).toBeTruthy();
    expect(within(bar()).getByText(/Your credit balance is too low/)).toBeTruthy();
  });

  it("puts a dropped connection in words of its own", async () => {
    const made = use({ mine: [keyCard()] });
    made.fail("vendorKeyCheck", droppedConnection());
    await renderKeys("developer");
    await open("Personal");
    await pressBar("Check now");
    expect(screen.getByText(/The connection dropped before the cluster answered/)).toBeTruthy();
    expect(screen.queryByText("stream closed")).toBeNull();
  });
});

describe("lending a key", () => {
  const people = directoryRow({
    people: [{ id: "ben", name: "Ben", detail: "ben@example.com" }, { id: "cy", name: "Cy", detail: "cy@example.com" }],
    groups: [{ id: "design", name: "Design", members: 3 }],
  });

  it("keeps the key to its owner until they lend it, and shows no audience while it is off", async () => {
    use({ mine: [keyCard()], directory: people });
    await renderKeys("developer");
    await open("Personal");
    expect(isOn("Lend this key")).toBe(false);
    expect(screen.queryByRole("region", { name: "Users and groups" })).toBeNull();
  });

  it("turns lending on, adds a person, and sends exactly who it is lent to", async () => {
    const made = use({ mine: [keyCard()], directory: people });
    await renderKeys("developer");
    await open("Personal");
    fireEvent.click(screen.getByRole("switch", { name: "Lend this key" }));
    await settle();
    expect(made.query.vendorKeySetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ keyId: "k1", mode: "people", paused: false, userIds: [], groupIds: [] }));
    expect(screen.getByRole("region", { name: "Users and groups" })).toBeTruthy();

    const search = screen.getByRole("combobox", { name: "Search users and groups" });
    fireEvent.focus(search);
    fireEvent.click(screen.getByRole("option", { name: "Ben" }));
    fireEvent.click(screen.getByRole("button", { name: "Add selected users and groups" }));
    await settle();
    expect(made.query.vendorKeySetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ keyId: "k1", mode: "people", userIds: ["ben"], groupIds: [] }));
  });

  it("shows who it is lent to with what each used this month, and pauses one of them", async () => {
    const made = use({
      mine: [keyCard({ lentTo: 2, sharing: { mode: "people", paused: false, userIds: ["ben"], groupIds: ["design"], pausedUserIds: [], pausedGroupIds: [] } })],
      directory: directoryRow({
        people: [{ id: "ben", name: "Ben", detail: "" }],
        groups: [{ id: "design", name: "Design", members: 3 }],
        current: { people: [{ id: "ben", name: "Ben", known: true, inDirectory: true }], groups: [{ id: "design", name: "Design", known: true, inDirectory: true }] },
      }),
      activity: { k1: activityRow({ keyId: "k1", calls: 7, estimatedCost: 0.7, people: [{ userId: "ben", name: "Ben", mine: false, calls: 5, estimatedCost: 0.5 }, { userId: "me", name: "Me", mine: true, calls: 2, estimatedCost: 0.2 }] }) },
    });
    await renderKeys("developer");
    await open("Personal");
    expect(isOn("Lend this key")).toBe(true);
    const cards = screen.getByRole("region", { name: "Users and groups" });
    expect(within(cards).getByText("Ben")).toBeTruthy();
    expect(within(cards).getByText("5 calls this month")).toBeTruthy();

    fireEvent.click(within(cards).getByRole("button", { name: "Pause lending to Ben" }));
    await settle();
    expect(made.query.vendorKeySetSharing).toHaveBeenLastCalledWith(
      expect.objectContaining({ keyId: "k1", mode: "people", userIds: ["ben"], groupIds: ["design"], pausedUserIds: ["ben"] }),
    );
  });

  it("turning lending off pauses it and keeps the list", async () => {
    const made = use({
      mine: [keyCard({ lentTo: 1, sharing: { mode: "people", paused: false, userIds: ["ben"], groupIds: [], pausedUserIds: [], pausedGroupIds: [] } })],
      directory: directoryRow({ people: [{ id: "ben", name: "Ben", detail: "" }], current: { people: [{ id: "ben", name: "Ben", known: true, inDirectory: true }], groups: [] } }),
    });
    await renderKeys("developer");
    await open("Personal");
    fireEvent.click(screen.getByRole("switch", { name: "Lend this key" }));
    await settle();
    expect(made.query.vendorKeySetSharing).toHaveBeenLastCalledWith(expect.objectContaining({ keyId: "k1", mode: "people", paused: true, userIds: ["ben"] }));
  });

  it("says why lending was not saved, in the cluster's words", async () => {
    const made = use({ mine: [keyCard()], directory: people });
    made.fail("vendorKeySetSharing", "vendorKeySetSharing: Ben is not somebody you can lend this key to.");
    await renderKeys("developer");
    await open("Personal");
    fireEvent.click(screen.getByRole("switch", { name: "Lend this key" }));
    await settle();
    expect(screen.getByText("Lending could not be updated.")).toBeTruthy();
    expect(screen.getByText("Ben is not somebody you can lend this key to.")).toBeTruthy();
  });
});

describe("somebody else's key", () => {
  it("shows a key lent to the reader as a short card, with no act at all", async () => {
    const made = use({ lent: [lentCard()] });
    const view = await renderKeys("developer");
    await open("Team");
    expect(screen.getByRole("heading", { name: "Team" })).toBeTruthy();
    expect(fact("Vendor")).toBe("OpenAI");
    expect(fact("Lent by")).toBe("Ben");
    expect(within(bar()).getByText("Working")).toBeTruthy();
    expect(barActs()).toEqual([]);
    expect(screen.queryByRole("region", { name: "Settings" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Lending" })).toBeNull();
    expect(screen.queryByText("Fingerprint")).toBeNull();
    expect(view.container.querySelector("input, [role=switch]")).toBeNull();
    // Its activity is its owner's to read.
    expect(made.query.vendorKeyActivity).not.toHaveBeenCalled();
  });

  it("says a lent key is paused for the reader", async () => {
    use({ lent: [lentCard({ paused: true })] });
    await renderKeys("developer");
    await open("Team");
    expect(within(bar()).getByText("Paused")).toBeTruthy();
    expect(within(bar()).getByText(/Ben has paused lending it to you/)).toBeTruthy();
  });

  it("lets a cluster owner check and remove another person's key, and nothing else", async () => {
    const made = use({
      mine: [],
      all: [keyCard({ id: "k5", label: "Dana's", ownerUserId: "dana", ownerName: "Dana", relation: "other", lentTo: 2 })],
    });
    await renderKeys("owner");
    fireEvent.click(screen.getByRole("button", { name: "Find API keys" }));
    fireEvent.click(screen.getByRole("radio", { name: "Every key on this cluster" }));
    await settle();
    await open("Dana's");

    expect(fact("Owner")).toBe("Dana");
    expect(fact("Lent to")).toBe("2 people or groups");
    expect(barActs()).toEqual(["Check now"]);
    const settings = section("Settings");
    expect(within(settings).getByRole("button", { name: "Remove this key" })).toBeTruthy();
    // Not theirs to rename, make the default, let Auto spend or lend.
    expect(within(settings).queryByRole("button", { name: "Rename" })).toBeNull();
    expect(screen.queryByRole("switch", { name: "Let Auto use this key" })).toBeNull();
    expect(screen.queryByRole("switch", { name: "Lend this key" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Lending" })).toBeNull();
    // What it paid for is its owner's to read.
    expect(made.query.vendorKeyActivity).not.toHaveBeenCalled();
    expect(screen.queryByText("Estimated cost this month")).toBeNull();

    await pressBar("Check now");
    expect(made.query.vendorKeyCheck).toHaveBeenLastCalledWith({ keyId: "k5" });
  });
});

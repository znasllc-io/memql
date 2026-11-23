import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// Settings > API keys, the list (design record
// 2026-11-21-vendor-accounts-and-api-keys, U1): a person's own keys and the
// keys lent to them, as records that open; the one Add control; and for a
// cluster owner, every key on the cluster behind Refine. Every key here is a
// fixture.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { activityRow, keyCard, keysConnection, lentCard, renderKeys, settle } = await import("./apiKeysFixtures");
const { API_KEYS_ADD_PART } = await import("../../src/apps/settings/apikeys/ApiKeysSection");

afterEach(cleanup);

type Seed = Parameters<typeof keysConnection>[0];

function use(seed: Seed = {}) {
  const made = keysConnection(seed);
  h.connection = made.connection;
  return made;
}

const list = () => screen.getByRole("list", { name: "API keys" });
const rowOf = (name: string) => within(list()).getByRole("button", { name: new RegExp(`^Open ${name}\\b`) });

describe("reading the keys", () => {
  it("draws the list's shape while the read is out, and neither an empty list nor an error", async () => {
    const made = use({ mine: [keyCard()] });
    made.hold("vendorKeysForActor");
    const view = await renderKeys("developer");
    const skeleton = view.container.querySelector(".os-record-skeleton") as HTMLElement;
    expect(skeleton.getAttribute("role")).toBe("status");
    expect(within(skeleton).getByText("Loading API keys")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "No API keys" })).toBeNull();
    expect(screen.queryByText("Your API keys could not be read.")).toBeNull();
    await made.release("vendorKeysForActor");
    expect(rowOf("Personal")).toBeTruthy();
    expect(view.container.querySelector(".os-record-skeleton")).toBeNull();
  });

  it("says a person has no keys only after an empty read", async () => {
    use();
    await renderKeys("developer");
    expect(screen.getByRole("region", { name: "No API keys" })).toBeTruthy();
    expect(screen.queryByText("Your API keys could not be read.")).toBeNull();
  });

  it("says a read failed in the cluster's own words, which is not an empty list", async () => {
    const made = use({ mine: [keyCard()] });
    made.fail("vendorKeysForActor", "vendorKeysForActor: That did not complete. Reload the list to see where it stands, and try again.");
    await renderKeys("developer");
    expect(screen.getByText("Your API keys could not be read.")).toBeTruthy();
    expect(screen.getByText("That did not complete. Reload the list to see where it stands, and try again.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "No API keys" })).toBeNull();
  });

  it("keeps the keys on screen when a later read fails, and says updates are interrupted", async () => {
    const made = use({ mine: [keyCard()] });
    const view = await renderKeys("developer");
    expect(rowOf("Personal")).toBeTruthy();
    made.fail("vendorKeysForActor", "vendorKeysForActor: stream closed");
    await view.show(false);
    await view.show(true);
    expect(rowOf("Personal")).toBeTruthy();
    expect(screen.getByText("API key updates are interrupted.")).toBeTruthy();
    expect(screen.queryByText("Your API keys could not be read.")).toBeNull();
  });

  it("reads again when the section is shown again", async () => {
    const made = use({ mine: [keyCard()] });
    const view = await renderKeys("developer");
    const before = made.query.vendorKeysForActor.mock.calls.length;
    made.state.mine = [keyCard(), keyCard({ id: "k2", label: "Work", isDefault: false })];
    await view.show(false);
    await view.show(true);
    expect(made.query.vendorKeysForActor.mock.calls.length).toBeGreaterThan(before);
    expect(rowOf("Work")).toBeTruthy();
  });
});

describe("a key's row", () => {
  it("names the key, its vendor and fingerprint, how many it is lent to and what it cost this month", async () => {
    use({
      mine: [keyCard({ lentTo: 2 }), keyCard({ id: "k2", vendor: "openai", label: "Work", fingerprint: "...c3d4" })],
      activity: { k1: activityRow({ keyId: "k1", calls: 12, estimatedCost: 1.2, priced: true }), k2: activityRow({ keyId: "k2", calls: 3, estimatedCost: 0, priced: false }) },
    });
    await renderKeys("developer");
    const personal = rowOf("Personal");
    expect(within(personal).getByText("Personal")).toBeTruthy();
    expect(within(personal).getByText("Anthropic · ...a1b2")).toBeTruthy();
    expect(within(personal).getByText("Lent to 2")).toBeTruthy();
    expect(within(personal).getByText("Estimated $1.20 this month")).toBeTruthy();
    const work = rowOf("Work");
    expect(within(work).getByText("OpenAI · ...c3d4")).toBeTruthy();
    // A model that was called has no price listed here: the cost is not a zero.
    expect(within(work).getByText("Not priced")).toBeTruthy();
    expect(within(work).queryByText(/Lent to/)).toBeNull();
  });

  it("says a partly priced month is a floor, and a month with no calls is not a zero", async () => {
    use({
      mine: [keyCard(), keyCard({ id: "k2", label: "Work", isDefault: false })],
      activity: { k1: activityRow({ keyId: "k1", calls: 9, estimatedCost: 0.4, priced: false }), k2: activityRow({ keyId: "k2", calls: 0, estimatedCost: 0, priced: true }) },
    });
    await renderKeys("developer");
    expect(within(rowOf("Personal")).getByText("Estimated $0.40 or more this month")).toBeTruthy();
    expect(within(rowOf("Work")).getByText("Not used this month")).toBeTruthy();
  });

  it("marks the default only when the person has more than one key at that vendor", async () => {
    use({ mine: [keyCard()] });
    const one = await renderKeys("developer");
    expect(within(rowOf("Personal")).queryByText("Default")).toBeNull();
    one.unmount();

    use({ mine: [keyCard(), keyCard({ id: "k2", label: "Work", isDefault: false }), keyCard({ id: "k3", vendor: "openai", label: "Other" })] });
    await renderKeys("developer");
    expect(within(rowOf("Personal")).getByText("Default")).toBeTruthy();
    expect(within(rowOf("Work")).queryByText("Default")).toBeNull();
    // The only key at its vendor is the default by construction: not said.
    expect(within(rowOf("Other")).queryByText("Default")).toBeNull();
  });

  it("draws no state when every key is working, each state when they differ, and a broken key always", async () => {
    use({ mine: [keyCard(), keyCard({ id: "k2", label: "Work", isDefault: false })] });
    const same = await renderKeys("developer");
    expect(within(list()).queryByText("Working")).toBeNull();
    same.unmount();

    use({
      mine: [
        keyCard(),
        keyCard({ id: "k2", label: "Work", isDefault: false, state: "refused", detail: "invalid x-api-key" }),
        keyCard({ id: "k3", vendor: "openai", label: "Broke", state: "credit" }),
        keyCard({ id: "k4", vendor: "openai", label: "Restricted", isDefault: false, checkedBy: "use", state: "unchecked" }),
      ],
    });
    const mixed = await renderKeys("developer");
    expect(within(rowOf("Personal")).getByText("Working")).toBeTruthy();
    expect(within(rowOf("Work")).getByText("Refused")).toBeTruthy();
    expect(within(rowOf("Broke")).getByText("Out of credit")).toBeTruthy();
    expect(within(rowOf("Restricted")).getByText("Not checked yet")).toBeTruthy();
    mixed.unmount();

    // One key, and it is refused: nothing else on the page would say so.
    use({ mine: [keyCard({ state: "refused", detail: "invalid x-api-key" })] });
    await renderKeys("developer");
    expect(within(rowOf("Personal")).getByText("Refused")).toBeTruthy();
  });

  it("shows a key lent to the person with its lender, and none of its owner's facts", async () => {
    use({ mine: [keyCard()], lent: [lentCard(), lentCard({ id: "k8", label: "Shared", ownerName: "Cy", paused: true })] });
    await renderKeys("developer");
    const team = rowOf("Team");
    expect(within(team).getByText("OpenAI · lent by Ben")).toBeTruthy();
    expect(within(team).queryByText(/this month|Not priced|Lent to/)).toBeNull();
    // Its owner paused lending it to this person: that is why it is not paying.
    expect(within(rowOf("Shared")).getByText("Paused")).toBeTruthy();
  });

  it("is a flat record list with no badge, chip or refresh control", async () => {
    use({ mine: [keyCard({ lentTo: 2 }), keyCard({ id: "k2", label: "Work", isDefault: false, state: "refused" })], lent: [lentCard()] });
    const view = await renderKeys("developer");
    expect(list().classList.contains("os-record-list")).toBe(true);
    expect(view.container.querySelector(".os-record-list .os-chip, .os-record-list .os-badge, .os-record-list .os-tag")).toBeNull();
    expect(screen.queryByRole("button", { name: /refresh|reload/i })).toBeNull();
  });
});

describe("what a person may do", () => {
  it("offers the one Add control, the shared icon-only button, to somebody who may add a key", async () => {
    use({ mine: [keyCard()] });
    await renderKeys("developer");
    const add = screen.getByRole("button", { name: "Add an API key" });
    expect(add.classList.contains("os-add-button")).toBe(true);
    expect(add.textContent?.trim()).toBe("");
    expect(screen.getAllByRole("button", { name: "Add an API key" })).toHaveLength(1);
  });

  // Owner decision 2026-11-22: keys are bring-your-own for everybody. The
  // set each role holds is read from the seeds themselves (seededAccess.ts).
  it.each(["owner", "developer", "admin", "user", "viewer"])("offers the Add control to a signed-in %s", async (role) => {
    use();
    await renderKeys(role);
    expect(screen.getAllByRole("button", { name: "Add an API key" })).toHaveLength(1);
  });

  it("offers no Add control to somebody the part was withheld from, and still shows the keys they own and are lent", async () => {
    use({ mine: [keyCard()], lent: [lentCard()] });
    // An operator's deny of the part, as it resolves for this person. A key's
    // owner sees and opens their keys whatever they hold: only adding is gated.
    await renderKeys("user", {}, [API_KEYS_ADD_PART]);
    expect(screen.queryByRole("button", { name: "Add an API key" })).toBeNull();
    expect(rowOf("Personal")).toBeTruthy();
    expect(rowOf("Team")).toBeTruthy();
    fireEvent.click(rowOf("Personal"));
    await settle();
    expect(screen.getByRole("heading", { name: "Personal" })).toBeTruthy();
  });

  // The setup screen's "Add an API key" opens this section asking to add one
  // (owner decision 2026-11-23), so the act lands where it says.
  it("opens straight into adding a key when it was opened to add one", async () => {
    use();
    const consumeIntent = vi.fn();
    await renderKeys("user", { intent: { id: "intent-1", payload: { addKey: true } }, consumeIntent });
    expect(screen.getByRole("heading", { name: "Add an API key" })).toBeTruthy();
    expect(screen.getByRole("radiogroup", { name: "Vendor" })).toBeTruthy();
    expect(consumeIntent).toHaveBeenCalledWith("intent-1");
  });

  it("opens on the list for somebody withheld from adding, and still uses up the request", async () => {
    use();
    const consumeIntent = vi.fn();
    await renderKeys("user", { intent: { id: "intent-2", payload: { addKey: true } }, consumeIntent }, [API_KEYS_ADD_PART]);
    expect(screen.queryByRole("radiogroup", { name: "Vendor" })).toBeNull();
    expect(screen.getByRole("heading", { name: "API keys" })).toBeTruthy();
    expect(consumeIntent).toHaveBeenCalledWith("intent-2");
  });

  it("tells a person with no keys and no way to add one what they will see here", async () => {
    use();
    await renderKeys("user", {}, [API_KEYS_ADD_PART]);
    expect(screen.getByText("Keys that other people lend you appear here.")).toBeTruthy();
  });

  it("lets a cluster owner reach every key through Refine, and asks the cluster only then", async () => {
    const made = use({
      mine: [keyCard()],
      all: [keyCard(), keyCard({ id: "k5", label: "Dana's", ownerUserId: "dana", ownerName: "Dana", relation: "other", fingerprint: "...e5f6" })],
    });
    await renderKeys("owner");
    expect(made.query.vendorKeysForActor.mock.calls.every((call) => (call[0] as { all?: boolean }).all !== true)).toBe(true);
    expect(within(list()).queryByText("Dana's")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Find API keys" }));
    fireEvent.click(screen.getByRole("radio", { name: "Every key on this cluster" }));
    await settle();
    expect(made.query.vendorKeysForActor).toHaveBeenLastCalledWith({ all: true }, expect.anything());
    const danas = rowOf("Dana's");
    expect(within(danas).getByText("Anthropic · ...e5f6")).toBeTruthy();
    expect(within(danas).getByText("Dana")).toBeTruthy();
    // The owner's own key is in that list once, not twice.
    expect(within(list()).getAllByRole("button", { name: /^Open Personal\b/ })).toHaveLength(1);

    // The constraint is a removable chip, and removing it is the way back.
    fireEvent.click(screen.getByRole("button", { name: "Remove Every key on this cluster" }));
    await settle();
    expect(within(list()).queryByText("Dana's")).toBeNull();
  });

  // K5: a cluster owner can check and remove ANY key on their cluster. One
  // that is also lent to them arrives twice, as the short card of a borrower
  // and as the full card of a cluster owner, and the fuller one is the one
  // they asked for: the short card has no Check now and no Remove.
  it("gives a cluster owner the full card of a key that is also lent to them", async () => {
    use({
      mine: [keyCard()],
      lent: [lentCard({ id: "k9", label: "Team" })],
      all: [keyCard(), keyCard({ id: "k9", label: "Team", vendor: "openai", fingerprint: "...9z9z", ownerUserId: "ben", ownerName: "Ben", relation: "lent", isDefault: true })],
    });
    await renderKeys("owner");
    // Before asking, it is a key lent to them like any other.
    expect(within(rowOf("Team")).getByText("OpenAI · lent by Ben")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Find API keys" }));
    fireEvent.click(screen.getByRole("radio", { name: "Every key on this cluster" }));
    await settle();
    expect(within(list()).getAllByRole("button", { name: /^Open Team\b/ })).toHaveLength(1);
    expect(within(rowOf("Team")).getByText("OpenAI · ...9z9z")).toBeTruthy();

    fireEvent.click(within(list()).getByRole("button", { name: /^Open Team\b/ }));
    await settle();
    const acts = within(screen.getByRole("group", { name: "What you can do with this" })).queryAllByRole("button").map((b) => b.textContent?.trim() ?? "");
    expect(acts).toEqual(["Check now"]);
    expect(screen.getByRole("button", { name: "Remove this key" })).toBeTruthy();
  });

  it("offers a developer no way to every key", async () => {
    use({ mine: [keyCard()] });
    await renderKeys("developer");
    expect(screen.queryByRole("button", { name: "Find API keys" })).toBeNull();
  });

  it("narrows a cluster owner's list by a key's name, its owner or its vendor", async () => {
    use({
      mine: [keyCard()],
      all: [keyCard(), keyCard({ id: "k5", label: "Staging", vendor: "openai", ownerUserId: "dana", ownerName: "Dana", relation: "other" })],
    });
    await renderKeys("owner");
    fireEvent.click(screen.getByRole("button", { name: "Find API keys" }));
    fireEvent.click(screen.getByRole("radio", { name: "Every key on this cluster" }));
    await settle();
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "dana" } });
    expect(within(list()).queryByRole("button", { name: /^Open Personal\b/ })).toBeNull();
    expect(rowOf("Staging")).toBeTruthy();
    fireEvent.change(screen.getByRole("textbox", { name: "Search" }), { target: { value: "nothing like it" } });
    expect(screen.getByRole("region", { name: "No matching keys" })).toBeTruthy();
  });
});

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { readTheBarNow } from "../readTheBar";

// Fleet > API keys > Add an API key (design record
// 2026-11-21-vendor-accounts-and-api-keys, U2): one question a step on the
// shell's one wizard, the forward act on the floor, a refusal in place, and a
// typed value that goes to the cluster in one call and nowhere else. The value
// typed here is a fixture and is never a key.

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { TYPED_KEY, droppedConnection, keyCard, keysConnection, renderKeys, settle } = await import("./keysFixtures");

afterEach(cleanup);
// A bar that has just changed takes no press for a moment (kit ActionBar): each
// press reads the bar first, on a fake Date so reading costs nothing.
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  window.localStorage.clear();
  window.sessionStorage.clear();
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

const keyField = () => screen.getByLabelText("The API key") as HTMLInputElement;
const nameField = () => screen.getByLabelText("A name for this key") as HTMLInputElement;

/** Opens the wizard and answers Vendor and Name, leaving the Key step open. */
async function toTheKeyStep(vendor = "Anthropic", name = "Personal") {
  fireEvent.click(screen.getByRole("button", { name: "Add an API key" }));
  await settle();
  fireEvent.click(screen.getByRole("radio", { name: new RegExp(`^${vendor}`) }));
  await settle();
  fireEvent.change(nameField(), { target: { value: name } });
  await pressBar("Continue");
}

describe("adding a key, a question at a time", () => {
  it("opens under the Add control's own words, on the vendor, with nothing to press but a choice", async () => {
    use();
    await renderKeys("developer");
    fireEvent.click(screen.getByRole("button", { name: "Add an API key" }));
    await settle();
    expect(screen.getByRole("heading", { name: "Add an API key" })).toBeTruthy();
    expect(screen.getByRole("radiogroup", { name: "Vendor" })).toBeTruthy();
    // One choice is the whole step: there is no forward act to press after it.
    expect(barActs()).toEqual(["Cancel"]);
    // The list it came from is not drawn beneath it.
    expect(screen.queryByRole("region", { name: "No API keys" })).toBeNull();
  });

  it("moves on by choosing the vendor, then asks for a name with Continue on the floor", async () => {
    use();
    await renderKeys("developer");
    fireEvent.click(screen.getByRole("button", { name: "Add an API key" }));
    await settle();
    fireEvent.click(screen.getByRole("radio", { name: /^OpenAI/ }));
    await settle();
    expect(nameField()).toBeTruthy();
    expect(screen.queryByLabelText("The API key")).toBeNull();
    // Nothing to continue with yet: the act is absent, not disabled.
    expect(barActs()).toEqual(["Cancel"]);
    fireEvent.change(nameField(), { target: { value: "Work" } });
    expect(barActs()).toEqual(["Cancel", "Continue"]);
    await pressBar("Continue");
    expect(keyField()).toBeTruthy();
  });

  it("refuses a name the person already uses, before anything is sent", async () => {
    const made = use({ mine: [keyCard({ label: "Personal" })] });
    await renderKeys("developer");
    fireEvent.click(screen.getByRole("button", { name: "Add an API key" }));
    await settle();
    fireEvent.click(screen.getByRole("radio", { name: /^Anthropic/ }));
    await settle();
    fireEvent.change(nameField(), { target: { value: "personal" } });
    expect(barActs()).toEqual(["Cancel"]);
    expect(within(bar()).getByText("You already have a key with that name.")).toBeTruthy();
    expect(made.query.vendorKeyAdd).not.toHaveBeenCalled();
  });

  it("hides the key as it is typed, and is not a field a browser keeps", async () => {
    use();
    await renderKeys("developer");
    await toTheKeyStep();
    const field = keyField();
    expect(field.classList.contains("os-input-concealed")).toBe(true);
    expect(field.getAttribute("autocomplete")).toBe("off");
    // Not a password field: a browser offers to keep those.
    expect(field.getAttribute("type")).toBe("text");
    expect(field.getAttribute("name")).toBeNull();
    expect(field.getAttribute("spellcheck")).toBe("false");
    // The forward act arrives with something to send.
    expect(barActs()).toEqual(["Cancel"]);
    fireEvent.change(field, { target: { value: TYPED_KEY } });
    expect(barActs()).toEqual(["Cancel", "Add key"]);
  });
});

describe("where the typed value goes", () => {
  it("is sent in one call, to vendorKeyAdd, and is nowhere else a test can look", async () => {
    const made = use();
    const view = await renderKeys("developer");
    await toTheKeyStep("Anthropic", "Personal");
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });

    // While it is typed and not yet sent: not in the page's markup, not in the
    // page context Ask reads, not in the browser's storage.
    expect(view.container.innerHTML).not.toContain(TYPED_KEY);
    expect(document.body.innerHTML).not.toContain(TYPED_KEY);
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(TYPED_KEY);
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain(TYPED_KEY);

    const warned = vi.spyOn(console, "warn").mockImplementation(() => {});
    const errored = vi.spyOn(console, "error").mockImplementation(() => {});
    await pressBar("Add key");

    expect(made.query.vendorKeyAdd).toHaveBeenCalledTimes(1);
    expect(made.query.vendorKeyAdd.mock.calls[0]![0]).toEqual({ vendor: "anthropic", label: "Personal", key: TYPED_KEY });
    const carrying = made.wire().filter((call) => call.includes(TYPED_KEY));
    expect(carrying).toHaveLength(1);
    expect(carrying[0]!.startsWith("builtin vendorKeyAdd(")).toBe(true);

    expect(document.body.innerHTML).not.toContain(TYPED_KEY);
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(TYPED_KEY);
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain(TYPED_KEY);
    for (const spy of [warned, errored]) {
      expect(spy.mock.calls.flat().map(String).join(" ")).not.toContain(TYPED_KEY);
      spy.mockRestore();
    }
  });

  it("returns to the list with the new key, which is the default at its vendor", async () => {
    const made = use();
    await renderKeys("developer");
    await toTheKeyStep("Anthropic", "Personal");
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Add key");

    expect(screen.queryByRole("heading", { name: "Add an API key" })).toBeNull();
    const list = screen.getByRole("list", { name: "API keys" });
    expect(within(list).getByRole("button", { name: /^Open Personal\b/ })).toBeTruthy();
    expect(within(list).getByText("Anthropic · ...9z9z")).toBeTruthy();
    expect(made.state.mine).toHaveLength(1);
  });

  it("trims what was pasted around the key before sending it", async () => {
    const made = use();
    await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: `  ${TYPED_KEY}\n` } });
    await pressBar("Add key");
    expect((made.query.vendorKeyAdd.mock.calls[0]![0] as { key: string }).key).toBe(TYPED_KEY);
  });
});

describe("when the cluster says no", () => {
  it("puts the refusal beside the field in the vendor's words, and keeps what was typed for another try", async () => {
    const made = use();
    made.fail("vendorKeyAdd", "vendorKeyAdd: Anthropic did not accept this key: invalid x-api-key");
    await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Add key");

    expect(screen.getByText("The key was not added.")).toBeTruthy();
    expect(screen.getByText("Anthropic did not accept this key: invalid x-api-key")).toBeTruthy();
    // Still on the Key step, with the value still in the field.
    expect(keyField().value).toBe(TYPED_KEY);
    expect(barActs()).toEqual(["Cancel", "Add key"]);
    expect(made.state.mine).toHaveLength(0);

    // A second try with a corrected value goes through, and the refusal goes.
    fireEvent.change(keyField(), { target: { value: `${TYPED_KEY}-fixed` } });
    await pressBar("Add key");
    expect(screen.queryByText("The key was not added.")).toBeNull();
    expect(within(screen.getByRole("list", { name: "API keys" })).getByRole("button", { name: /^Open Personal\b/ })).toBeTruthy();
  });

  it("never repeats the typed value, even when a refusal does", async () => {
    const made = use();
    made.fail("vendorKeyAdd", `vendorKeyAdd: the engine did not complete builtin vendorKeyAdd(key: "${TYPED_KEY}")`);
    const view = await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Add key");
    expect(screen.getByText("The key was not added.")).toBeTruthy();
    expect(view.container.textContent).not.toContain(TYPED_KEY);
    expect(document.body.innerHTML).not.toContain(TYPED_KEY);
  });

  it("says a dropped connection may have added the key, and offers no second send from the draft", async () => {
    const made = use();
    made.fail("vendorKeyAdd", droppedConnection());
    await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Add key");

    expect(screen.getByText("The key may have been added.")).toBeTruthy();
    expect(screen.getByText(/Check the list before trying again/)).toBeTruthy();
    expect(screen.queryByText("stream closed")).toBeNull();
    // Sending again from the draft could add the key twice.
    expect(barActs()).toEqual(["Back to API keys"]);
    expect(made.query.vendorKeyAdd).toHaveBeenCalledTimes(1);
    await pressBar("Back to API keys");
    expect(screen.getByRole("heading", { name: "API keys" })).toBeTruthy();
  });

  it("says in the person's words that they may not add keys, when the part was taken away", async () => {
    const made = use();
    made.fail("vendorKeyAdd", 'vendorKeyAdd: capability_not_held: "vendorKeyAdd" requires execute on app:fleet/keys');
    await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Add key");
    expect(screen.getByText("The key was not added.")).toBeTruthy();
    expect(screen.getByText(/You may not add API keys/)).toBeTruthy();
    expect(screen.queryByText(/capability_not_held/)).toBeNull();
  });
});

describe("the cluster's turn, and the ways out", () => {
  it("draws the check as a wait, with no forward act and Leave as the way out", async () => {
    const made = use();
    made.hold("vendorKeyAdd");
    await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Add key");

    expect(within(bar()).getByText("Checking with Anthropic")).toBeTruthy();
    expect(barActs()).toEqual(["Leave"]);
    // The field is not on the page while the cluster works.
    expect(screen.queryByLabelText("The API key")).toBeNull();
    expect(document.body.innerHTML).not.toContain(TYPED_KEY);

    await made.release("vendorKeyAdd");
    expect(within(screen.getByRole("list", { name: "API keys" })).getByRole("button", { name: /^Open Personal\b/ })).toBeTruthy();
  });

  it("lets a person leave during the check, and shows the key when it lands anyway", async () => {
    const made = use();
    made.hold("vendorKeyAdd");
    await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Add key");
    await pressBar("Leave");
    expect(screen.getByRole("heading", { name: "API keys" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "No API keys" })).toBeTruthy();

    await made.release("vendorKeyAdd");
    expect(within(screen.getByRole("list", { name: "API keys" })).getByRole("button", { name: /^Open Personal\b/ })).toBeTruthy();
  });

  it("says a key that may not list models is checked by use, before finishing", async () => {
    const made = use();
    made.state.nextAdded = { checkedBy: "use", state: "unchecked", note: "OpenAI accepted this key, but it may not list models. It is checked by the calls it pays for." };
    await renderKeys("developer");
    await toTheKeyStep("OpenAI", "Restricted");
    fireEvent.change(keyField(), { target: { value: "sk-typed-in-a-test-0002" } });
    await pressBar("Add key");

    expect(screen.getByText(/It is checked by the calls it pays for\./)).toBeTruthy();
    expect(barActs()).toEqual(["Done"]);
    await pressBar("Done");
    expect(within(screen.getByRole("list", { name: "API keys" })).getByRole("button", { name: /^Open Restricted\b/ })).toBeTruthy();
  });

  it("cancels before anything is sent by simply leaving, and forgets what was typed", async () => {
    const made = use();
    await renderKeys("developer");
    await toTheKeyStep();
    fireEvent.change(keyField(), { target: { value: TYPED_KEY } });
    await pressBar("Cancel");
    expect(screen.getByRole("heading", { name: "API keys" })).toBeTruthy();
    expect(made.query.vendorKeyAdd).not.toHaveBeenCalled();
    expect(made.wire().some((call) => call.includes(TYPED_KEY))).toBe(false);

    // Opened again, the wizard starts empty.
    await toTheKeyStep();
    expect(keyField().value).toBe("");
  });
});

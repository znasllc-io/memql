import { render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted -- the
// shape deployables.test.tsx already uses.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import {
  SETTINGS_KEY_FORM,
  settingsFingerprint,
  settingsKeyProblem,
  settingsRows,
  toSettingsMap,
} from "../../src/apps/deployables/settings-editor";
import {
  PLATFORM_SITE,
  SHOP,
  click,
  fakeConnection,
  siteRow,
  type as typeInto,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// A deployable's App values: the runtime settings a bundle reads at load
// (epic memql#4906). These cases lived beside the traffic figure, whose page
// the Statistics page replaced; they are about the settings editor alone.
//
// The arithmetic is asserted directly, because what a draft MEANS -- a blank
// key, a duplicate name, a reordered map -- is a claim about functions. The
// rendered half asserts what reaches the wire and what a person reads.

// ---------------------------------------------------------------------------
// The settings editor's arithmetic
// ---------------------------------------------------------------------------

describe("the runtime-settings editor", () => {
  it("mirrors the server's key form", () => {
    for (const ok of ["apiBase", "A", "a_b_c9", "k".padEnd(64, "9")]) {
      expect(SETTINGS_KEY_FORM.test(ok)).toBe(true);
    }
    for (const bad of ["", "9lives", "_lead", "api-base", "api.base", "k".padEnd(65, "9")]) {
      expect(SETTINGS_KEY_FORM.test(bad)).toBe(false);
    }
  });

  it("orders rows by key so a save does not reshuffle them under the person who made it", () => {
    expect(settingsRows({ zulu: "1", alpha: "2" }).map((r) => r.key)).toEqual(["alpha", "zulu"]);
  });

  it("drops a blank key from the save and keeps a blank value", () => {
    const rows = [
      { id: "1", key: "apiBase", value: "https://api.example" },
      { id: "2", key: "", value: "orphan" },
      { id: "3", key: "region", value: "" },
    ];
    // A row somebody has just added must not make the whole save refuse; an
    // empty string IS a value a bundle can read.
    expect(toSettingsMap(rows)).toEqual({ apiBase: "https://api.example", region: "" });
  });

  it("says nothing about a key nobody has typed yet", () => {
    expect(settingsKeyProblem("", [])).toBe("");
  });

  it("names the form when a key cannot be read by an app", () => {
    expect(settingsKeyProblem("api-base", [{ id: "1", key: "api-base", value: "" }])).toContain("config.settings");
  });

  it("compares two settings objects by their VALUES, not by key order", () => {
    // The stored map arrives from the wire in the payload's order and the
    // draft is rebuilt from rows sorted by key, so a raw stringify comparison
    // reports a difference on a form nobody has touched -- a Save button lit
    // on an untouched panel, and an edit that looks unsaved after it saved.
    const wireOrder = { zulu: "1", alpha: "2" };
    const draftOrder = { alpha: "2", zulu: "1" };
    expect(JSON.stringify(wireOrder)).not.toBe(JSON.stringify(draftOrder));
    expect(settingsFingerprint(wireOrder)).toBe(settingsFingerprint(draftOrder));
    // And it still tells a real change apart.
    expect(settingsFingerprint(wireOrder)).not.toBe(settingsFingerprint({ alpha: "2", zulu: "changed" }));
    expect(settingsFingerprint(wireOrder)).not.toBe(settingsFingerprint({ alpha: "2" }));
  });

  it("catches two rows sharing a name, which the server structurally cannot", () => {
    const rows = [
      { id: "1", key: "apiBase", value: "a" },
      { id: "2", key: "apiBase", value: "b" },
    ];
    // The two collapse into one object key on the way out, so the save would
    // succeed and quietly keep whichever came last.
    expect(settingsKeyProblem("apiBase", rows)).toContain("Only one of them would be saved");
    expect(Object.keys(toSettingsMap(rows))).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// On the page
// ---------------------------------------------------------------------------

function mount(connection: FakeConnection) {
  h.connection = connection;
  return render(
    withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} />, {
      role: "owner",
      userId: "u-me",
    }),
  );
}

/** Open a deployable and return its page, scoped to the region the page owns. */
async function openDeployable(seed: FakeSeed, hostname: string): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const connection = fakeConnection(seed);
  mount(connection);
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  await click((await screen.findByText(hostname)).closest("button"));
  return { connection, page: await screen.findByRole("region", { name: `Deployable ${hostname}` }) };
}

/** Open a deployable's App values, a page of its own. */
async function openAppValues(seed: FakeSeed, hostname: string): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const { connection, page } = await openDeployable(seed, hostname);
  await click(within(page).getByRole("button", { name: /^App values/ }));
  return { connection, page: await screen.findByRole("region", { name: `App values for ${hostname}` }) };
}

describe("a deployable's App values, rendered", () => {
  const SHOP_LIVE = siteRow({ ...SHOP, id: "site-shop", status: "live" });

  it("shows the settings a deployable carries, and the sentence about secrets", async () => {
    const { page } = await openAppValues(
      { sites: [siteRow({ ...SHOP_LIVE, id: "site-shop", settings: { apiBase: "https://api.eu.example" } })] },
      "shop.memql.example.com",
    );
    await waitFor(() => expect(within(page).getByText(/Not a place for a secret/i)).toBeTruthy());
    expect((within(page).getAllByRole("textbox") as HTMLInputElement[]).some((i) => i.value === "apiBase")).toBe(true);
    expect((within(page).getAllByRole("textbox") as HTMLInputElement[]).some((i) => i.value === "https://api.eu.example")).toBe(true);
  });

  it("sends the whole map, so removing a setting is expressible", async () => {
    const { connection, page } = await openAppValues(
      { sites: [siteRow({ ...SHOP_LIVE, id: "site-shop", settings: { apiBase: "https://api.eu.example", region: "eu" } })] },
      "shop.memql.example.com",
    );
    await waitFor(() => expect(within(page).getAllByRole("button", { name: /^Remove region$/ }).length).toBe(1));
    await click(within(page).getByRole("button", { name: /^Remove region$/ }));
    await click(within(page).getByRole("button", { name: /save settings/i }));

    await waitFor(() => expect(connection.callsNamed("updateSiteSettings").length).toBe(1));
    const call = connection.callsNamed("updateSiteSettings")[0]!;
    expect(call).toContain("apiBase");
    // THE WHOLE MAP: a merge would have kept `region` and made the removal
    // silently do nothing.
    expect(call).not.toContain("region");
  });

  it("reports a refused save verbatim", async () => {
    const { page } = await openAppValues(
      {
        sites: [siteRow({ ...SHOP_LIVE, id: "site-shop", settings: { apiBase: "x" } })],
        settingsError:
          'v1:platform:site: settings key "apiTokenRef" ends in Ref, and a setting is never a reference',
      },
      "shop.memql.example.com",
    );
    const value = (within(page).getAllByRole("textbox") as HTMLInputElement[]).find((i) => i.value === "x")!;
    await typeInto(value, "y");
    await click(within(page).getByRole("button", { name: /save settings/i }));
    await waitFor(() => expect(within(page).getByText(/ends in Ref/)).toBeTruthy());
  });

  it("renders no settings controls at all on a system-owned row", async () => {
    const { page } = await openDeployable({ sites: [PLATFORM_SITE] }, "os.memql.example.com");
    // The seeded platform row is exempt from the whole axis: there is no App
    // values to open, and nothing on its page could edit one.
    expect(within(page).queryByRole("button", { name: /^App values/ })).toBeNull();
    expect(within(page).queryByRole("button", { name: /add a setting/i })).toBeNull();
    expect(within(page).queryByRole("button", { name: /save settings/i })).toBeNull();
  });
});

import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/modules/profile/avatarImage", async (load) => ({
  ...(await load<typeof import("../../src/modules/profile/avatarImage")>()),
  avatarFromFile: vi.fn(async () => "data:image/webp;base64,UklGRg=="),
}));

import { ProfileSection } from "../../src/apps/identity/ProfileSection";
import { budgetNotServed, storageShare } from "../../src/apps/identity/profileData";
import { parseBudget, resetDate, timezoneChoices } from "../../src/apps/identity/ProfileSection";
import { AccountAvatar } from "../../src/chrome/AccountAvatar";
import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import { resetMyProfileForTest } from "../../src/modules/profile/myProfile";

// Your profile (memql#6220): every section in each state it can be in.

const rows = (list: Row[]) => new Result({ data: list } as never);

const ME: Row = {
  id: "v1:identity:user:ada",
  displayName: "Ada Lovelace",
  firstName: "Ada",
  lastName: "Lovelace",
  primaryEmail: "ada@example.test",
  role: "owner",
  avatar: "",
  signInPolicy: "any",
  preferences: {},
};

const LIMITED = { used: 2_000_000_000, reserved: 0, limit: 10_000_000_000, available: 8_000_000_000, clusterUsed: 0, clusterReserved: 0, clusterLimit: 5e12, exempt: false };
const BUDGET = { monthlyLimitUSD: 50, spentThisMonthUSD: 12.5, resetsAt: "2026-11-01T00:00:00Z" };

interface Seed {
  me?: Row;
  storage?: Row | Error;
  passkeys?: Row[];
  budget?: Row | Error;
}

function connect(seed: Seed = {}) {
  const executeNamed = vi.fn(async (name: string, call: string) => {
    if (name === "myBudget") {
      if (seed.budget instanceof Error) throw seed.budget;
      return rows([seed.budget ?? BUDGET]);
    }
    if (name === "myBudgetSave") {
      const match = /monthlyLimitUSD: ([0-9.]+)/.exec(call);
      return rows([{ ...BUDGET, monthlyLimitUSD: match ? Number(match[1]) : null }]);
    }
    throw new Error(`unexpected ${name}`);
  });
  const query = {
    currentUser: vi.fn(async () => rows([seed.me ?? ME])),
    libraryBackupUsage: vi.fn(async () => {
      if (seed.storage instanceof Error) throw seed.storage;
      return rows([seed.storage ?? LIMITED]);
    }),
    passkeysForSelf: vi.fn(async () => rows(seed.passkeys ?? [{ id: "p1", active: true }, { id: "p2", active: true }, { id: "p3", active: false }])),
    updateMyName: vi.fn(async () => rows([])),
    setMyAvatar: vi.fn(async () => rows([])),
    clearMyAvatar: vi.fn(async () => rows([])),
    toggleComputerUseEnabled: vi.fn(async () => rows([])),
    updateMyPreferences: vi.fn(async () => rows([])),
    executeNamed,
  };
  h.connection = { query };
  return query;
}

function withSession(children: ReactNode) {
  return (
    <SessionProvider
      value={{
        access: { userId: "v1:identity:user:ada", primaryEmail: "ada@example.test", role: "owner", roleName: "Owner", rank: 400 },
        config: UNKNOWN_RUNTIME_CONFIG,
      }}
    >
      {children}
    </SessionProvider>
  );
}

async function open(seed: Seed = {}) {
  const query = connect(seed);
  render(withSession(<><ProfileSection /><span data-testid="dock"><AccountAvatar /></span></>));
  await screen.findByText("Ada Lovelace", { selector: ".os-profile-name" });
  return query;
}

afterEach(() => {
  resetMyProfileForTest();
  h.connection = null;
});

describe("the header", () => {
  it("shows a skeleton until the row lands, then the person: name, address and role", async () => {
    connect();
    render(withSession(<ProfileSection />));
    expect(screen.getByRole("status", { name: "" })).toBeTruthy();
    expect(await screen.findByText("Ada Lovelace", { selector: ".os-profile-name" })).toBeTruthy();
    expect(screen.getByText("ada@example.test · Owner")).toBeTruthy();
  });

  it("says why when the profile cannot be read, with a way to try again", async () => {
    h.connection = { query: { currentUser: vi.fn(async () => { throw new Error("stream closed"); }) } };
    render(withSession(<ProfileSection />));
    expect(await screen.findByText("Your profile could not be read.")).toBeTruthy();
    expect(screen.getByText("stream closed")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  });
});

describe("General", () => {
  it("renames in place: Save appears with a change, and the name composes from its parts", async () => {
    const query = await open();
    expect(screen.queryByRole("button", { name: "Save name" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Last name"), { target: { value: "King" } });
    const save = screen.getByRole("button", { name: "Save name" });
    await act(async () => { fireEvent.click(save); });
    expect(query.updateMyName).toHaveBeenCalledWith({ firstName: "Ada", lastName: "King", displayName: "Ada King" });
    expect(await screen.findByText("Ada King", { selector: ".os-profile-name" })).toBeTruthy();
  });

  it("keeps the draft and says why when the engine refuses the name", async () => {
    const query = await open();
    query.updateMyName.mockRejectedValueOnce(new Error("argument validation failed"));
    fireEvent.change(screen.getByLabelText("First name"), { target: { value: "Augusta" } });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Save name" })); });
    expect(screen.getByRole("alert").textContent).toBe("The name was not changed: argument validation failed");
    expect((screen.getByLabelText("First name") as HTMLInputElement).value).toBe("Augusta");
  });

  it("does not offer Save for a blank first name", async () => {
    await open();
    fireEvent.change(screen.getByLabelText("First name"), { target: { value: "   " } });
    expect(screen.queryByRole("button", { name: "Save name" })).toBeNull();
  });

  it("uploads a photo, and every avatar follows", async () => {
    const query = await open();
    expect(within(screen.getByTestId("dock")).queryByRole("img", { hidden: true })).toBeNull();
    const input = screen.getByLabelText("Choose a photo") as HTMLInputElement;
    await act(async () => {
      fireEvent.change(input, { target: { files: [new File(["x"], "me.png", { type: "image/png" })] } });
    });
    expect(query.setMyAvatar).toHaveBeenCalledWith({ avatar: "data:image/webp;base64,UklGRg==" });
    await waitFor(() => expect(screen.getByTestId("dock").querySelector("img")?.getAttribute("src")).toBe("data:image/webp;base64,UklGRg=="));
    expect(screen.getByRole("button", { name: "Change photo" })).toBeTruthy();
  });

  it("asks before removing the photo, and the initial comes back", async () => {
    const query = await open({ me: { ...ME, avatar: "data:image/png;base64,iVBORw0KGgo=" } });
    fireEvent.click(screen.getByRole("button", { name: "Remove photo" }));
    expect(screen.getByText("Remove your photo? Your initial is shown instead.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Keep it" }));
    expect(query.clearMyAvatar).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Remove photo" }));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 450)); // the confirmation's press hold
    });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Remove photo" })); });
    expect(query.clearMyAvatar).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.getByTestId("dock").querySelector("img")).toBeNull());
    expect(screen.getByTestId("dock").textContent).toBe("A");
  });

  it("shows the email and role as facts, and directory fields only when set", async () => {
    await open({ me: { ...ME, phone: "+1 555 0100", primaryRole: "Engineer" } });
    expect(screen.getByText("+1 555 0100")).toBeTruthy();
    expect(screen.getByText("Engineer")).toBeTruthy();
  });
});

describe("Storage", () => {
  it("reads the allowance the platform already keeps: allocated, used, left and a bar", async () => {
    await open();
    const section = screen.getByRole("region", { name: "Storage" });
    expect(within(section).getByText("10 GB")).toBeTruthy();
    expect(within(section).getByText("2 GB")).toBeTruthy();
    expect(within(section).getByText("8 GB")).toBeTruthy();
    expect(within(section).getByRole("meter", { name: "Storage used" }).getAttribute("aria-valuenow")).toBe("20");
  });

  it("says there is no personal limit, with no bar, for an exempt account", async () => {
    await open({ storage: { ...LIMITED, exempt: true } });
    const section = screen.getByRole("region", { name: "Storage" });
    expect(within(section).getByText("No personal limit")).toBeTruthy();
    expect(within(section).queryByRole("meter")).toBeNull();
  });

  it("says once that storage is not set up, rather than drawing an error", async () => {
    await open({ storage: new Error("backup storage is unavailable") });
    expect(await screen.findByText("Storage is not set up on this cluster.")).toBeTruthy();
  });

  it("reports any other failure with the engine's reason", async () => {
    await open({ storage: new Error("timeout") });
    expect(await screen.findByText("Storage could not be checked.")).toBeTruthy();
  });

  it("computes the share in use, and none without a personal limit", () => {
    expect(storageShare(LIMITED)).toBe(0.2);
    expect(storageShare({ ...LIMITED, reserved: 9_000_000_000 })).toBe(1);
    expect(storageShare({ ...LIMITED, exempt: true })).toBeNull();
    expect(storageShare({ ...LIMITED, limit: 0 })).toBeNull();
  });
});

describe("AI budget", () => {
  it("is absent on a cluster that does not serve the budget", async () => {
    await open({ budget: new Error('function "myBudget" not found') });
    await waitFor(() => expect(screen.queryByRole("region", { name: "AI budget" })).toBeNull());
    expect(budgetNotServed('function "myBudget" not found')).toBe(true);
    expect(budgetNotServed("timeout")).toBe(false);
  });

  it("shows the limit, this month's spend and when it resets", async () => {
    await open();
    const section = await screen.findByRole("region", { name: "AI budget" });
    expect((within(section).getByLabelText("Monthly budget in US dollars") as HTMLInputElement).value).toBe("50");
    expect(within(section).getByText("Spent this month")).toBeTruthy();
    expect(within(section).getByText(/12\.50/)).toBeTruthy();
    expect(within(section).getByText("Nov 1")).toBeTruthy();
  });

  it("saves a new limit, and an emptied field removes it by leaving the amount out", async () => {
    const query = await open();
    const section = await screen.findByRole("region", { name: "AI budget" });
    expect(within(section).queryByRole("button", { name: "Save budget" })).toBeNull();
    const field = within(section).getByLabelText("Monthly budget in US dollars");
    fireEvent.change(field, { target: { value: "75" } });
    await act(async () => { fireEvent.click(within(section).getByRole("button", { name: "Save budget" })); });
    expect(query.executeNamed).toHaveBeenCalledWith("myBudgetSave", "builtin myBudgetSave(monthlyLimitUSD: 75)");

    fireEvent.change(within(section).getByLabelText("Monthly budget in US dollars"), { target: { value: "" } });
    await act(async () => { fireEvent.click(within(section).getByRole("button", { name: "Remove budget" })); });
    expect(query.executeNamed).toHaveBeenCalledWith("myBudgetSave", "builtin myBudgetSave()");
  });

  it("does not offer Save for an amount that is not dollars", async () => {
    await open();
    const section = await screen.findByRole("region", { name: "AI budget" });
    fireEvent.change(within(section).getByLabelText("Monthly budget in US dollars"), { target: { value: "lots" } });
    expect(within(section).queryByRole("button", { name: "Save budget" })).toBeNull();
    expect(within(section).getByText("Enter an amount in dollars, at least $1.")).toBeTruthy();
    expect(parseBudget("$1,250.50")).toBe(1250.5);
    expect(parseBudget("0.5")).toBeNull();
    expect(parseBudget("12.345")).toBeNull();
    expect(resetDate("2026-11-01T00:00:00Z")).toBe("Nov 1");
    expect(resetDate("")).toBe("");
  });
});

describe("Sign-in and security", () => {
  it("counts active passkeys and states the sign-in policy in words", async () => {
    await open({ me: { ...ME, signInPolicy: "passkey_only" } });
    const section = screen.getByRole("region", { name: "Sign-in and security" });
    expect(await within(section).findByText("2 passkeys")).toBeTruthy();
    expect(within(section).getByText("Passkey only")).toBeTruthy();
  });
});

describe("Preferences", () => {
  it("turns computer use off through the person's own switch, and says what that means", async () => {
    const query = await open();
    const toggle = screen.getByRole("switch", { name: "Let tasks use my machines" }) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
    await act(async () => { fireEvent.click(toggle); });
    expect(query.toggleComputerUseEnabled).toHaveBeenCalledWith({ enabled: false });
    expect(await screen.findByText("Tasks cannot run anything on your machines until this is on.")).toBeTruthy();
  });

  it("offers UTC, the person's own zone and the browser's", () => {
    const zones = timezoneChoices("America/Argentina/Buenos_Aires");
    expect(zones).toContain("America/Argentina/Buenos_Aires");
    expect(zones).not.toContain("UTC");
    expect([...zones].sort()).toEqual(zones);
  });
});

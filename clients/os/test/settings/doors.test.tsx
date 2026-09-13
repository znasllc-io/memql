import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ReactNode } from "react";

import type { DoorId, InferenceReading } from "../../src/apps/settings/routingFacts";

// Settings -> Doors (epic memql#4984; rebuilt by epic memql#5088; re-shaped
// into a list by epic memql#5153).
//
// THIS SUITE IS THE RECORD OF WHAT THE SCREEN OWES, and epic memql#5153's D5
// is "nothing lost" -- so when "AI providers" became "Doors" every assertion
// here was kept, and each one was either satisfied where it stood, re-pointed
// at where its behaviour moved, or fixed by the section. What moved:
//
//   - THE THREE STANDING PANELS ARE A FOUR-ROW LIST, in a FIXED try order
//     (fleet, apps, anthropic, openai). The old screen re-ordered itself on a
//     local cluster to put the fleet first; the new one does not re-order at
//     all, because the order IS the product -- free doors first, paid last --
//     and a screen that flatters the cluster's current state teaches the wrong
//     thing. What the old assertion protected (the fleet door is prominent on
//     a local cluster) is now true for everyone, permanently.
//   - A VENDOR'S FORM OPENS BEHIND ITS ROW rather than standing on the page,
//     so a test that reads a vendor panel presses that row's act first.
//   - THE FLEET DOOR'S READING MOVED from `providerFacts.fleetDoorFrom` to
//     `routingFacts.doorReadings`, which answers for all four doors out of one
//     read. Its four sentences came with it, so the pure cases below are the
//     same cases pointed at the live function.
//
// THE SECTION HAS NO KEY FIELD, and this suite's first job is still to keep it
// that way. Federation is the only door for a cloud vendor and the two free
// doors are the others; the engine's key-sealing builtin is deleted, so a box
// that posted to it would be a control whose only outcome is a refusal.
//
// The planted key below is what the "no key anywhere" sweeps look for. It is
// long and distinctive so a sweep over the rendered tree is not vacuous, and
// nothing in the suite ever types it -- there is nowhere to type it.
const PLANTED_KEY = "sk-PLANTED-PROVIDER-KEY-DO-NOT-EMIT-000000";

/** The sentence `anthropicCredential` hands a half-configured provider entry. */
const HALF_SET_REASON =
  'provider "streamClaudeSonnet" is HALF-CONFIGURED for Anthropic workload identity ' +
  "federation: ruleId, organizationId set, serviceAccountId missing. Set all four " +
  "(or none). Runbook: docs/public/operate/auth/anthropic-federation.md";

const h = vi.hoisted(() => {
  const reply = (rows: unknown[]) => ({ rows: () => rows });
  const state = {
    providers: [] as unknown[],
    providerError: null as Error | null,
    federationCalls: [] as Record<string, string>[],
    inference: {} as Record<string, unknown>,
    inferenceError: null as Error | null,
    verifyReply: { verified: true, reason: "" } as Record<string, unknown>,
    reloadCalls: 0,
  };
  const connection = {
    nodeId: "bff-test",
    engineVersion: "v9.9.9",
    engineCommit: "abcdef123456",
    subscriptions: null,
    dispatcher: null,
    query: {
      providerAuthStatus: vi.fn(async () => {
        if (state.providerError) throw state.providerError;
        return reply(state.providers);
      }),
      inferenceStatus: vi.fn(async () => {
        if (state.inferenceError) throw state.inferenceError;
        return reply([state.inference]);
      }),
      providerFederationSet: vi.fn(async (args: Record<string, string>) => {
        state.federationCalls.push(args);
        return reply([{ message: "Federation ids stored." }]);
      }),
      providerVerify: vi.fn(async () => reply([state.verifyReply])),
      providersReload: vi.fn(async () => {
        state.reloadCalls += 1;
        return reply([{ availableOnThisNode: 1, registered: 2 }]);
      }),
    },
    onStatusChange: () => () => {},
  };
  return { connection, state };
});

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { SessionProvider } = await import("../../src/chrome/access");
const { OsProvider } = await import("../../src/chrome/state");
const { OS_REGISTRY } = await import("../../src/apps/registry");
const { SettingsApp } = await import("../../src/apps/settings/SettingsApp");
const { LocalDesktopStore } = await import("../../src/system/store");
const { UNKNOWN_RUNTIME_CONFIG } = await import("../../src/cluster/config");
const { roleOpens } = await import("../seededAccess");
const { DOORS_SECTION_RESOURCE, DOOR_WORDS } = await import(
  "../../src/apps/settings/DoorsSection"
);
const { doorFor, localityOf, missingFederationFields, summarize } = await import(
  "../../src/apps/settings/providerFacts"
);
const { DOOR_COST, DOOR_NAMES, DOOR_ORDER, UNREAD_INFERENCE, doorReadings } = await import(
  "../../src/apps/settings/routingFacts"
);

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function wrap(children: ReactNode, role: string, domain: string) {
  return (
    <SessionProvider
      value={{
        access: { userId: "u-1", primaryEmail: "owner@example.com", role: role, roleName: "", rank: 0 },
        config: {
          ...UNKNOWN_RUNTIME_CONFIG,
          domain,
          identityUrl: domain === "" ? "" : `https://identity.${domain}`,
        },
      }}
    >
      <OsProvider
        registry={OS_REGISTRY}
        actorRole={role}
        grid={{ cols: 12, rows: 8 }}
        store={new LocalDesktopStore(memStorage())}
      >
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

/** Let both reads and the effects they trigger settle. */
async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

async function renderDoors(role = "owner", domain = "example.com") {
  const view = render(
    wrap(
      <SettingsApp sectionId="providers" navigate={vi.fn()} askContext={vi.fn()} />,
      role,
      domain,
    ),
  );
  await settle();
  return view;
}

/**
 * One door's row in the try-order list.
 *
 * BY ID, NOT BY NAME. `data-os-doorid` is the engine's own door vocabulary and
 * is what the row is keyed on; reading a row by its rendered NAME would make
 * every assertion below a test of the copy as well as of the state.
 */
function doorRow(id: DoorId): HTMLElement {
  const row = document.querySelector<HTMLElement>(`[data-os-doorid="${id}"]`);
  if (row === null) throw new Error(`no door row rendered for ${id}`);
  return row;
}

/** The state word that row carries -- the channel that survives greyscale. */
function doorWordIn(id: DoorId): string {
  return doorRow(id).querySelector(".os-door-state")?.textContent ?? "";
}

/** The state that row is in. */
function doorStateIn(id: DoorId): string {
  return doorRow(id).getAttribute("data-os-door") ?? "";
}

/** The doors as a reader takes them: top to bottom. */
function doorOrder(): string[] {
  return [...document.querySelectorAll("[data-os-doorid]")].map(
    (el) => el.getAttribute("data-os-doorid") ?? "",
  );
}

/** ...and the names beside them, which is what a person actually reads. */
function doorNamesInOrder(): string[] {
  return [...document.querySelectorAll(".os-doorrow-name")].map((el) => el.textContent ?? "");
}

/** Press a vendor row's act, which is what opens its panel. */
async function openVendorPanel(id: "anthropic" | "openai", label: string): Promise<HTMLElement> {
  fireEvent.click(
    within(doorRow(id)).getByRole("button", { name: new RegExp(`^(Set up|Edit) ${label}$`) }),
  );
  await settle();
  return screen.getByRole("region", { name: label });
}

beforeEach(() => {
  // PER TEST, because vitest isolates modules per FILE and not per test: the
  // spies below are module state, so a click in one case is still recorded
  // when the next one asks whether anything was called. `clearAllMocks` clears
  // the call log and keeps the implementations `vi.fn(impl)` installed.
  vi.clearAllMocks();
  h.state.providers = [
    {
      name: "chat54Mini",
      vendor: "openai",
      model: "gpt-5.4-mini",
      available: true,
      authSource: "federation",
      reason: "",
    },
    {
      name: "streamClaudeSonnet",
      vendor: "anthropic",
      model: "claude-sonnet-5",
      available: false,
      authSource: "unresolved",
      reason: "no credential configured",
    },
  ];
  h.state.providerError = null;
  h.state.federationCalls = [];
  h.state.inference = {
    eligible: true,
    localEligible: true,
    localModelCount: 3,
    eligibleModelIds: ["qwen3-coder", "llama4"],
    minimumContextWindow: 32000,
    fleetCatalogInstalled: true,
  };
  h.state.inferenceError = null;
  h.state.verifyReply = { verified: true, reason: "" };
  h.state.reloadCalls = 0;
});

describe("the provider summary", () => {
  // Pure, so the three states are pinned without rendering. The keyless case
  // is the one that matters: it is how a correctly-installed cluster starts.
  it("calls an empty registry the install state, not a fault", () => {
    expect(summarize([]).tone).toBe("unconfigured");
    expect(summarize([]).headline).toMatch(/how a cluster is installed/);
  });

  it("reports a partial registry as partial rather than ready", () => {
    const rows = [
      { name: "a", vendor: "openai", model: "m", available: true, authSource: "env", reason: "" },
      {
        name: "b",
        vendor: "anthropic",
        model: "m",
        available: false,
        authSource: "unresolved",
        reason: "",
      },
    ];
    expect(summarize(rows)).toEqual({
      tone: "partial",
      headline: "1 of 2 providers can be called.",
    });
  });

  it("calls a registry with no callable provider unconfigured, however many are registered", () => {
    // The trap this pins: `total > 0` is not the same question as "can this
    // cluster call a model". Two registered providers that both fail to
    // resolve is the keyless state wearing a count.
    const rows = [
      {
        name: "a",
        vendor: "openai",
        model: "m",
        available: false,
        authSource: "unresolved",
        reason: "",
      },
      {
        name: "b",
        vendor: "anthropic",
        model: "m",
        available: false,
        authSource: "unresolved",
        reason: "",
      },
    ];
    expect(summarize(rows).tone).toBe("unconfigured");
  });
});

describe("a vendor's door, as a pure reading", () => {
  const row = (over: Record<string, unknown>) =>
    ({
      name: "p",
      vendor: "openai",
      model: "m",
      available: false,
      authSource: "unresolved",
      reason: "",
      ...over,
    }) as Parameters<typeof doorFor>[1][number];

  it("is open when a provider of that vendor resolved through federation", () => {
    expect(doorFor("openai", [row({ authSource: "federation", available: true })]).state).toBe(
      "open",
    );
  });

  it("is unset when no id is anywhere -- which is how every cluster is installed", () => {
    expect(doorFor("openai", [row({})]).state).toBe("unset");
    // And a vendor with no registered provider at all is unset, not unknown:
    // there is one answer to "can this cluster call Anthropic" and it is no.
    expect(doorFor("anthropic", [row({})]).state).toBe("unset");
  });

  it("is half when the engine says half-configured, and keeps the engine's words", () => {
    const door = doorFor("anthropic", [row({ vendor: "anthropic", reason: HALF_SET_REASON })]);
    expect(door.state).toBe("half");
    expect(door.said).toBe(HALF_SET_REASON);
  });

  it("prefers half over open, so the dangerous state cannot hide behind the reassuring one", () => {
    const door = doorFor("openai", [
      row({ authSource: "federation", available: true }),
      row({ name: "q", reason: HALF_SET_REASON }),
    ]);
    expect(door.state).toBe("half");
  });

  it("matches a vendor's provider types by prefix, the way the engine does", () => {
    // One auth block registers `openai`, `openaistt`, `openaiwhisper` and
    // seven more. An equality match would report the door shut while every
    // one of them was federated.
    expect(
      doorFor("openai", [row({ vendor: "openaiwhisper", authSource: "federation" })]).state,
    ).toBe("open");
    // The negative control: a prefix match must not reach across vendors.
    expect(
      doorFor("anthropic", [row({ vendor: "openaiwhisper", authSource: "federation" })]).state,
    ).toBe("unset");
  });
});

describe("whether either vendor could reach this cluster's issuer", () => {
  // Mirrors `IsLocalDomain` in integrations/email/delivery.go, arm for arm.
  it("reads every local shape the engine reads", () => {
    for (const d of [
      "localhost",
      "127.0.0.1",
      "::1",
      "0.0.0.0",
      "memql.localhost",
      "os.memql.localhost",
      "MEMQL.LOCALHOST",
      "app.local.example.com",
    ]) {
      expect(localityOf(d), d).toBe("local");
    }
  });

  it("reads an ordinary domain as reachable", () => {
    for (const d of ["example.com", "memql.example.com", "local.example.com"]) {
      // Note the third: `local.example.com` has `local` as its FIRST label,
      // not its second, so it is an ordinary domain. The engine draws the line
      // in the same place, and getting it wrong here would withhold the form
      // from a real cluster.
      expect(localityOf(d), d).toBe("reachable");
    }
  });

  it("answers unknown for an unread domain rather than guessing local", () => {
    // The one place this deliberately differs from the Go predicate, which
    // reads empty as local. In a browser empty means the runtime config has
    // not landed, and flashing "federation is not available here" at a cloud
    // operator is a claim the surface could not take back.
    expect(localityOf("")).toBe("unknown");
    expect(localityOf("   ")).toBe("unknown");
  });
});

// ===========================================================================
// The two free doors, as pure readings.
//
// RE-POINTED, NOT REWRITTEN (epic memql#5153). These cases were written
// against `providerFacts.fleetDoorFrom`, which the list replaced with
// `routingFacts.doorReadings` -- one read answering for all four doors, so
// there is no second reading of the same rows to disagree with. The sentences
// they assert came across verbatim; leaving them pointed at the retired
// function would have left five green assertions guarding nothing rendered.
// ===========================================================================

/** A reading of `inferenceStatus`, as the engine hands one back. */
function reading(over: Partial<InferenceReading>): InferenceReading {
  return { ...UNREAD_INFERENCE, read: true, ...over };
}

/** The four doors, with both vendors unset so a case is about one door. */
function doorsFrom(status: InferenceReading) {
  return doorReadings(status, () => ({ state: "unset" as const, said: "" }));
}

const fleetFrom = (over: Partial<InferenceReading>) => doorsFrom(reading(over))[0]!;
const appFrom = (over: Partial<InferenceReading>) => doorsFrom(reading(over))[1]!;

describe("the fleet door, as a pure reading", () => {
  it("is open when a machine offers a model that clears the floor", () => {
    const door = fleetFrom({
      localEligible: true,
      localModelCount: 3,
      eligibleModelIds: ["a", "b"],
      minimumContextWindow: 32000,
      fleetCatalogInstalled: true,
    });
    expect(door.state).toBe("open");
    expect(door.said).toMatch(/2 of 3 models on your machines meet the/);
  });

  it("agrees with itself about number", () => {
    // "2 of 3 models ... meets" and "offer 1 model, and none of them meets"
    // both shipped past a green suite, because no assertion read the sentence.
    expect(
      fleetFrom({
        localEligible: true,
        localModelCount: 1,
        eligibleModelIds: ["a"],
        minimumContextWindow: 32000,
        fleetCatalogInstalled: true,
      }).said,
    ).toMatch(/1 of 1 model on your machines meets the/);
    expect(
      fleetFrom({
        localEligible: false,
        fleetCatalogInstalled: true,
        localModelCount: 1,
        minimumContextWindow: 32000,
      }).said,
    ).toMatch(/offer one model, and it does not meet the/);
  });

  it("tells unreadable inventory apart from an empty fleet", () => {
    // Inventory access is independent of whether this node dispatches calls.
    expect(
      fleetFrom({ localEligible: false, fleetCatalogInstalled: false, localModelCount: 0 }).said,
    ).toMatch(/fleet inventory cannot be read/);
    expect(
      fleetFrom({ localEligible: false, fleetCatalogInstalled: true, localModelCount: 0 }).said,
    ).toMatch(/No machine you own is offering a model/);
  });

  it("names the floor when machines are there but none clears it", () => {
    const door = fleetFrom({
      localEligible: false,
      fleetCatalogInstalled: true,
      localModelCount: 2,
      minimumContextWindow: 32000,
    });
    // RE-POINTED STATE. The old vocabulary had one shut word, so this read
    // `unset`; the list has a fourth state and this is what it is for -- the
    // node can place the call and the machines are there, and only the models
    // fall short. The word a reader sees moves from "Not set up" to "Half set
    // up", which is the honest one.
    expect(door.state).toBe("half");
    expect(door.said).toMatch(/offer 2 models/);
    expect(door.said).toMatch(/32,000-token floor/);
  });

  it("calls no reading unknown rather than closed", () => {
    expect(doorsFrom(UNREAD_INFERENCE)[0]!.state).toBe("unknown");
  });

  it("says a read that FAILED is not a fleet with nothing on it", () => {
    // The two unread cases are different facts: nobody has asked yet, and we
    // asked and could not get an answer. Neither of them is "shut", and the
    // second is the one an operator can act on.
    expect(doorsFrom({ ...UNREAD_INFERENCE })[0]!.said).toMatch(/has not answered yet/);
    const failed = doorsFrom({ ...UNREAD_INFERENCE, error: "inferenceStatus: not connected" })[0]!;
    expect(failed.state).toBe("unknown");
    expect(failed.said).toMatch(/not the same as a fleet with nothing on it/);
    expect(failed.detail).toBe("inferenceStatus: not connected");
  });
});

describe("the app door, as a pure reading", () => {
  it("is open when a signed-in app is ready, and names which one", () => {
    const door = appFrom({ appEligible: true, runnableApps: ["claude-code"], appSessionsInstalled: true });
    expect(door.state).toBe("open");
    expect(door.said).toMatch(/Claude Code signed in and ready/);
    expect(door.metered).toBe(false);
  });

  it("needs a runnable app as well as the verdict, so an empty list is not an open door", () => {
    // `appEligible` with no `runnableApps` would render "No app signed in and
    // ready" beside the word Open.
    expect(appFrom({ appEligible: true, runnableApps: [], appSessionsInstalled: true }).state).toBe(
      "half",
    );
  });

  it("is half when this node can open a session and nobody has signed in", () => {
    const door = appFrom({ appEligible: false, appSessionsInstalled: true });
    expect(door.state).toBe("half");
    expect(door.said).toMatch(/No signed-in Claude Code or Codex/);
  });

  it("is shut when this cluster cannot open app sessions at all", () => {
    // `appSessionsInstalled` reports local dispatch capability, and the
    // engine says so in its own field description: it reports whether the NODE
    // can open sessions, never whether an app is installed on a machine. The
    // two states have entirely different fixes and only one of them is the
    // person's to make.
    const door = appFrom({ appEligible: false, appSessionsInstalled: false });
    expect(door.state).toBe("shut");
    expect(door.said).toMatch(/not set up to run work inside a signed-in app/);
  });
});

describe("which ids a draft is still missing", () => {
  it("names them for each vendor, and never counts the optional workspace", () => {
    expect(missingFederationFields("anthropic", {}).map((f) => f.key)).toEqual([
      "ruleId",
      "organizationId",
      "serviceAccountId",
    ]);
    expect(
      missingFederationFields("anthropic", {
        ruleId: "fdrl_1",
        organizationId: "org-1",
        serviceAccountId: "sa-1",
      }),
    ).toEqual([]);
    expect(
      missingFederationFields("openai", { identityProviderId: "idp_1" }).map((f) => f.key),
    ).toEqual(["serviceAccountId"]);
  });

  it("treats whitespace as missing, because the engine trims before it counts", () => {
    expect(
      missingFederationFields("openai", { identityProviderId: "  ", serviceAccountId: "x" }),
    ).toHaveLength(1);
  });
});

describe("Settings -> Doors: there is no key", () => {
  it("offers no field, button or label that would take one", async () => {
    const { container } = await renderDoors();
    expect(screen.queryByLabelText(/api key/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /seal/i })).toBeNull();
    expect(container.innerHTML).not.toContain(PLANTED_KEY);
    // No input on the surface is a credential box: every one of them takes a
    // federation id, which is public by construction.
    for (const input of container.querySelectorAll("input")) {
      expect(input.getAttribute("type")).not.toBe("password");
    }
    // The negative control. Without it this asserts only that the sweep ran.
    expect(container.innerHTML).toContain("chat54Mini");
  });

  it("keeps the same sweep true once both vendor forms are open", async () => {
    // The forms moved behind their rows, so the sweep above now runs over a
    // page that has no field on it at all -- which would pass on a section
    // that had a key box waiting one click away.
    const { container } = await renderDoors();
    await openVendorPanel("anthropic", "Anthropic");
    await openVendorPanel("openai", "OpenAI");
    for (const input of container.querySelectorAll("input")) {
      expect(input.getAttribute("type")).not.toBe("password");
      expect(input.getAttribute("aria-label") ?? "").not.toMatch(/key/i);
    }
    expect(screen.queryByLabelText(/api key/i)).toBeNull();
    // The reachable positive: the sweep really did have fields to look at.
    expect(container.querySelectorAll("input").length).toBeGreaterThan(0);
  });

  it("never offers a key as a route in the section's own copy", async () => {
    const { container } = await renderDoors();
    // The retired claim, verbatim. The 2026-08-22 record said OpenAI
    // published no federation; that was false when written -- OpenAI's has
    // been generally available since 2025-05-26.
    expect(container.textContent).not.toMatch(/publishes no federation/i);
    expect(container.textContent).not.toMatch(/paste[^.]{0,20}key/i);
    // Both halves of the position, said standing on the page rather than
    // behind a row: federation is the route for a vendor, and there is no key
    // anywhere. The list's first draft dropped the first clause.
    expect(container.textContent).toMatch(/federation is the only door/i);
    expect(container.textContent).toMatch(/no API key to enter/i);
  });
});

describe("Settings -> Doors: the try order is the product", () => {
  it("renders the four doors in the order a call tries them", async () => {
    await renderDoors();
    expect(doorOrder()).toEqual(["fleet", "app", "anthropic", "openai"]);
    // The same list, spelled once, is what every screen in this epic reads.
    expect(doorOrder()).toEqual([...DOOR_ORDER]);
    // And as a person reads it, which is the assertion that fails when a name
    // and its row come apart.
    expect(doorNamesInOrder()).toEqual([
      DOOR_NAMES.fleet,
      DOOR_NAMES.app,
      DOOR_NAMES.anthropic,
      DOOR_NAMES.openai,
    ]);
  });

  it("does not re-order itself on a local cluster", async () => {
    // THE RE-POINTED ONE. "Puts the fleet first, because on a local cluster it
    // is the only door" protected the fleet door's prominence where it is the
    // only door; the list makes it first for everyone, permanently, so the
    // assertion becomes that the order does not move. A screen that re-orders
    // to flatter the cluster's current state teaches that the metered door is
    // sometimes the recommendation, which is the opposite of the product.
    const local = await renderDoors("owner", "memql.localhost");
    expect(doorOrder()).toEqual(["fleet", "app", "anthropic", "openai"]);
    local.unmount();
    const cloud = await renderDoors("owner", "example.com");
    expect(doorOrder()).toEqual(["fleet", "app", "anthropic", "openai"]);
    cloud.unmount();
    // ...and while the domain is still unread, which is the state every
    // render passes through.
    await renderDoors("owner", "");
    expect(doorOrder()).toEqual(["fleet", "app", "anthropic", "openai"]);
  });

  it("states what every door costs, and says which two cost nothing", async () => {
    // THE COST LINE IS THE ARGUMENT, not decoration: it is the only place a
    // person learns, at the moment they are deciding, that the first two doors
    // spend nothing and the last two bill.
    await renderDoors();
    for (const id of DOOR_ORDER) {
      const cost = doorRow(id).querySelector(".os-doorrow-cost")?.textContent ?? "";
      expect(cost, id).toBe(DOOR_COST[id]);
      expect(cost, id).not.toBe("");
    }
    expect(doorRow("fleet").textContent).toMatch(/Nothing is billed/i);
    expect(doorRow("app").textContent).toMatch(/Nothing is billed here/i);
    expect(doorRow("anthropic").textContent).toMatch(/Billed per call/i);
    expect(doorRow("openai").textContent).toMatch(/Billed per call/i);
  });
});

describe("Settings -> Doors: the four doors", () => {
  it("renders a federated vendor, an unset one and the fleet, each distinctly", async () => {
    await renderDoors();
    expect(doorWordIn("openai")).toBe(DOOR_WORDS.open);
    expect(doorStateIn("openai")).toBe("open");
    expect(doorWordIn("anthropic")).toBe(DOOR_WORDS.unset);
    // RE-POINTED VALUE. The door vocabulary the list reads spells the shut
    // state `shut`; the WORD a person sees is unchanged, which is the half
    // this assertion was about.
    expect(doorStateIn("anthropic")).toBe("shut");
    expect(doorStateIn("fleet")).toBe("open");
    // Distinct is the whole requirement, and the word carries it in greyscale.
    expect(doorWordIn("openai")).not.toBe(doorWordIn("anthropic"));
  });

  it("styles an unset door as normal, not as a failure", async () => {
    h.state.providers = [];
    await renderDoors();
    const anthropic = doorRow("anthropic");
    expect(doorStateIn("anthropic")).toBe("shut");
    // Not an alert, not an error tone: a cluster with no federated vendor is
    // how every cluster is installed and the permanent state of every local
    // one. An operator who meets a red banner concludes the install failed.
    expect(within(anthropic).queryByRole("alert")).toBeNull();
    expect(anthropic.querySelector('[data-tone="error"]')).toBeNull();
    expect(anthropic.textContent).toMatch(/normal state of a new cluster/i);
    // And it does not open by listing what is blank. An untouched form has
    // failed nothing, and naming its empty fields on arrival is how the
    // normal state comes to read as a list of complaints.
    const panel = await openVendorPanel("anthropic", "Anthropic");
    expect(panel.textContent).not.toMatch(/Still needed/);
    expect(within(panel).queryByRole("alert")).toBeNull();
  });

  it("says what saving would do to a door that is already open", async () => {
    // An empty form under "Open" reads as unfinished. Rotating onto a
    // different service account is a real act and must stay reachable -- it
    // is just not the act the panel is about, so the caption says so.
    await renderDoors();
    // The act itself says which of the two it is, which is the row's whole
    // share of that distinction.
    expect(
      within(doorRow("openai")).getByRole("button", { name: "Edit OpenAI" }),
    ).toBeTruthy();
    const openai = await openVendorPanel("openai", "OpenAI");
    expect(openai.textContent).toMatch(/already in use/i);
    expect(openai.textContent).toMatch(/replaces them at the next Apply/i);
  });

  it("names the missing ids once somebody is filling the form in, and not before", async () => {
    h.state.providers = [];
    await renderDoors();
    const openai = await openVendorPanel("openai", "OpenAI");
    expect(openai.textContent).not.toMatch(/Still needed/);
    fireEvent.change(within(openai).getByLabelText("Identity provider id"), {
      target: { value: "idp_1" },
    });
    expect(openai.textContent).toMatch(/Still needed: Service account id/);
  });

  it("tells a half-set vendor to re-enter every id, because the write stores the set whole", async () => {
    // The engine's sentence above names two of three ids as already SET. A
    // form that then demanded only the third would create a set nobody had
    // checked; one that silently demanded all three would look broken.
    h.state.providers = [
      {
        name: "streamClaudeSonnet",
        vendor: "anthropic",
        model: "claude-sonnet-5",
        available: false,
        authSource: "unresolved",
        reason: HALF_SET_REASON,
      },
    ];
    await renderDoors();
    const anthropic = await openVendorPanel("anthropic", "Anthropic");
    expect(anthropic.textContent).toMatch(/re-enter every id/i);
    // And it must not contradict the engine by calling those two ids missing
    // before anybody has touched the form.
    expect(anthropic.textContent).not.toMatch(/Still needed/);
  });

  it("renders a half-configured door as the actionable one, in the engine's own words", async () => {
    h.state.providers = [
      {
        name: "streamClaudeSonnet",
        vendor: "anthropic",
        model: "claude-sonnet-5",
        available: false,
        authSource: "unresolved",
        reason: HALF_SET_REASON,
      },
    ];
    await renderDoors();
    expect(doorStateIn("anthropic")).toBe("half");
    expect(doorWordIn("anthropic")).toBe(DOOR_WORDS.half);
    const anthropic = doorRow("anthropic");
    // The consequence, said plainly ON THE ROW -- this is the state that takes
    // the fleet down at its next restart, hours after the save that caused it,
    // and a person scanning the list has to meet it without opening anything.
    expect(anthropic.textContent).toMatch(/refuses to boot/i);
    // And which ids, verbatim: the engine already names both halves better
    // than a re-derivation here would.
    expect(within(anthropic).getByText(/serviceAccountId missing/)).toBeTruthy();
  });
});

describe("Settings -> Doors: a vendor's form opens behind its row", () => {
  it("opens on the row's act and closes on the same one", async () => {
    h.state.providers = [];
    await renderDoors();
    // The standing page is four lines. Nothing is open until somebody asks.
    expect(screen.queryByRole("region", { name: "Anthropic" })).toBeNull();
    const act = () => within(doorRow("anthropic")).getByRole("button", { name: /Anthropic|Close/ });
    expect(act().getAttribute("aria-expanded")).toBe("false");

    fireEvent.click(act());
    await settle();
    expect(screen.getByRole("region", { name: "Anthropic" })).toBeTruthy();
    expect(act().getAttribute("aria-expanded")).toBe("true");
    expect(act().textContent).toBe("Close");

    fireEvent.click(act());
    await settle();
    expect(screen.queryByRole("region", { name: "Anthropic" })).toBeNull();
    expect(act().getAttribute("aria-expanded")).toBe("false");
  });

  it("holds one form at a time, so the page never grows two", async () => {
    h.state.providers = [];
    await renderDoors();
    await openVendorPanel("anthropic", "Anthropic");
    await openVendorPanel("openai", "OpenAI");
    expect(screen.queryByRole("region", { name: "Anthropic" })).toBeNull();
    expect(screen.getByRole("region", { name: "OpenAI" })).toBeTruthy();
  });
});

describe("Settings -> Doors: applying federation", () => {
  it("saves Anthropic's ids under its own vendor, without the optional workspace", async () => {
    await renderDoors();
    const anthropic = await openVendorPanel("anthropic", "Anthropic");
    for (const [label, value] of [
      ["Federation rule id", "fdrl_1"],
      ["Organization id", "org-1"],
      ["Service account id", "sa-1"],
    ] as const) {
      fireEvent.change(within(anthropic).getByLabelText(label), { target: { value } });
    }
    fireEvent.click(within(anthropic).getByRole("button", { name: "Save Anthropic federation" }));
    await settle();
    expect(h.state.federationCalls).toEqual([
      { vendor: "anthropic", ruleId: "fdrl_1", organizationId: "org-1", serviceAccountId: "sa-1" },
    ]);
    // The projected token path is a deployment fact, already base env beside
    // the volume it names. A box for it here could only disagree with the
    // mount, and a path that disagrees with the mount refuses boot.
    expect(h.state.federationCalls[0]?.["identityTokenFile"]).toBeUndefined();
    // A blank optional field is OMITTED rather than written empty: an empty
    // string is a value and "not set" is not.
    expect(h.state.federationCalls[0]?.["workspaceId"]).toBeUndefined();
  });

  it("saves OpenAI's two ids under its own vendor", async () => {
    // The other half of the vendor argument: one of the ids is spelled the
    // same for both vendors, so a write that did not say which vendor it was
    // could be read as either.
    h.state.providers = [];
    await renderDoors();
    const openai = await openVendorPanel("openai", "OpenAI");
    fireEvent.change(within(openai).getByLabelText("Identity provider id"), {
      target: { value: "idp_1" },
    });
    fireEvent.change(within(openai).getByLabelText("Service account id"), {
      target: { value: "svac_1" },
    });
    fireEvent.click(within(openai).getByRole("button", { name: "Save OpenAI federation" }));
    await settle();
    expect(h.state.federationCalls).toEqual([
      { vendor: "openai", identityProviderId: "idp_1", serviceAccountId: "svac_1" },
    ]);
  });

  it("will not save a partial set, and names what is still missing", async () => {
    h.state.providers = [];
    await renderDoors();
    const openai = await openVendorPanel("openai", "OpenAI");
    fireEvent.change(within(openai).getByLabelText("Identity provider id"), {
      target: { value: "idp_1" },
    });
    // One of the two required ids is still blank. A partial set REFUSES BOOT,
    // so accepting one here would take the fleet down at its next restart.
    expect(
      within(openai)
        .getByRole("button", { name: "Save OpenAI federation" })
        .hasAttribute("disabled"),
    ).toBe(true);
    expect(openai.textContent).toMatch(/Service account id/);
    expect(h.state.federationCalls).toEqual([]);
  });
});

describe("Settings -> Doors: a local cluster cannot federate", () => {
  it("withholds both forms and says why, rather than offering ids that cannot work", async () => {
    h.state.providers = [];
    await renderDoors("owner", "memql.localhost");
    for (const id of ["anthropic", "openai"] as const) {
      const row = doorRow(id);
      expect(doorStateIn(id)).toBe("shut");
      expect(doorWordIn(id)).toBe(DOOR_WORDS.closed);
      // RE-POINTED. The reason used to live in the standing panel; it is now
      // on the row, and the ACT that would open a form is ABSENT rather than
      // disabled -- DESIGN.md rule 12's position, applied to a control whose
      // only outcome would be an afternoon of work that cannot succeed.
      expect(within(row).queryByRole("button")).toBeNull();
      expect(row.textContent).toMatch(/local cluster/i);
      expect(row.textContent).toMatch(/no id typed here would ever be accepted/i);
    }
    // ...and there is no form anywhere on the surface to reach by any route.
    expect(screen.queryByRole("button", { name: /Save .* federation/ })).toBeNull();
    expect(screen.queryByLabelText(/Service account id/)).toBeNull();
  });

  it("offers the form while the domain is still unread, rather than guessing local", async () => {
    h.state.providers = [];
    await renderDoors("owner", "");
    const openai = await openVendorPanel("openai", "OpenAI");
    expect(within(openai).getByLabelText("Identity provider id")).toBeTruthy();
    expect(openai.textContent).not.toMatch(/local cluster/i);
  });
});

describe("Settings -> Doors: who may see it", () => {
  it("admits a developer and refuses a writer", async () => {
    // D7: a developer helps an owner through setup, so the four provider
    // builtins move to the owner-or-developer SET. It is a SET rather than a
    // floor because the ladder puts admin (200) BELOW developer (300) --
    // `{ min: "developer" }` would admit admin, whose concern is user
    // administration, and offer them a form the engine refuses field by field.
    expect(roleOpens("developer", DOORS_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("owner", DOORS_SECTION_RESOURCE)).toBe(true);
    expect(roleOpens("admin", DOORS_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("writer", DOORS_SECTION_RESOURCE)).toBe(false);
    expect(roleOpens("reader", DOORS_SECTION_RESOURCE)).toBe(false);
  });

  it("declares the same requirement in the manifest as in the section", () => {
    // Two copies of a gate is how they drift. This is the one that fails when
    // somebody widens one of them. The section id stays `providers` on
    // purpose -- deep links point at it by name.
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings?.sections?.find((s) => s.id === "providers")?.requires).toBe(
      DOORS_SECTION_RESOURCE,
    );
  });

  it("renders the whole surface for a developer session", async () => {
    await renderDoors("developer");
    expect(doorOrder()).toEqual(["fleet", "app", "anthropic", "openai"]);
    expect(screen.getByRole("region", { name: "What this node can call" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
    // And the vendor forms are reachable, which is the half a developer is
    // admitted here to do.
    expect(await openVendorPanel("anthropic", "Anthropic")).toBeTruthy();
  });
});

describe("Settings -> Doors: the registry it can reach", () => {
  it("says what can be called, and names the credential source per provider", async () => {
    await renderDoors();
    const registry = screen.getByRole("region", { name: "What this node can call" });
    expect(within(registry).getByText(/1 of 2 providers can be called./)).toBeTruthy();
    expect(within(registry).getByText(/workload identity/)).toBeTruthy();
    expect(within(registry).getByText(/nothing configured/)).toBeTruthy();
  });

  it("renders a vendor's refusal as the vendor's answer, not as a fault of ours", async () => {
    h.state.verifyReply = { verified: false, reason: "invalid x-api-key" };
    const { container } = await renderDoors();
    const registry = screen.getByRole("region", { name: "What this node can call" });
    fireEvent.click(
      within(registry).getByRole("button", { name: "Verify chat54Mini with the vendor" }),
    );
    await settle();
    const refusal = screen.getByText(/invalid x-api-key/);
    expect(refusal).toBeTruthy();
    // IN SURFACE, NEVER A TOAST. A refusal that floats away takes the vendor's
    // own words with it, and those words are the whole answer.
    expect(container.querySelector(".os-settings")?.contains(refusal)).toBe(true);
  });

  it("reaches no vendor until somebody presses Verify", async () => {
    await renderDoors();
    // Rendering must not spend a person's quota with a third party. The check
    // is a control, never something a panel does on open.
    expect(h.connection.query.providerVerify).not.toHaveBeenCalled();
  });

  it("reaches no vendor when a vendor's form is opened either", async () => {
    // The forms are behind a control now, and opening one is the moment a
    // "check it while we are here" call would look reasonable.
    await renderDoors();
    await openVendorPanel("anthropic", "Anthropic");
    expect(h.connection.query.providerVerify).not.toHaveBeenCalled();
  });

  it("renders a server refusal in surface, in the engine's own words", async () => {
    h.state.providerError = new Error("providerAuthStatus is owner-only");
    const { container } = await renderDoors("admin");
    // The section is a role SET in the manifest, so an admin should never
    // reach it -- but presentation is not the authorization, and if they do,
    // the engine's own sentence is what they read.
    expect(screen.getByText(/declined this read for admin/)).toBeTruthy();
    const detail = screen.getByText("providerAuthStatus is owner-only");
    expect(container.querySelector(".os-settings")?.contains(detail)).toBe(true);
  });

  it("keeps the fleet door standing when the provider read is refused", async () => {
    // Two readings, settling separately. A refusal of one must never decide
    // the state of the other -- they have different reasons to fail.
    h.state.providerError = new Error("providerAuthStatus is owner-only");
    await renderDoors("admin");
    expect(doorRow("fleet")).toBeTruthy();
    expect(doorStateIn("fleet")).toBe("open");
    expect(doorRow("fleet").textContent).toMatch(/32,000-token floor/);
  });

  it("says the fleet reading failed rather than calling the door shut", async () => {
    h.state.inferenceError = new Error("inferenceStatus: not connected");
    await renderDoors();
    expect(doorStateIn("fleet")).toBe("unknown");
    expect(doorWordIn("fleet")).toBe(DOOR_WORDS.unknown);
    expect(within(doorRow("fleet")).getByText("inferenceStatus: not connected")).toBeTruthy();
    // The other free door settles off the same read, so it says the same.
    expect(doorStateIn("app")).toBe("unknown");
  });

  it("separates saving from applying, and says what Apply did", async () => {
    await renderDoors();
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await settle();
    expect(h.state.reloadCalls).toBe(1);
    expect(screen.getByText(/can call 1 of 2 providers/)).toBeTruthy();
  });
});

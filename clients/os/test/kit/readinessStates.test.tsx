import type { ReactNode } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { OS_REGISTRY } from "../../src/apps/registry";
import { UNKNOWN_RUNTIME_CONFIG } from "../../src/cluster/config";
import {
  gateFor,
  markToneFor,
  SetupGroup,
  stateWords,
  SurfaceUnconfigured,
} from "../../src/kit/ReadinessStates";
import { canConfigure } from "../../src/kit/ReadinessStates";
import type { Readiness } from "../../src/live/readiness";
import { MODULE_SETTINGS_SECTION } from "../../src/system/modules";
import { sectionsFor } from "../../src/system/registry";
import { installSeededAccess } from "../seededAccess";
import type { Verdict } from "../../src/system/readinessFold";

function verdict(module: string, state: Verdict["state"], lanes: Verdict["lanes"] = []): Verdict {
  return { module, state, core: false, disagreement: [], nodes: [], lanes };
}

function readiness(loaded: boolean, verdicts: Verdict[]): Readiness {
  const by = new Map(verdicts.map((v) => [v.module, v]));
  return { loaded, state: "live", of: (id) => by.get(id) ?? null, reseed: () => {} };
}

/** The REAL shell provider, as every other suite mounts it. */
function withOs(node: ReactNode, role: string) {
  return (
    <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 12, rows: 8 }} layout="desktop">
      {node}
    </OsProvider>
  );
}

function withSession(node: ReactNode, role: string) {
  return (
    <SessionProvider
      value={{
        access: { userId: "u", primaryEmail: "u@example.com", role, roleName: "", rank: 0 },
        config: UNKNOWN_RUNTIME_CONFIG,
        ladderLoaded: true,
      }}
    >
      {node}
    </SessionProvider>
  );
}

describe("gateFor", () => {
  it("is unknown while the feed has not loaded, whatever is required", () => {
    expect(gateFor(readiness(false, []), ["storage"], []).state).toBe("unknown");
    expect(gateFor(undefined, ["storage"], []).state).toBe("unknown");
  });

  it("is unconfigured when a required module is unconfigured or unreported", () => {
    expect(gateFor(readiness(true, [verdict("storage", "unconfigured")]), ["storage"], []).unmet).toEqual([
      "storage",
    ]);
    expect(gateFor(readiness(true, []), ["storage"], []).state).toBe("unconfigured");
  });

  it("is partial when a required module is partial or a wanted one is not configured", () => {
    expect(gateFor(readiness(true, [verdict("storage", "partial")]), ["storage"], []).state).toBe(
      "partial",
    );
    expect(
      gateFor(
        readiness(true, [verdict("storage", "configured"), verdict("ai", "unconfigured")]),
        ["storage"],
        ["ai"],
      ).state,
    ).toBe("partial");
  });

  it("is ready when every required module is configured and nothing wanted is missing", () => {
    expect(gateFor(readiness(true, [verdict("storage", "configured")]), ["storage"], []).state).toBe(
      "ready",
    );
    expect(gateFor(readiness(true, []), [], []).state).toBe("ready");
  });

  // A wanted module NEVER gates, however badly it stands. This is the
  // distinction the two verbs exist for.
  it("never gates on a wanted module", () => {
    const g = gateFor(readiness(true, [verdict("storage", "unconfigured")]), [], ["storage"]);
    expect(g.state).toBe("partial");
    expect(g.unmet).toEqual([]);
    expect(g.wanted).toEqual(["storage"]);
  });
});

describe("markToneFor", () => {
  it("draws only while a person is needed", () => {
    expect(markToneFor({ state: "unknown", unmet: [], wanted: [] })).toBeNull();
    expect(markToneFor({ state: "ready", unmet: [], wanted: [] })).toBeNull();
    expect(markToneFor({ state: "partial", unmet: [], wanted: ["ai"] })).toBe("partlySetUp");
    expect(markToneFor({ state: "unconfigured", unmet: ["storage"], wanted: [] })).toBe("needsSetup");
  });
});

describe("stateWords", () => {
  it("tells not-reported apart from not-set-up", () => {
    expect(stateWords(null)).toBe("Not reported");
    expect(stateWords(verdict("ai", "unreported"))).toBe("Not reported");
    expect(stateWords(verdict("ai", "unconfigured"))).toBe("Not set up");
    expect(stateWords(verdict("ai", "partial"))).toBe("Partly set up");
    expect(stateWords(verdict("ai", "configured"))).toBe("Set up");
  });
});

describe("SurfaceUnconfigured", () => {
  it("offers the act to an owner and the sentence to everyone else", () => {
    const onSetUp = vi.fn();
    const { rerender } = render(
      <SurfaceUnconfigured
        surface="Campaigns"
        unmet={["email"]}
        descriptions={{ email: "Sending mail needs a mailbox this cluster can send from." }}
        canSetUp
        onSetUp={onSetUp}
      />,
    );
    expect(screen.getByRole("heading", { name: "Campaigns is not set up yet" })).toBeTruthy();
    expect(
      screen.getByText("Sending mail needs a mailbox this cluster can send from."),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Set up Campaigns" }));
    expect(onSetUp).toHaveBeenCalledTimes(1);

    rerender(
      <SurfaceUnconfigured
        surface="Campaigns"
        unmet={["email"]}
        descriptions={{}}
        canSetUp={false}
        onSetUp={onSetUp}
      />,
    );
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("An owner or developer can set it up in Settings.")).toBeTruthy();
  });

  it("renders one sentence per unmet module", () => {
    render(
      <SurfaceUnconfigured
        surface="Materializer"
        unmet={["ai", "storage"]}
        descriptions={{ ai: "Inference needs a provider.", storage: "Files live in blob storage." }}
        canSetUp={false}
        onSetUp={() => {}}
      />,
    );
    expect(screen.getByText("Inference needs a provider.")).toBeTruthy();
    expect(screen.getByText("Files live in blob storage.")).toBeTruthy();
  });
});

describe("SetupGroup", () => {
  it("lists each module with its state and points at the one place it is configured", () => {
    const r = readiness(true, [
      verdict("ai", "unconfigured"),
      verdict("storage", "configured", [
        { name: "azure-blob", configurableFrom: "deployment", complete: true, slots: [] },
      ]),
    ]);
    render(
      withSession(
        <SetupGroup app="Materializer" requires={["ai", "storage"]} wants={[]} readiness={r} />,
        "owner",
      ),
    );
    expect(screen.getByRole("heading", { name: "Set up" })).toBeTruthy();
    expect(screen.getByText("Inference")).toBeTruthy();
    expect(screen.getByText("Not set up")).toBeTruthy();
    expect(screen.getByText("Storage")).toBeTruthy();
    // "Set up" is both the group's heading and a row's state word, so this
    // asserts the ROW carries it rather than matching the heading again.
    const rowStates = Array.from(document.querySelectorAll(".os-setup-state")).map(
      (el) => el.textContent,
    );
    expect(rowStates).toContain("Set up");
    // No OsProvider in this test, so the act is the words, not a button --
    // a button with no window to open into would go nowhere.
    expect(screen.getByText("Settings, under Doors")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open Doors" })).toBeNull();
    // And the CONFIGURED module carries no act at all: "Set in the
    // deployment" beside a row that says "Set up" is an instruction with
    // nothing behind it.
    expect(screen.queryByText("Set in the deployment")).toBeNull();
  });

  it("names the variables for a deployment-only lane", () => {
    const r = readiness(true, [
      verdict("storage", "unconfigured", [
        {
          name: "azure-blob",
          configurableFrom: "deployment",
          complete: false,
          slots: [{ name: "MEMQL_AZURE_BLOB_CONTAINER", present: false, source: "unset" }],
        },
      ]),
    ]);
    render(
      withSession(<SetupGroup app="Files" requires={[]} wants={["storage"]} readiness={r} />, "developer"),
    );
    expect(screen.getByText("Set in the deployment")).toBeTruthy();
    expect(screen.getByText("MEMQL_AZURE_BLOB_CONTAINER")).toBeTruthy();
  });

  it("names only the slots that are still missing", () => {
    const r = readiness(true, [
      verdict("storage", "partial", [
        {
          name: "azure-blob",
          configurableFrom: "deployment",
          complete: false,
          slots: [
            { name: "MEMQL_AZURE_BLOB_CONTAINER", present: true, source: "env" },
            { name: "MEMQL_AZURE_STORAGE_CONNECTION_STRING", present: false, source: "unset" },
          ],
        },
      ]),
    ]);
    render(
      withSession(<SetupGroup app="Files" requires={[]} wants={["storage"]} readiness={r} />, "owner"),
    );
    expect(screen.getByText("MEMQL_AZURE_STORAGE_CONNECTION_STRING")).toBeTruthy();
    expect(screen.queryByText("MEMQL_AZURE_BLOB_CONTAINER")).toBeNull();
  });

  it("names the disagreeing nodes so a mid-rollout state reads as one", () => {
    const v = verdict("storage", "partial");
    v.disagreement = ["bff-b=unconfigured", "bff-a=configured"];
    render(
      withSession(<SetupGroup app="Files" requires={["storage"]} wants={[]} readiness={readiness(true, [v])} />, "owner"),
    );
    expect(screen.getByText(/bff-b=unconfigured, bff-a=configured/)).toBeTruthy();
  });

  it("says it is still reading rather than showing a set of wrong answers", () => {
    render(
      withSession(
        <SetupGroup app="Files" requires={["storage"]} wants={[]} readiness={readiness(false, [])} />,
        "owner",
      ),
    );
    expect(screen.getByText("Reading this cluster's setup.")).toBeTruthy();
    expect(screen.queryByText("Not set up")).toBeNull();
  });

  // THE ACT IS GATED ON REACHING THE SECTION, not just on being allowed to
  // configure -- and after epic memql#5088 that gate is INERT, which is worth
  // asserting rather than leaving as a silence.
  //
  // This test used to read: "offers a developer the words, not a button, for
  // an owner-only section", and its example was AI providers. That example is
  // gone. D7 of memql#5088 moved the providers section to owner-or-developer,
  // which is exactly the set canConfigure admits, so a developer who is shown
  // the group can now also reach the section it points at.
  //
  // The guard in ReadinessStates is still right and still asked of the
  // REGISTRY rather than restated. What changed is that no configuration
  // currently triggers it. So the test that used to exercise it becomes the
  // test that says WHY it cannot be exercised: every module the group can
  // offer must be reachable by every role the group is shown to. The day that
  // stops being true, this fails and names the pair, and the guard beside it
  // becomes load-bearing again.
  it("every role the group is shown to can reach every section it points at", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings, "the settings app must be in the registry").toBeTruthy();

    const configuringRoles = ["owner", "developer"] as const;
    const targets = Object.entries(MODULE_SETTINGS_SECTION).filter(([, t]) => t !== null);
    // A REACHABLE POSITIVE: an empty map would satisfy every assertion below.
    expect(targets.length, "no module points at a settings section").toBeGreaterThan(0);

    for (const role of configuringRoles) {
      expect(canConfigure(role), `${role} must be shown the group`).toBe(true);
      installSeededAccess(role);
      const reachable = new Set(sectionsFor(settings!).map((sec) => sec.id));
      for (const [moduleId, target] of targets) {
        expect(
          reachable.has(target!.section),
          `${role} is shown "Set up" for ${moduleId} but cannot reach Settings -> ${target!.section}; ` +
            `the button would navigate a window nowhere. Either widen that section or ` +
            `confirm the guard in ReadinessStates suppresses the button for this pair.`,
        ).toBe(true);
      }
    }
  });

  // The new fact D7 establishes, asserted directly rather than inferred from
  // the invariant above: a developer gets the BUTTON, not the words.
  it("offers a developer the button for AI providers", () => {
    const r = readiness(true, [verdict("ai", "unconfigured")]);
    render(
      withOs(
        withSession(
          <SetupGroup app="Nexus" requires={["ai"]} wants={[]} readiness={r} />,
          "developer",
        ),
        "developer",
      ),
    );
    expect(
      screen.queryByRole("button", { name: "Open Doors" }),
      "a developer may configure providers since memql#5088 D7, so the act is offered",
    ).not.toBeNull();
    expect(screen.queryByText("Settings, under Doors")).toBeNull();
  });

  it("renders nothing for a viewer", () => {
    const r = readiness(true, [verdict("ai", "unconfigured")]);
    const { container } = render(
      withSession(<SetupGroup app="Nexus" requires={["ai"]} wants={[]} readiness={r} />, "viewer"),
    );
    expect(container.textContent).toBe("");
  });

  it("renders nothing for an app that declares no modules", () => {
    const { container } = render(
      withSession(
        <SetupGroup app="Bin" requires={[]} wants={[]} readiness={readiness(true, [])} />,
        "owner",
      ),
    );
    expect(container.textContent).toBe("");
  });
});

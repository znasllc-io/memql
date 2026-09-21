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
  setAsideLabel,
  stateWords,
  verdictDetail,
  SurfaceUnconfigured,
} from "../../src/kit/ReadinessStates";
import { canConfigure } from "../../src/kit/ReadinessStates";
import type { Readiness } from "../../src/live/readiness";
import { MODULE_SETTINGS_SECTION } from "../../src/system/modules";
import { sectionsFor } from "../../src/system/registry";
import { installSeededAccess } from "../seededAccess";
import type { Verdict } from "../../src/system/readinessFold";

function verdict(module: string, state: Verdict["state"], lanes: Verdict["lanes"] = []): Verdict {
  return { module, state, core: false, disagreement: [], nodes: [], lanes, unknown: [], stale: [], aside: [] };
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

  // TWO CAUSES OF `partial`, TWO NAMES (memql#5259). Nodes that DIFFER --
  // some set up, some not -- are not a half-filled form, and "Partly set up"
  // sent the owner looking for one. A lane every node agrees is half-filled
  // keeps its old word.
  it("names nodes that differ apart from a half-filled setup", () => {
    const differ = verdict("ai", "partial");
    differ.nodes = [
      { nodeId: "a", nodeType: "agent", state: "configured", reportedAt: "" },
      { nodeId: "b", nodeType: "bff", state: "unconfigured", reportedAt: "" },
    ];
    expect(stateWords(differ)).toBe("Set up on some nodes");
    const halfEverywhere = verdict("storage", "partial");
    halfEverywhere.nodes = [
      { nodeId: "a", nodeType: "bff", state: "partial", reportedAt: "" },
      { nodeId: "b", nodeType: "bff", state: "unconfigured", reportedAt: "" },
    ];
    expect(stateWords(halfEverywhere)).toBe("Partly set up");
  });

  // TWO CAUSES OF `unreported`, TWO NAMES. Nodes that answered and could not
  // finish the check are not nodes that never answered, and neither is a
  // reason to send anybody to a form.
  it("names a check that failed apart from nobody answering", () => {
    const failed = verdict("ai", "unreported");
    failed.unknown = ["bff-a", "bff-b"];
    expect(stateWords(failed)).toBe("Could not check");
  });

  // A NODE BEHIND THE CLUSTER CHANGES NO WORD: the verdict reads exactly as it
  // would without it, and only the detail names it.
  it("does not let a stale node change the words", () => {
    const withStale = verdict("ai", "configured");
    withStale.stale = ["edge-a"];
    expect(stateWords(withStale)).toBe("Set up");
    expect(verdictDetail(withStale)).toMatch(/1 node catching up: edge-a\./);
    expect(setAsideLabel(withStale)).toBe("1 catching up");
    // A node that could not check outranks one catching up, and a verdict
    // whose own words say it gets no label beside them.
    withStale.unknown = ["bff-b"];
    expect(setAsideLabel(withStale)).toBe("1 could not check");
    const failed = verdict("ai", "unreported");
    failed.unknown = ["bff-b"];
    expect(setAsideLabel(failed)).toBe("");
  });

  it("keeps the detail to one line however many nodes there are", () => {
    const many = verdict("ai", "configured");
    many.unknown = ["n1", "n2", "n3", "n4", "n5"];
    expect(verdictDetail(many)).toBe("5 nodes could not check: n1, n2, n3 and 2 more. Not counted; each retries on its own.");
    expect(verdictDetail(verdict("ai", "configured"))).toBe("");
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

  // NODES THAT DIFFER READ AS WHAT THEY ARE, and the nodes are a hover away
  // (memql#5259). The raw `bff-b=unconfigured, bff-a=configured` pairs used to
  // be printed into the row itself -- a list of pod names beside a mark, on a
  // surface where a person reads the answer, not the fold.
  it("says set up on some nodes and puts the nodes in the title, not the row", () => {
    const v = verdict("storage", "partial");
    v.disagreement = ["bff-b=unconfigured", "bff-a=configured"];
    v.nodes = [
      { nodeId: "bff-b", nodeType: "bff", state: "unconfigured", reportedAt: "2026-09-13T11:58:00Z" },
      { nodeId: "bff-a", nodeType: "bff", state: "configured", reportedAt: "2026-09-13T11:58:00Z" },
    ];
    render(
      withSession(<SetupGroup app="Files" requires={["storage"]} wants={[]} readiness={readiness(true, [v])} />, "owner"),
    );
    const state = screen.getByText("Set up on some nodes");
    expect(state.closest("[title]")?.getAttribute("title")).toMatch(/Not set up on bff-b\./);
    expect(screen.queryByText(/bff-b=unconfigured/)).toBeNull();
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
    const configuringRoles = ["owner", "developer"] as const;
    const targets = Object.entries(MODULE_SETTINGS_SECTION).filter(([, t]) => t !== null);
    // A REACHABLE POSITIVE: an empty map would satisfy every assertion below.
    expect(targets.length, "no module points at a settings section").toBeGreaterThan(0);

    for (const role of configuringRoles) {
      expect(canConfigure(role), `${role} must be shown the group`).toBe(true);
      installSeededAccess(role);
      for (const [moduleId, target] of targets) {
        // THE TARGET'S OWN APP, because a module's home is not always Settings:
        // the GitHub App is registered from Deployables. An app id that is not
        // in the registry is the same defect as an unreachable section -- a
        // button that opens nothing -- so it fails here by name too.
        const app = OS_REGISTRY.apps.find((a) => a.id === target!.app);
        expect(app, `${moduleId} points at an app, ${target!.app}, that is not in the registry`).toBeTruthy();
        const reachable = new Set(sectionsFor(app!).map((sec) => sec.id));
        expect(
          reachable.has(target!.section),
          `${role} is shown "Set up" for ${moduleId} but cannot reach ${target!.place} -> ${target!.section}; ` +
            `the button would navigate a window nowhere. Either widen that section or ` +
            `confirm the guard in ReadinessStates suppresses the button for this pair.`,
        ).toBe(true);
      }
    }
  });

  // THE GITHUB APP IS CONFIGURED FROM DEPLOYABLES, not from the deployment and
  // not from Settings. The row used to name six MEMQL_GITHUB_APP_* variables --
  // on the same page as the control that now registers the app, so one page
  // said two different things about one module.
  describe("a module whose home is another app", () => {
    const unset = readiness(true, [
      verdict("githubApp", "unconfigured", [
        {
          name: "app",
          configurableFrom: "os",
          complete: false,
          slots: [{ name: "MEMQL_GITHUB_APP_ID", present: false, source: "unset" }],
        },
      ]),
    ]);

    it("opens that app's section, and never names deployment variables", () => {
      render(
        withOs(
          withSession(<SetupGroup app="Nexus" requires={[]} wants={["githubApp"]} readiness={unset} />, "owner"),
          "owner",
        ),
      );
      expect(screen.getByRole("button", { name: "Open Sources" })).toBeTruthy();
      expect(screen.queryByText("Set in the deployment")).toBeNull();
      expect(screen.queryByText("MEMQL_GITHUB_APP_ID")).toBeNull();
    });

    it("says where in words when there is no window to open", () => {
      render(withSession(<SetupGroup app="Nexus" requires={[]} wants={["githubApp"]} readiness={unset} />, "owner"));
      expect(screen.getByText("Deployables settings, under Sources")).toBeTruthy();
    });

    it("points down the page, with no button, when it is drawn on the page that configures it", () => {
      render(
        withOs(
          withSession(
            <SetupGroup
              app="Deployables"
              requires={[]}
              wants={["githubApp"]}
              readiness={unset}
              here={{ app: "deployables", section: "settings" }}
            />,
            "owner",
          ),
          "owner",
        ),
      );
      // "Open Sources" here would open the page somebody is already reading.
      expect(screen.getByText("Below, under Sources")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Open Sources" })).toBeNull();
    });
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

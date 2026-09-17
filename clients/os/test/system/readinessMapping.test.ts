import { describe, expect, it } from "vitest";

import { OS_REGISTRY } from "../../src/apps/registry";
import { allRequirementsFor, requirementsFor } from "../../src/system/registry";

// THE MAPPING (design record 2026-09-06-configuration-readiness, section 5.1),
// pinned so a later edit to one app cannot silently drop a gate. A dropped
// `requires` is invisible: the app opens and refuses one read at a time, which
// is the wall of refusals this epic exists to replace.

const app = (id: string) => {
  const found = OS_REGISTRY.apps.find((a) => a.id === id);
  if (!found) throw new Error(`no app ${id}`);
  return found;
};

describe("the readiness mapping (design record section 5.1)", () => {
  it("Campaigns permits preparation while requiring setup at the send controls", () => {
    expect(requirementsFor(app("campaigns"), "campaigns")).toEqual({
      requires: [],
      wants: ["email", "campaigns"],
    });
    expect(requirementsFor(app("campaigns"), "settings")).toEqual({ requires: [], wants: [] });
  });

  it("Materializer requires ai and storage", () => {
    expect(requirementsFor(app("materializer"), "composer").requires).toEqual(["ai", "storage"]);
  });

  it("Nexus requires ai on goals, runs and approvals, not on automations", () => {
    for (const s of ["goals", "runs", "approvals"]) {
      expect(requirementsFor(app("nexus"), s).requires).toEqual(["ai"]);
    }
    expect(requirementsFor(app("nexus"), "automations").requires).toEqual([]);
  });

  it("Fleet requires workbench on Workbenches and localApps on Apps only", () => {
    expect(requirementsFor(app("fleet"), "workbenches").requires).toEqual(["workbench"]);
    expect(requirementsFor(app("fleet"), "apps").requires).toEqual(["localApps"]);
    expect(requirementsFor(app("fleet"), "machines").requires).toEqual([]);
  });

  it("Deployables, Files, Training and Logs only want", () => {
    expect(requirementsFor(app("deployables"), "map")).toEqual({
      requires: [],
      wants: ["storage", "githubApp"],
    });
    expect(requirementsFor(app("files"), "browse")).toEqual({ requires: [], wants: ["storage"] });
    expect(requirementsFor(app("training"), "upload")).toEqual({ requires: [], wants: ["ai"] });
    // USERS DECLARES NOTHING AT ALL any more (epic memql#5167): the Invites
    // section is gone -- an invitation is a person who has not arrived, so the
    // roster carries them -- and the roster, the groups and the roles all read
    // fine with no mailbox. Only SENDING an invitation needs one, so the
    // People Head asks the question for itself and renders the gate's own
    // sentence where Invite would be. A section-level `wants` would have put a
    // setup mark on an app whose every screen works.
    expect(requirementsFor(app("users"), "people")).toEqual({ requires: [], wants: [] });
    expect(requirementsFor(app("users"), "groups")).toEqual({ requires: [], wants: [] });
    // The Logs app's own STREAM is its logsSection, which requirementsFor
    // exempts, so its want surfaces on the section that is not exempt. The
    // MARK still draws for the whole app: the window frame computes that from
    // the manifest's lists directly, not through this function.
    expect(requirementsFor(app("logs"), "search")).toEqual({ requires: [], wants: ["storage"] });
    expect(app("logs").wants).toEqual(["storage"]);
  });

  // The MARK's union, which is a different question from what gates a
  // section: is a person needed anywhere in this app. Fleet's is the case
  // worth pinning -- its two requirements sit on two different sections, and
  // neither may leak onto the other.
  it("the mark's union spans an app's sections without gating them", () => {
    expect(allRequirementsFor(app("fleet")).requires.sort()).toEqual(["localApps", "workbench"]);
    expect(requirementsFor(app("fleet"), "machines").requires).toEqual([]);
    expect(allRequirementsFor(app("nexus")).requires).toEqual(["ai"]);
    expect(requirementsFor(app("nexus"), "automations").requires).toEqual([]);
    expect(allRequirementsFor(app("users")).wants).toEqual([]);
  });

  it("the Ask widget requires ai", () => {
    expect(OS_REGISTRY.widgets.find((w) => w.id === "ask")?.needs).toEqual(["ai"]);
  });

  // A store row, a source, a deployable and a folder are CONTENT, not
  // configuration: these apps keep the empty states they have.
  it("apps that declare nothing declare nothing", () => {
    for (const id of ["stores", "accounts", "bin", "cluster", "concepts", "settings"]) {
      const a = app(id);
      expect(a.needs ?? []).toEqual([]);
      expect(a.wants ?? []).toEqual([]);
      for (const s of a.sections ?? []) {
        expect([...(s.needs ?? []), ...(s.wants ?? [])]).toEqual([]);
      }
    }
  });

  // No app may gate the one place its own repair lives. readinessProblem
  // refuses this at the contract; this checks the shipped registry.
  it("no app requires anything on its settings or logs section", () => {
    for (const a of OS_REGISTRY.apps) {
      expect(requirementsFor(a, a.settingsSection)).toEqual({ requires: [], wants: [] });
      expect(requirementsFor(a, a.logsSection)).toEqual({ requires: [], wants: [] });
    }
  });
});

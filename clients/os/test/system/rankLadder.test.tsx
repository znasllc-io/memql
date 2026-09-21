import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { RankMark, RoleTag } from "../../src/kit/RankMark";
import { PeerRowReadOnly, SurfaceRefused } from "../../src/kit/RankStates";
import { OS_REGISTRY } from "../../src/apps/registry";
import { appsFor, sectionsFor, widgetsFor } from "../../src/system/registry";
import { clearEffectiveCapabilities, roleAdmits, roleRank, setRoleLadder } from "../../src/system/roles";
import { installSeededAccess, seededAppResources } from "../seededAccess";
import { SEEDED_LADDER } from "../seededLadder";

// THE LADDER AS CLUSTER STATE (epic memql#4832, D1), and the surfaces that
// read it -- and, since epic memql#5289, the EFFECTIVE SET as cluster state
// beside it, which is what the app roster reads now.

beforeEach(() => {
  setRoleLadder(SEEDED_LADDER);
  installSeededAccess("owner");
});

describe("the ladder is cluster state", () => {
  // The whole point of D1: the shell holds no ordering of its own, so a
  // cluster that defines a custom role ranks it without a client release.
  it("ranks a custom role the cluster defines", () => {
    setRoleLadder([
      ...SEEDED_LADDER,
      { slug: "lead", name: "Lead", rank: 250, aliases: [] },
    ]);
    expect(roleRank("lead")).toBe(250);
    // 250 sits between admin (200) and developer (300) -- the spacing the
    // engine's ranks were given for exactly this.
    expect(roleAdmits("lead", { min: "admin" })).toBe(true);
    expect(roleAdmits("lead", { min: "developer" })).toBe(false);
    expect(roleAdmits("developer", { min: "lead" })).toBe(true);
  });

  // A deactivated or malformed rung must not silently become rank 0, which
  // every actor would clear.
  it("drops a rung it cannot rank rather than defaulting it", () => {
    setRoleLadder(SEEDED_LADDER);
    expect(roleRank("nosuchrole")).toBe(-1);
    expect(roleAdmits("owner", { min: "nosuchrole" })).toBe(false);
  });
});

describe("every shipped requirement names a seeded resource", () => {
  // THE CLIENT-SIDE TWIN of the Go parity gate
  // (component/memql/app_resource_os_parity_test.go). `requires:` is a plain
  // string, so a typo cannot be caught by the type system. It is caught here
  // instead: a resource no seed declares is held by nobody, so the surface
  // would vanish for everyone including the owner -- a silent and total
  // outage of one app.
  it("resolves every app, section and widget requirement against the seeds", () => {
    const seeded = new Set(seededAppResources());
    const unresolved: string[] = [];
    const check = (label: string, resource: string | undefined) => {
      if (resource === undefined) return;
      if (!seeded.has(resource)) unresolved.push(`${label} -> ${resource}`);
    };
    for (const app of OS_REGISTRY.apps) {
      check(`app ${app.id}`, app.requires);
      expect(app.requires, `app ${app.id} names its own door`).toBe(`app:${app.id}`);
      for (const section of app.sections ?? []) {
        check(`section ${app.id}/${section.id}`, section.requires);
        if (section.requires !== undefined) {
          expect(section.requires, `section ${app.id}/${section.id}`).toBe(`app:${app.id}/${section.id}`);
        }
      }
    }
    for (const widget of OS_REGISTRY.widgets) {
      check(`widget ${widget.id}`, widget.requires);
      expect(widget.requires, `widget ${widget.id} names its own door`).toBe(`app:${widget.id}`);
    }
    expect(unresolved).toEqual([]);
  });

  // The other direction: a seeded app resource nobody asks for gates
  // nothing and drifts.
  it("every seeded app resource is named by some manifest or section", () => {
    const named = new Set<string>();
    for (const app of OS_REGISTRY.apps) {
      named.add(app.requires);
      for (const section of app.sections ?? []) if (section.requires) named.add(section.requires);
    }
    for (const widget of OS_REGISTRY.widgets) named.add(widget.requires);
    expect(seededAppResources().filter((r) => !named.has(r))).toEqual([]);
  });

  // A REACHABLE POSITIVE for the cases above: the check can fail. Without
  // this, a registry that stopped declaring requirements would pass it.
  it("the check above can actually fail", () => {
    expect(seededAppResources()).not.toContain("app:definitely-not-an-app");
    expect(seededAppResources().length).toBeGreaterThan(0);
  });

  // NO `roles:` ANYWHERE. The retired form is not a type any more, but a
  // manifest object can still carry an extra key, and the Go gate refuses
  // the key by name -- this is the same refusal one language over.
  it("no manifest or section carries the retired roles floor", () => {
    const carrying: string[] = [];
    for (const app of OS_REGISTRY.apps) {
      if ("roles" in app) carrying.push(`app ${app.id}`);
      for (const section of app.sections ?? []) if ("roles" in section) carrying.push(`section ${app.id}/${section.id}`);
    }
    for (const widget of OS_REGISTRY.widgets) if ("roles" in widget) carrying.push(`widget ${widget.id}`);
    expect(carrying).toEqual([]);
  });
});

describe("each seeded role's desktop is the one the floors used to draw", () => {
  // THE PROMISE OF THE SWITCH (design section 5): the seeds reproduce the
  // hand-written floors exactly, so the day the shell reads the effective
  // set instead, nobody's desktop changes. These are the floors as the
  // registry stated them before the switch, written out per role, so a seed
  // that drifts fails here by name.
  //
  // `Stores` is gone from every row (epic memql#5530): the app is deleted and
  // the store a storefront fronts is configured on the storefront's own
  // deployable, behind `execute app:deployables/store` -- a PART, which is
  // not an app and so draws no icon on anybody's desktop. What that part
  // grants is asserted where parts are, not here.
  const expected: Record<string, { apps: string[]; not: string[] }> = {
    owner: { apps: ["Users", "Accounts", "Training", "Logs", "Cluster", "Concepts"], not: [] },
    developer: { apps: ["Users", "Accounts", "Training", "Logs", "Cluster", "Concepts"], not: [] },
    admin: { apps: ["Users", "Accounts", "Training", "Logs", "Cluster", "Concepts"], not: [] },
    writer: { apps: ["Training", "Deployables", "Files", "Settings"], not: ["Users", "Accounts", "Logs"] },
    reader: { apps: ["Deployables", "Files", "Settings"], not: ["Users", "Accounts", "Training", "Logs"] },
  };
  for (const [role, want] of Object.entries(expected)) {
    it(`draws ${role}'s desktop`, () => {
      installSeededAccess(role);
      const names = appsFor(OS_REGISTRY).map((a) => a.name);
      for (const name of want.apps) expect(names, `${role} should see ${name}`).toContain(name);
      for (const name of want.not) expect(names, `${role} should not see ${name}`).not.toContain(name);
    });
  }

  // The defect the ladder flip measured, now a fact about the seeds: a
  // developer opens the apps the engine considers them MORE privileged for.
  it("offers a developer the admin-floored SECTION as well", () => {
    const settings = OS_REGISTRY.apps.find((a) => a.id === "settings");
    expect(settings).toBeTruthy();
    installSeededAccess("developer");
    expect(sectionsFor(settings!).map((s) => s.id)).toContain("cluster");
    installSeededAccess("reader");
    expect(sectionsFor(settings!).map((s) => s.id)).not.toContain("cluster");
  });

  // Widgets are gated the same way apps are, and the desk context menu
  // reads the FILTERED list.
  it("filters widgets by the effective set", () => {
    installSeededAccess("owner");
    expect(widgetsFor(OS_REGISTRY).map((w) => w.id)).toEqual(["ask", "setup"]);
    installSeededAccess("reader");
    expect(widgetsFor(OS_REGISTRY).map((w) => w.id)).toEqual(["ask"]);
    clearEffectiveCapabilities();
    expect(widgetsFor(OS_REGISTRY)).toEqual([]);
  });
});

describe("the rung mark", () => {
  it("draws one tick per rung and lights the viewer's", () => {
    const { container } = render(<RankMark actorRole="developer" />);
    const ticks = container.querySelectorAll(".os-rank-tick");
    expect(ticks.length).toBe(SEEDED_LADDER.length);
    expect(container.querySelectorAll("[data-actor]").length).toBe(1);
    expect(container.querySelector("[role=img]")?.getAttribute("aria-label")).toContain("Developer");
  });

  // The mark grows with the cluster. A five-tick literal would have been the
  // easy version and could not draw this.
  it("draws a seventh tick for a cluster with seven rungs", () => {
    setRoleLadder([
      ...SEEDED_LADDER,
      { slug: "lead", name: "Lead", rank: 250, aliases: [] },
      { slug: "auditor", name: "Auditor", rank: 75, aliases: [] },
    ]);
    const { container } = render(<RankMark actorRole="lead" />);
    expect(container.querySelectorAll(".os-rank-tick").length).toBe(7);
  });

  // Two rungs lit is the whole reason the mark exists: it is what makes a
  // read-only peer row explain itself without prose.
  it("lights the owner's rung as well as the viewer's", () => {
    const { container } = render(<RankMark actorRole="developer" ownerRole="admin" />);
    expect(container.querySelectorAll("[data-actor]").length).toBe(1);
    expect(container.querySelectorAll("[data-owner]").length).toBe(1);
  });

  // NOTHING is drawn before the ladder lands. A mark drawn from no rungs would
  // be a confident picture of nothing.
  it("renders nothing before the ladder loads", () => {
    setRoleLadder([]);
    const { container } = render(<RankMark actorRole="owner" />);
    expect(container.querySelector(".os-rank-mark")).toBeNull();
  });

  it("sets a role slug in the data voice", () => {
    const { container } = render(<RoleTag role="developer" />);
    expect(container.querySelector(".os-role-slug")?.textContent).toBe("developer");
  });

  // A legacy slug resolves through its rung's aliases and renders as the rung.
  it("renders a legacy slug as the rung it aliases", () => {
    const { container } = render(<RoleTag role="writer" />);
    expect(container.querySelector(".os-role-slug")?.textContent).toBe("user");
  });
});

describe("the refused surface", () => {
  it("names the surface, the resource and where the viewer stands", () => {
    render(<SurfaceRefused surface="Accounts" resource="app:accounts" actorRole="reader" />);
    expect(screen.getByRole("heading", { name: /Accounts is not open to you/ })).toBeTruthy();
    const body = screen.getByText(/Opening it takes/);
    expect(within(body).getByText("read on app:accounts")).toBeTruthy();
    expect(within(body).getByText("viewer")).toBeTruthy();
    // NEVER "and above": what opens a surface is a capability, which a role,
    // a group or a grant to this person by name may hold, so no rung is the
    // answer and none is named.
    expect(body.textContent).not.toContain("and above");
  });

  // It says what resolves it. "Access denied" describes what already
  // happened; the person needs the next move, and the next move is a grant.
  it("names the action that resolves it", () => {
    render(<SurfaceRefused surface="Accounts" resource="app:accounts" actorRole="reader" />);
    expect(screen.getByText(/An owner or admin can grant it to you in Settings, under Access\./)).toBeTruthy();
  });

  // An unreported role is not a refusal about the person -- it is the shell
  // not knowing yet, and it must not be phrased as a verdict on them.
  it("distinguishes an unreported role from a low one", () => {
    render(<SurfaceRefused surface="Accounts" resource="app:accounts" actorRole="" />);
    expect(screen.getByText(/has not been reported by the cluster/)).toBeTruthy();
  });
});

describe("the read-only peer row", () => {
  it("says the row is somebody else's without naming a permission", () => {
    render(<PeerRowReadOnly actorRole="developer" />);
    expect(screen.getByText(/Read-only/)).toBeTruthy();
    // Deliberately absent: this is not a permission anybody can grant, so the
    // copy must not imply one exists to ask for.
    expect(screen.queryByText(/permission/i)).toBeNull();
    expect(screen.queryByText(/contact your admin/i)).toBeNull();
  });

  it("says peers are level when the owner's rung is known", () => {
    render(<PeerRowReadOnly actorRole="developer" ownerRole="developer" ownerName="Ada" />);
    expect(screen.getByText(/the same rank as you/)).toBeTruthy();
    expect(screen.getByText(/Ada owns this/)).toBeTruthy();
  });
});

import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import { RankMark, RoleTag } from "../../src/kit/RankMark";
import { PeerRowReadOnly, SurfaceRefused } from "../../src/kit/RankStates";
import { OS_REGISTRY } from "../../src/apps/registry";
import { appsForRole, sectionsForRole, widgetsForRole } from "../../src/system/registry";
import { roleAdmits, roleRank, setRoleLadder } from "../../src/system/roles";
import type { RoleRequirement } from "../../src/system/roles";
import { SEEDED_LADDER } from "../seededLadder";

// THE LADDER AS CLUSTER STATE (epic memql#4832, D1), and the surfaces that
// read it.

beforeEach(() => setRoleLadder(SEEDED_LADDER));

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

describe("every shipped role requirement names a real rung", () => {
  // THE CLIENT-SIDE TWIN of the engine's load-time @requiresRank validation.
  //
  // `RoleRequirement.min` is a plain string now -- it has to be, since the set
  // of roles is cluster state -- so a typo cannot be caught by the type
  // system. It is caught here instead: a floor naming no rung admits NOBODY,
  // so the surface would vanish for everyone including the owner, which is a
  // silent and total outage of one app.
  it("resolves every app, section and widget requirement", () => {
    const unresolved: string[] = [];
    // BOTH FORMS. `{ min }` names one slug; `{ any }` names a set, and every
    // member has to resolve -- a set with one unresolvable member silently
    // narrows rather than failing, which is the quieter of the two bugs.
    const check = (label: string, requirement: RoleRequirement | undefined) => {
      if (requirement === undefined) return;
      const slugs = "any" in requirement ? [...requirement.any] : [requirement.min];
      for (const slug of slugs) {
        if (roleRank(slug) < 0) unresolved.push(`${label} -> ${slug}`);
      }
    };
    for (const app of OS_REGISTRY.apps) {
      check(`app ${app.id}`, app.roles);
      for (const section of app.sections ?? []) {
        check(`section ${app.id}/${section.id}`, section.roles);
      }
    }
    for (const widget of OS_REGISTRY.widgets) {
      check(`widget ${widget.id}`, widget.roles);
    }
    expect(unresolved).toEqual([]);
  });

  // A REACHABLE POSITIVE for the case above: the check can fail. Without this,
  // a registry that stopped declaring requirements at all would pass it.
  it("the check above can actually fail", () => {
    expect(roleRank("definitely-not-a-role")).toBeLessThan(0);
    const gated = OS_REGISTRY.apps.filter((a) => a.roles !== undefined);
    expect(gated.length).toBeGreaterThan(0);
  });
});

describe("the flipped ordering reaches the registry", () => {
  // The defect, measured where it was visible: a developer could not see an
  // app the engine considered them MORE privileged for.
  it("offers a developer the admin-floored apps", () => {
    const forDeveloper = appsForRole(OS_REGISTRY, "developer").map((a) => a.name);
    expect(forDeveloper).toContain("Users");
    expect(forDeveloper).toContain("Accounts");
  });

  it("still withholds them from a writer and a reader", () => {
    for (const role of ["writer", "reader"]) {
      const apps = appsForRole(OS_REGISTRY, role).map((a) => a.name);
      expect(apps).not.toContain("Users");
      expect(apps).not.toContain("Accounts");
    }
  });

  it("offers the deploy tier the Actions section it used to hide", () => {
    const deployables = OS_REGISTRY.apps.find((a) => a.id === "deployables");
    expect(deployables).toBeTruthy();
    const ids = sectionsForRole(deployables!, "developer").map((s) => s.id);
    expect(ids).toContain("actions");
    expect(sectionsForRole(deployables!, "reader").map((s) => s.id)).not.toContain("actions");
  });

  // Widgets are role-gated the same way apps are, and the desk context menu
  // reads the FILTERED list now -- it read the raw registry, so a gated widget
  // would have been offered to everyone.
  it("filters widgets by role", () => {
    expect(widgetsForRole(OS_REGISTRY, "").length).toBe(
      OS_REGISTRY.widgets.filter((w) => w.roles === undefined).length,
    );
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
  it("names the surface, the floor and where the viewer stands", () => {
    render(<SurfaceRefused surface="Accounts" required="admin" actorRole="reader" />);
    expect(screen.getByRole("heading", { name: /Accounts needs a higher role/ })).toBeTruthy();
    const body = screen.getByText(/This app is open to/);
    expect(within(body).getByText("admin")).toBeTruthy();
    expect(within(body).getByText("viewer")).toBeTruthy();
  });

  // It says what resolves it. "Access denied" describes what already happened;
  // the person needs the next move.
  it("names the action that resolves it", () => {
    render(<SurfaceRefused surface="Accounts" required="admin" actorRole="reader" />);
    expect(screen.getByText(/An owner can change your role in Users\./)).toBeTruthy();
  });

  // An unreported role is not a refusal about the person -- it is the shell
  // not knowing yet, and it must not be phrased as a verdict on them.
  it("distinguishes an unreported role from a low one", () => {
    render(<SurfaceRefused surface="Accounts" required="admin" actorRole="" />);
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

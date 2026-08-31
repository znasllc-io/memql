import { describe, expect, it } from "vitest";

import { hiddenSurfaces } from "../../src/apps/settings/hiddenSurfaces";
import { OS_REGISTRY } from "../../src/apps/registry";

describe("the permissions self-view (memql#4744)", () => {
  it("lists admin- and writer-gated apps for a reader", () => {
    const hidden = hiddenSurfaces(OS_REGISTRY, "reader");
    const labels = hidden.map((h) => h.label);
    expect(labels).toContain("Users");
    expect(labels).toContain("Training");
    expect(labels).toContain("Settings -- Cluster");
    expect(hidden.find((h) => h.label === "Users")?.requires).toBe("admin");
    expect(hidden.find((h) => h.label === "Training")?.requires).toBe("writer");
  });

  it("a writer keeps Training and still loses the admin surfaces", () => {
    const labels = hiddenSurfaces(OS_REGISTRY, "writer").map((h) => h.label);
    expect(labels).not.toContain("Training");
    expect(labels).toContain("Users");
    expect(labels).toContain("Settings -- Cluster");
  });

  it("an admin loses nothing in this registry", () => {
    expect(hiddenSurfaces(OS_REGISTRY, "admin")).toEqual([]);
  });

  it("an owner shows nothing hidden", () => {
    expect(hiddenSurfaces(OS_REGISTRY, "owner")).toEqual([]);
  });

  it("does not enumerate the sections of an app that is itself hidden", () => {
    const labels = hiddenSurfaces(OS_REGISTRY, "reader").map((h) => h.label);
    expect(labels).toContain("Users");
    // "Users -- People" would say the same thing three times and bury the
    // informative case: a section gated above an app you CAN open.
    expect(labels.filter((l) => l.startsWith("Users --"))).toEqual([]);
  });

  it("names the cause when the actor's own role is unrankable", () => {
    // roleAdmits refuses to let an unknown role unlock anything gated, so an
    // empty role hides everything -- including surfaces that ask for
    // nothing. "requires none" would be true and useless there.
    const hidden = hiddenSurfaces(OS_REGISTRY, "");
    expect(hidden.length).toBeGreaterThan(0);
    expect(hidden.find((h) => h.label === "Users")?.requires).toBe("admin");
  });
});

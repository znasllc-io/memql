import { render, screen } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { setEffectiveCapabilities } from "../../src/system/roles";

const h = vi.hoisted(() => ({ runtime: vi.fn(), usersDirectory: vi.fn(), language: vi.fn(), facts: vi.fn() }));
vi.mock("../../src/kit", async importOriginal => ({
  ...await importOriginal<typeof import("../../src/kit")>(),
  useAppReach: () => ({ open: vi.fn() }),
}));
vi.mock("../../src/apps/cluster/readiness/ReadinessSection", () => ({
  ReadinessSection: () => { h.runtime(); return <p>Runtime readiness</p>; },
}));
vi.mock("../../src/apps/settings/ClusterSection", () => ({
  ClusterSection: () => { h.facts(); return <p>Recorded cluster facts</p>; },
}));
vi.mock("../../src/apps/settings/BenchmarksSection", () => ({ BenchmarksSection: () => <p>Measured benchmarks</p> }));
vi.mock("../../src/apps/settings/LanguageSection", () => ({
  LanguageSection: () => { h.language(); return <p>Language forms</p>; },
}));
vi.mock("../../src/apps/settings/AccessSection", () => ({ AccessSection: () => <p>Effective access</p> }));
vi.mock("../../src/apps/users/actions", () => ({ useUsersActions: () => { h.usersDirectory(); throw new Error("An Access-only reader must not start the Users directory"); } }));
import { ClusterApp } from "../../src/apps/cluster/ClusterApp";
import { ConceptsApp } from "../../src/apps/concepts/ConceptsApp";
import { UsersApp } from "../../src/apps/users/UsersApp";

const props = { navigate: vi.fn(), askContext: vi.fn() };
function grant(...resources: string[]) {
  setEffectiveCapabilities(resources.map(resource => ({ resource, verb: "read", effect: "allow" as const, source: "user" })));
}
beforeEach(() => { localStorage.clear(); vi.clearAllMocks(); });

it("opens the moved facts under the original grant without mounting runtime reads", () => {
  grant("app:settings", "app:settings/cluster");
  render(<ClusterApp {...props} sectionId="facts" />);
  expect(screen.getByText("Recorded cluster facts")).toBeTruthy();
  expect(h.runtime).not.toHaveBeenCalled();
});

it.each([["app:settings"], ["app:settings/cluster"], []])("does not add facts to Overview when one original door is absent (%j)", (...resources) => {
  grant("app:cluster", ...resources);
  render(<ClusterApp {...props} sectionId="overview" />);
  expect(screen.getByText("Runtime readiness")).toBeTruthy();
  expect(h.facts).not.toHaveBeenCalled();
});

it("adds read-only facts to Overview only after both original doors admit", () => {
  grant("app:cluster", "app:settings", "app:settings/cluster");
  render(<ClusterApp {...props} sectionId="overview" />);
  expect(screen.getByText("Runtime readiness")).toBeTruthy();
  expect(screen.getByText("Recorded cluster facts")).toBeTruthy();
});

it("opens benchmarks without mounting runtime reads", () => {
  grant("app:settings", "app:settings/benchmarks");
  render(<ClusterApp {...props} sectionId="benchmarks" />);
  expect(screen.getByText("Measured benchmarks")).toBeTruthy();
  expect(h.runtime).not.toHaveBeenCalled();
});

it("opens the same language surface from Concepts", () => {
  grant("app:settings", "app:settings/language");
  render(<ConceptsApp {...props} sectionId="language" />);
  expect(screen.getByText("Language forms")).toBeTruthy();
});

it("opens Access without starting unrelated Users directories", () => {
  grant("app:settings", "app:settings/access");
  render(<UsersApp {...props} sectionId="access" />);
  expect(screen.getByText("Effective access")).toBeTruthy();
  expect(h.usersDirectory).not.toHaveBeenCalled();
});

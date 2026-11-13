import { scrollNextPage } from "../scrollNextPage";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { SkillsSection, trainingSkill } from "../../src/apps/training/SkillsSection";

const state = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => state.connection }));
afterEach(cleanup);
const skill = { id: "software", name: "Software development", version: 2, active: true, status: "active", predefined: true, instructions: { body: "Current reviewed content." } };
const rows = (data: Record<string, unknown>[], cursor = "") => new Result({ data, meta: { hasMore: !!cursor, cursor } });
const catalog = (skills: Record<string, unknown>[], nextCursor = "") => rows([{ skills, nextCursor }]);
function connect(overrides: Record<string, unknown> = {}) {
  const query = {
    workSkillCatalogPage: vi.fn().mockResolvedValue(catalog([{ ...skill, reviewedBaseline: true }])),
    workSkillCatalogEntry: vi.fn().mockResolvedValue(rows([{ skill: { ...skill, reviewedBaseline: true } }])),
    workSkillSelectionsForSkill: vi.fn().mockResolvedValue(rows([])),
    ...overrides,
  };
  state.connection = { query };
  return query;
}

it("requires server-verified baseline provenance rather than a mutable catalog flag", () => {
  expect(trainingSkill(skill).predefined).toBe(false);
  expect(trainingSkill({ ...skill, reviewedBaseline: true }).predefined).toBe(true);
});

it("loads history only for the selected skill and preserves exact historical instructions", async () => {
  const historic = { id: "receipt", runId: "owned-run", stepKey: "fix", createdAt: "2026-10-13T00:00:00Z", data: { skillSelection: { schemaVersion: 1, selected: [{ id: "software", version: 1, text: "Original instructions used for this work.", reason: "matched-declared-terms", terms: ["software"], digest: "a".repeat(64) }], rejected: [] } } };
  const query = connect({ workSkillSelectionsForSkill: vi.fn().mockResolvedValue(rows([historic])) });
  render(<SkillsSection />);
  const button = await screen.findByRole("button", { name: /Software development/ });
  expect(query.workSkillCatalogEntry).not.toHaveBeenCalled();
  expect(query.workSkillSelectionsForSkill).not.toHaveBeenCalled();
  fireEvent.click(button);
  expect(await screen.findByText("Current reviewed content.")).toBeTruthy();
  const receipt = await screen.findByText(/Selected for work · Version 1/);
  fireEvent.click(receipt);
  expect(screen.getByText("Original instructions used for this work.")).toBeTruthy();
  expect(screen.getByText("owned-run")).toBeTruthy();
  expect(screen.getByText("a".repeat(64))).toBeTruthy();
  expect(query.workSkillSelectionsForSkill).toHaveBeenCalledWith({ skillId: "software" }, expect.anything());
});

it("reads ten summaries per click, retains pages on reopen and searches on the server", async () => {
  const first = Array.from({ length: 10 }, (_, n) => ({ id: `skill-${n}`, name: `Skill ${n}` }));
  const next = Array.from({ length: 10 }, (_, n) => ({ id: `skill-${n + 10}`, name: `Skill ${n + 10}` }));
  const read = vi.fn(async ({ search, cursor }: { search: string; cursor: string }) => search ? catalog([{ id: "remote", name: "Remote matching skill" }]) : catalog(cursor ? next : first, cursor ? "" : "older"));
  connect({ workSkillCatalogPage: read });
  const view = render(<SkillsSection />);
  await screen.findByRole("button", { name: /^Skill 9\b/ });
  expect(read).toHaveBeenCalledTimes(1);
  expect(screen.queryByRole("button", { name: /^Skill 10\b/ })).toBeNull();
  await scrollNextPage();
  await screen.findByRole("button", { name: /^Skill 19\b/ });
  expect(read.mock.calls[1]![0].cursor).toBe("older");
  view.unmount();
  render(<SkillsSection />);
  expect(screen.getByRole("button", { name: /^Skill 19\b/ })).toBeTruthy();
  expect(read).toHaveBeenCalledTimes(2);
  fireEvent.click(screen.getByRole("button", { name: "Search skills" }));
  fireEvent.change(screen.getByPlaceholderText("Search skills"), { target: { value: "Remote" } });
  await screen.findByRole("button", { name: /^Remote matching skill\b/ });
  expect(read.mock.calls[2]![0]).toEqual({ search: "Remote", cursor: "" });
  expect(screen.queryByRole("button", { name: /^Skill 19\b/ })).toBeNull();
  fireEvent.change(screen.getByPlaceholderText("Search skills"), { target: { value: "" } });
  await waitFor(() => expect(screen.getByRole("button", { name: /^Skill 19\b/ })).toBeTruthy());
  expect(read).toHaveBeenCalledTimes(3);
});

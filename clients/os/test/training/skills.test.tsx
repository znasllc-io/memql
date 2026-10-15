import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { SkillsSection, trainingSkill } from "../../src/apps/training/SkillsSection";

const state = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => state.connection }));
afterEach(cleanup);
const skill = { id: "software", name: "Software development", version: 2, active: true, status: "active", predefined: true, instructions: { body: "Current reviewed content." } };
const rows = (data: Record<string, unknown>[]) => new Result({ data, meta: { hasMore: false } });

it("requires server-verified baseline provenance rather than a mutable catalog flag", () => {
  expect(trainingSkill(skill).predefined).toBe(false);
  expect(trainingSkill({ ...skill, reviewedBaseline: true }).predefined).toBe(true);
});

it("shows exact historical instructions independently of today's catalog version", async () => {
  const historic = { id: "receipt", runId: "owned-run", stepKey: "fix", createdAt: "2026-10-13T00:00:00Z", data: { skillSelection: { schemaVersion: 1, selected: [{ id: "software", version: 1, text: "Original instructions used for this work.", reason: "matched-declared-terms", terms: ["software"], digest: "a".repeat(64) }], rejected: [] } } };
  state.connection = { query: {
    workSkillCatalog: vi.fn().mockResolvedValue(rows([{ skills: [{ ...skill, reviewedBaseline: true }] }])),
    workSkillSelectionsForOwner: vi.fn().mockResolvedValue(rows([historic])),
  } };
  render(<SkillsSection />);
  fireEvent.click(await screen.findByRole("button", { name: /Software development/ }));
  expect(screen.getByText("Current reviewed content.")).toBeTruthy();
  const receipt = screen.getByText(/Selected for work · Version 1/);
  fireEvent.click(receipt);
  expect(screen.getByText("Original instructions used for this work.")).toBeTruthy();
  expect(screen.getByText("owned-run")).toBeTruthy();
  expect(screen.getByText("a".repeat(64))).toBeTruthy();
});

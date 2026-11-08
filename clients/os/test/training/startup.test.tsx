import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { TrainingApp } from "../../src/apps/training/TrainingApp";
import { fakeConnection, settle, withSession } from "./harness";
afterEach(cleanup);

it("opening Skills requests one summary page without files, runs, domains or review history", async () => {
  const connection = fakeConnection();
  const read = vi.fn().mockResolvedValue(new Result({ data: [{ skills: [{ id: "s", name: "Available skill" }], nextCursor: "" }] }));
  connection.query.workSkillCatalogPage = read;
  h.connection = connection;
  const props = { navigate: vi.fn(), askContext: vi.fn() };
  const view = render(withSession(<TrainingApp {...props} sectionId="settings" />));
  await settle();
  expect(connection.calls).toEqual([]);
  expect(read).not.toHaveBeenCalled();
  view.rerender(withSession(<TrainingApp {...props} sectionId="skills" />));
  await screen.findByRole("button", { name: /Available skill/ });
  expect(read).toHaveBeenCalledTimes(1);
  expect(connection.calls).toEqual([]);
});

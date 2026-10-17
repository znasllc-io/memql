import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ active: vi.fn(), bind: vi.fn() }));
const query = { activeEmbedderBinding: h.active, bindEmbedder: h.bind };
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => ({ query }) }));
const { EmbeddingStatus } = await import("../../src/apps/fleet/models/EmbeddingStatus");
beforeEach(() => { h.active.mockReset().mockResolvedValue({ rows: () => [] }); h.bind.mockReset(); });
afterEach(cleanup);

it.each([{ rows: [] }, { rows: [{ providerRef: "fleet:embedding-model", activatedAt: "" }] }])("never offers assignment for an unbound or pending model", async ({ rows }) => {
  h.active.mockResolvedValue({ rows: () => rows });
  render(<EmbeddingStatus model="embedding-model" />);
  await waitFor(() => expect(h.active).toHaveBeenCalled());
  expect(screen.queryByRole("button")).toBeNull();
  expect(screen.queryByText("Used for memory search")).toBeNull();
  expect(h.bind).not.toHaveBeenCalled();
});

it("reports only the model that is actually bound", async () => {
  h.active.mockResolvedValue({ rows: () => [{ providerRef: "fleet:embedding-model", activatedAt: "2026-10-15T00:00:00Z" }] });
  const view = render(<EmbeddingStatus model="embedding-model" />);
  await screen.findByText("Used for memory search");
  expect(screen.queryByRole("button")).toBeNull();
  view.rerender(<EmbeddingStatus model="another-model" />);
  expect(screen.queryByText("Used for memory search")).toBeNull();
  expect(h.bind).not.toHaveBeenCalled();
});

it("can retry an unavailable status without making an assignment", async () => {
  h.active.mockRejectedValueOnce(new Error("Disconnected"));
  render(<EmbeddingStatus model="embedding-model" />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(h.active).toHaveBeenCalledTimes(2);
  expect(screen.queryByRole("button")).toBeNull();
  expect(h.bind).not.toHaveBeenCalled();
});

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ query: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => ({ query: h.query }) }));
const { EmbeddingStatus } = await import("../../src/apps/fleet/models/EmbeddingStatus");
const { builtinReply, generatedQuery, rowsResult } = await import("./harness");
// The binding rows the cluster answers; each test says which.
let bound: Row[] = [];
const query = generatedQuery({
  activeEmbedderBinding: async () => rowsResult(bound),
  // A tripwire: the status reports the binding and never makes one.
  bindEmbedder: async () => builtinReply("bindEmbedder", []),
});
h.query = query;
const active = query.activeEmbedderBinding;
const bind = query.bindEmbedder;
// mockReset puts back the implementation vi.fn was given: the generated method.
beforeEach(() => { bound = []; active.mockReset(); bind.mockReset(); });
afterEach(cleanup);

it.each([{ rows: [] }, { rows: [{ providerRef: "fleet:embedding-model", activatedAt: "" }] }])("never offers assignment for an unbound or pending model", async ({ rows }) => {
  bound = rows;
  render(<EmbeddingStatus model="embedding-model" />);
  await waitFor(() => expect(active).toHaveBeenCalled());
  expect(screen.queryByRole("button")).toBeNull();
  expect(screen.queryByText("Used for memory search")).toBeNull();
  expect(bind).not.toHaveBeenCalled();
});

it("reports only the model that is actually bound", async () => {
  bound = [{ providerRef: "fleet:embedding-model", activatedAt: "2026-10-15T00:00:00Z" }];
  const view = render(<EmbeddingStatus model="embedding-model" />);
  await screen.findByText("Used for memory search");
  expect(screen.queryByRole("button")).toBeNull();
  view.rerender(<EmbeddingStatus model="another-model" />);
  expect(screen.queryByText("Used for memory search")).toBeNull();
  expect(bind).not.toHaveBeenCalled();
});

it("can retry an unavailable status without making an assignment", async () => {
  active.mockRejectedValueOnce(new Error("Disconnected"));
  render(<EmbeddingStatus model="embedding-model" />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(active).toHaveBeenCalledTimes(2);
  expect(screen.queryByRole("button")).toBeNull();
  expect(bind).not.toHaveBeenCalled();
});

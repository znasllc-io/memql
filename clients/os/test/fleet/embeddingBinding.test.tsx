import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ active: vi.fn(), bind: vi.fn(), role: "owner" }));
const query = { activeEmbedderBinding: h.active, bindEmbedder: h.bind };
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => ({ query }) }));
vi.mock("../../src/chrome/access", () => ({ useSessionIfPresent: () => ({ access: { role: h.role } }) }));
const { EmbeddingBinding } = await import("../../src/apps/fleet/models/EmbeddingBinding");
beforeEach(() => { h.role = "owner"; h.active.mockReset().mockResolvedValue({ rows: () => [] }); h.bind.mockReset(); });
afterEach(cleanup);
it("activates only after the server succeeds and allows retry after failure", async () => {
 h.bind.mockRejectedValueOnce(new Error("Model unavailable")).mockResolvedValue({});
 render(<EmbeddingBinding model="embedding-model" online />);
 fireEvent.click(await screen.findByRole("button", { name: "Use for memory search" }));
 await screen.findByRole("alert");
 expect(screen.queryByText("Used for memory search")).toBeNull();
 fireEvent.click(screen.getByRole("button", { name: "Use for memory search" }));
 await screen.findByText("Used for memory search");
 expect(h.bind).toHaveBeenLastCalledWith({ provider: "fleet:embedding-model" });
});
it("lets a failed binding read recover without guessing the cluster state", async () => {
 h.active.mockRejectedValueOnce(new Error("Disconnected"));
 render(<EmbeddingBinding model="embedding-model" online />);
 fireEvent.click(await screen.findByRole("button", { name: "Retry" }));
 await screen.findByRole("button", { name: "Use for memory search" });
 expect(h.bind).not.toHaveBeenCalled();
});
it("withholds configuration for non-owners and offline models", async () => {
 h.role = "writer";
 const view = render(<EmbeddingBinding model="embedding-model" online />);
 await waitFor(() => expect(h.active).toHaveBeenCalled());
 expect(screen.queryByRole("button")).toBeNull();
 h.role = "owner"; view.rerender(<EmbeddingBinding model="embedding-model" online={false} />);
 expect((await screen.findByRole("button", { name: "Use for memory search" })).hasAttribute("disabled")).toBe(true);
});

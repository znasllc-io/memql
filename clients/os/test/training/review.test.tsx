import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { TrainingApp } from "../../src/apps/training/TrainingApp";
import { LocalTrainingSettingsStore } from "../../src/apps/training/settings";
import {
  chunkRow,
  click,
  domainLiteRow,
  fakeConnection,
  settle,
  withSession,
  type FakeConnection,
} from "./harness";

// The Review section: what the queue lists, what a decision writes, and what a
// refusal says.

function memStore() {
  const data = new Map<string, string>();
  return new LocalTrainingSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection) {
  h.connection = connection;
  return render(
    withSession(
      <TrainingApp
        sectionId="review"
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
    ),
  );
}

beforeEach(() => {
  h.connection = null;
});

describe("the queue", () => {
  it("lists ONLY unvalidated chunks, and only from domains that hold them", async () => {
    const connection = fakeConnection({
      // The rollup says domain-sales has work and domain-quiet does not.
      domainRows: [
        domainLiteRow("domain-sales", "unvalidated"),
        domainLiteRow("domain-sales", "validated"),
        domainLiteRow("domain-quiet", "validated"),
      ],
      chunkPages: {
        "domain-sales": [
          [
            chunkRow({ id: "c-open", text: "Net 30 terms apply." }),
            chunkRow({ id: "c-done", text: "Already approved.", validationStatus: "validated" }),
            chunkRow({ id: "c-old", text: "Retired content.", superseded: true }),
          ],
        ],
        "domain-quiet": [[chunkRow({ id: "c-quiet", text: "Nothing to do here." })]],
      },
    });
    mount(connection);
    await settle(6);

    expect(screen.getByText("Net 30 terms apply.")).toBeTruthy();
    // Decided and superseded chunks are not work.
    expect(screen.queryByText("Already approved.")).toBeNull();
    expect(screen.queryByText("Retired content.")).toBeNull();

    // THE PAYOFF OF THE SHAPE CHANGE: a domain the rollup says has no
    // unvalidated chunks is never paged at all.
    expect(connection.calls).toContain('query documentChunksForDomain(domainId: "domain-sales")');
    expect(connection.calls).not.toContain(
      'query documentChunksForDomain(domainId: "domain-quiet")',
    );
  });

  it("groups by documentId, with the seeded corpus LAST", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: {
        "domain-sales": [
          [
            chunkRow({ id: "c-1", documentId: "doc-terms", text: "From the terms." }),
            chunkRow({ id: "c-2", documentId: "", text: "From the corpus." }),
          ],
        ],
      },
    });
    mount(connection);
    await settle(6);

    const sections = [...document.querySelectorAll(".os-train-group")];
    expect(sections.map((s) => s.getAttribute("aria-label"))).toEqual([
      "doc-terms",
      "Seeded corpus",
    ]);
    expect(
      within(sections[1] as HTMLElement).getByText(/Chunks with no source document/),
    ).toBeTruthy();
  });

  it("says nothing is awaiting review when no domain holds work", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "validated")],
    });
    mount(connection);
    await settle(6);
    expect(screen.getByText("Nothing awaiting review.")).toBeTruthy();
    // A queue with nothing to walk must not read as one that never loaded.
    expect(connection.callsNamed("documentChunksForDomain")).toHaveLength(0);
  });

  it("counts the PAGES it read and never claims a total", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: {
        "domain-sales": [[chunkRow({ id: "c-1" })], [chunkRow({ id: "c-2" })]],
      },
    });
    mount(connection);
    await settle(6);
    expect(screen.getByText(/1 awaiting review in 1 page read/)).toBeTruthy();
  });

  it("walks the keyset pages on Load more", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: {
        "domain-sales": [
          [chunkRow({ id: "c-1", text: "Page one chunk." })],
          [chunkRow({ id: "c-2", text: "Page two chunk." })],
        ],
      },
    });
    mount(connection);
    await settle(6);
    expect(screen.getByText("Page one chunk.")).toBeTruthy();
    expect(screen.queryByText("Page two chunk.")).toBeNull();

    await click(screen.getByText("Load more"));
    await settle(6);

    expect(screen.getByText("Page two chunk.")).toBeTruthy();
    expect(screen.getByText(/2 awaiting review in 2 pages read/)).toBeTruthy();
    // Every page walked: the control goes away rather than offering a read
    // that would return nothing.
    expect(screen.queryByText("Load more")).toBeNull();
  });

  it("keeps pulling past a page with no work rather than reading as a dead button", async () => {
    // A domain's newest fifty can be entirely validated. A step that fetched
    // one page and added nothing would look like a control that did nothing.
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: {
        "domain-sales": [
          [chunkRow({ id: "c-a", validationStatus: "validated" })],
          [chunkRow({ id: "c-b", validationStatus: "validated" })],
          [chunkRow({ id: "c-c", text: "Buried, but found." })],
        ],
      },
    });
    mount(connection);
    await settle(8);
    expect(screen.getByText("Buried, but found.")).toBeTruthy();
    expect(screen.getByText(/in 3 pages read/)).toBeTruthy();
  });
});

describe("a decision", () => {
  it("calls setChunkValidationStatus and renders the MemQL the engine sees", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: { "domain-sales": [[chunkRow({ id: "c-1", text: "Net 30 terms apply." })]] },
    });
    mount(connection);
    await settle(6);

    await click(screen.getByLabelText("Approve chunk c-1"));
    await settle();

    // The GENERATED BUILDER ran: this is the text that reached the wire, not
    // an argument object a stub recorded.
    expect(connection.calls).toContain(
      'mutation setChunkValidationStatus(chunkId: "c-1", status: "validated")',
    );
  });

  it("rejects with the other enum member", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: { "domain-sales": [[chunkRow({ id: "c-1" })]] },
    });
    mount(connection);
    await settle(6);

    await click(screen.getByLabelText("Reject chunk c-1"));
    await settle();

    expect(connection.calls).toContain(
      'mutation setChunkValidationStatus(chunkId: "c-1", status: "rejected")',
    );
  });

  it("COLLAPSES the card in place from the reply", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: { "domain-sales": [[chunkRow({ id: "c-1", text: "Net 30 terms apply." })]] },
    });
    mount(connection);
    await settle(6);
    expect(screen.getByLabelText("Approve chunk c-1")).toBeTruthy();

    await click(screen.getByLabelText("Approve chunk c-1"));
    await settle();

    // The card keeps its slot -- nothing moves under the cursor -- and now
    // says what was decided.
    const card = document.querySelector(".os-train-card[data-decided]");
    expect(card).toBeTruthy();
    expect(card?.getAttribute("data-decided")).toBe("validated");
    expect(within(card as HTMLElement).getByText("validated")).toBeTruthy();
    expect(screen.queryByLabelText("Approve chunk c-1")).toBeNull();
  });

  it("renders a refusal ON THE CARD and does NOT collapse it", async () => {
    // An optimistic flip would be uncorrectable: `v1:knowledge:*` carries no
    // broadcast routing, so nothing would ever put the card back.
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "unvalidated")],
      chunkPages: { "domain-sales": [[chunkRow({ id: "c-1", text: "Net 30 terms apply." })]] },
      decisionError: "row_authz: refused",
    });
    mount(connection);
    await settle(6);

    await click(screen.getByLabelText("Approve chunk c-1"));
    await settle();

    expect(screen.getByText("row_authz: refused")).toBeTruthy();
    expect(document.querySelector(".os-train-card[data-decided]")).toBeNull();
    expect(screen.getByLabelText("Approve chunk c-1")).toBeTruthy();
  });
});

describe("when the domain read fails", () => {
  it("says the queue is empty because the READ failed, not because there is no work", async () => {
    const connection = fakeConnection({ domainsError: "domains unavailable" });
    mount(connection);
    await settle(6);
    expect(screen.getByText(/did not return its knowledge domains/)).toBeTruthy();
    expect(screen.getByText("domains unavailable")).toBeTruthy();
  });
});

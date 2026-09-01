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

// The Domains section: the chunk-derived rollup, the detail walk, and the two
// things it deliberately does not claim.

function memStore() {
  const data = new Map<string, string>();
  return new LocalTrainingSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection, navigate = vi.fn()) {
  h.connection = connection;
  const view = render(
    withSession(
      <TrainingApp
        sectionId="domains"
        navigate={navigate}
        askContext={vi.fn()}
        store={memStore()}
      />,
    ),
  );
  return { view, navigate };
}

beforeEach(() => {
  h.connection = null;
});

describe("the domain cards", () => {
  it("renders one card per domain with a rollup that SUMS to the count", async () => {
    const connection = fakeConnection({
      domainRows: [
        domainLiteRow("domain-sales", "unvalidated"),
        domainLiteRow("domain-sales", "validated"),
        domainLiteRow("domain-sales", "validated"),
        domainLiteRow("domain-sales", "rejected"),
        domainLiteRow("domain-hr", "validated"),
      ],
      chunkPages: { "domain-sales": [[chunkRow({ id: "c-1" })]] },
    });
    mount(connection);
    await settle();

    const sales = (await screen.findByText("domain-sales")).closest(".os-row") as HTMLElement;
    expect(within(sales).getByText("4 chunks")).toBeTruthy();
    expect(within(sales).getByText("1 unvalidated")).toBeTruthy();
    expect(within(sales).getByText("2 validated")).toBeTruthy();
    expect(within(sales).getByText("1 rejected")).toBeTruthy();
    // 1 + 2 + 1 = 4. A reader checking the arithmetic is the reader this is
    // for, so the test checks it too.
    expect(screen.getByText("domain-hr")).toBeTruthy();
  });

  it("builds the rollup from ONE pass, with no per-domain page walking", async () => {
    // The whole point of adding `validationStatus` to `documentChunkDomainLite`
    // (memql#4740). Before it, a breakdown could only be built by paging every
    // domain and counting the first 50 -- a number that looks like a total and
    // is not one.
    //
    // Every domain here is fully validated, which makes the assertion clean:
    // the app's review queue is held at the root and walks domains that have
    // work, so a fixture with work in it would read pages for a reason that
    // has nothing to do with this rollup.
    const connection = fakeConnection({
      domainRows: [
        domainLiteRow("domain-sales", "validated"),
        domainLiteRow("domain-hr", "validated"),
      ],
    });
    mount(connection);
    await settle();

    const sales = (await screen.findByText("domain-sales")).closest(".os-row") as HTMLElement;
    expect(within(sales).getByText("1 chunks")).toBeTruthy();
    expect(connection.callsNamed("allDocumentChunkDomains")).toHaveLength(1);
    expect(connection.callsNamed("documentChunksForDomain")).toHaveLength(0);
  });

  it("offers the empty cluster a way to the dropzone", async () => {
    const connection = fakeConnection({ domainRows: [] });
    const { navigate } = mount(connection);
    await settle();

    expect(screen.getByText(/No domains yet -- upload a file to start/)).toBeTruthy();
    await click(screen.getByText("Go to Upload"));
    // The app's OWN navigation. It never opens a window.
    expect(navigate).toHaveBeenCalledWith("upload");
  });

  it("says WHEN it was read, because this feed is not live", async () => {
    // `v1:knowledge:*` carries no broadcast routing rule, so a caption
    // claiming liveness would be claiming wiring that does not exist.
    const connection = fakeConnection({ domainRows: [domainLiteRow("d", "validated")] });
    mount(connection);
    await settle();
    expect(screen.getByText(/Chunk writes are not broadcast/)).toBeTruthy();
  });

  it("reports a failed read rather than an empty cluster", async () => {
    const connection = fakeConnection({ domainsError: "read refused" });
    mount(connection);
    await settle();
    expect(screen.getByText(/did not return its knowledge domains/)).toBeTruthy();
    expect(screen.getByText("read refused")).toBeTruthy();
    expect(screen.queryByText(/No domains yet/)).toBeNull();
  });
});

describe("a domain's detail", () => {
  it("groups by document, labels the seeded corpus, and pages", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "validated")],
      chunkPages: {
        "domain-sales": [
          [
            chunkRow({ id: "c-1", documentId: "doc-terms", text: "From the terms." }),
            chunkRow({ id: "c-2", documentId: "", text: "From the corpus." }),
          ],
          [chunkRow({ id: "c-3", documentId: "", text: "Page two." })],
        ],
      },
    });
    mount(connection);
    await settle();

    await click(screen.getByText("domain-sales"));
    await settle();

    expect(screen.getByText("doc-terms")).toBeTruthy();
    expect(screen.getByText("Seeded corpus")).toBeTruthy();
    expect(screen.getByText("2 chunks loaded.")).toBeTruthy();

    await click(screen.getByText("Load more"));
    await settle();
    expect(screen.getByText(/3 chunks loaded -- that is all of them/)).toBeTruthy();
  });

  it("marks a SUPERSEDED chunk without hiding it -- it is history, not work", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "validated")],
      chunkPages: {
        "domain-sales": [
          [
            chunkRow({
              id: "c-old",
              text: "Retired content.",
              superseded: true,
              supersededReason: "superseded by 2026 figures",
              validationStatus: "validated",
            }),
          ],
        ],
      },
    });
    mount(connection);
    await settle();
    await click(screen.getByText("domain-sales"));
    await settle();

    const row = screen.getByText("Retired content.").closest(".os-row") as HTMLElement;
    expect(row.getAttribute("data-dim")).toBe("true");
    // Supersession is a SEPARATE axis from validation: a validated chunk can
    // be out of retrieval, and the two chips say so independently.
    expect(within(row).getByText("superseded")).toBeTruthy();
    expect(within(row).getByText("validated")).toBeTruthy();
  });

  it("shows the attach-to-agents affordance as INERT, and says why", async () => {
    const connection = fakeConnection({
      domainRows: [domainLiteRow("domain-sales", "validated")],
      chunkPages: { "domain-sales": [[chunkRow({ id: "c-1" })]] },
    });
    mount(connection);
    await settle();
    await click(screen.getByText("domain-sales"));
    await settle();

    const inert = screen.getByText(/Attach to agents -- with the Agents surface/);
    // NOT a disabled button: a disabled button invites a click and answers
    // nothing. This is a label that says where the feature went.
    expect(inert.closest("button")).toBeNull();
    expect(inert.getAttribute("title")).toMatch(/skill\.domainIds/);
  });
});

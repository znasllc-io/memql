// useConcepts consuming the follow-mode registry-delta stream (memql#4238):
// the snapshot renders, live deltas update the list in place (sorted), and a
// generation gap re-snapshots. Driven against a fake QueryClient whose
// subscribeConceptRegistry hands the test the delta callback -- the reducer
// invariants themselves are pinned in registryDelta.test.ts.

import { describe, expect, it } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import {
  type Concept,
  type ConceptRegistryDelta,
  type ConceptRegistryFollow,
  type Connection,
} from "@znasllc-io/memql-sdk-core/client";

import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { useConcepts } from "../src/cluster/useConcepts";
import { asQueryClient } from "./support/queryFake";

function concept(id: string): Concept {
  const [version = "", domain = "", entity = ""] = id.split(":");
  return { id, version, domain, entity, description: "", type: "concept" };
}

function delta(partial: Partial<ConceptRegistryDelta>): ConceptRegistryDelta {
  return { generation: 0, added: [], removed: [], reset: false, ...partial };
}

interface Harness {
  emit: (d: ConceptRegistryDelta) => void;
  subscribeCount: () => number;
  unsubscribeCount: () => number;
}

function Probe(): ReactNode {
  const { concepts, loading } = useConcepts();
  return (
    <div>
      <span data-testid="loading">{loading ? "loading" : "ready"}</span>
      <ul data-testid="list">
        {concepts.map((c) => (
          <li key={c.id}>{c.id}</li>
        ))}
      </ul>
    </div>
  );
}

async function renderProbe(): Promise<Harness> {
  let onDelta: ((d: ConceptRegistryDelta) => void) | null = null;
  let subscribeCount = 0;
  let unsubscribeCount = 0;

  const query = asQueryClient({
    // Own property, so it shadows the real prototype method AND the
    // listConcepts adapter in asQueryClient: this test drives deltas directly.
    subscribeConceptRegistry: (cb: (d: ConceptRegistryDelta) => void): ConceptRegistryFollow => {
      subscribeCount += 1;
      onDelta = cb;
      return {
        unsubscribe: () => {
          unsubscribeCount += 1;
          onDelta = null;
        },
      };
    },
  });

  const dial = (async () =>
    ({
      nodeId: "bff-test",
      serverVersion: "0.0.0-test",
      engineVersion: "",
      query,
      subscriptions: null,
      dispatcher: null,
      close: () => {},
      done: () => new Promise<void>(() => {}),
    }) as unknown as Connection) as unknown as typeof Connection.dial;

  render(
    <ClusterProvider dial={dial}>
      <Probe />
    </ClusterProvider>,
  );

  // Wait for the dial to settle and the hook to open the subscription.
  await waitFor(() => expect(onDelta).not.toBeNull());

  return {
    emit: (d) => {
      act(() => {
        onDelta?.(d);
      });
    },
    subscribeCount: () => subscribeCount,
    unsubscribeCount: () => unsubscribeCount,
  };
}

function renderedIds(): string[] {
  return Array.from(screen.getByTestId("list").querySelectorAll("li")).map(
    (li) => li.textContent ?? "",
  );
}

describe("useConcepts (live registry)", () => {
  it("renders the snapshot sorted, then applies live add and remove in place", async () => {
    const h = await renderProbe();

    h.emit(delta({ generation: 1, reset: true, added: [concept("v1:b:b"), concept("v1:a:a")] }));
    await waitFor(() => expect(renderedIds()).toEqual(["v1:a:a", "v1:b:b"]));
    expect(screen.getByTestId("loading").textContent).toBe("ready");

    h.emit(delta({ generation: 2, added: [concept("v1:aa:mid")] }));
    await waitFor(() => expect(renderedIds()).toEqual(["v1:a:a", "v1:aa:mid", "v1:b:b"]));

    h.emit(delta({ generation: 3, removed: ["v1:a:a"] }));
    await waitFor(() => expect(renderedIds()).toEqual(["v1:aa:mid", "v1:b:b"]));
  });

  it("re-snapshots on a generation gap", async () => {
    const h = await renderProbe();
    expect(h.subscribeCount()).toBe(1);

    h.emit(delta({ generation: 1, reset: true, added: [concept("v1:a:a")] }));
    await waitFor(() => expect(renderedIds()).toEqual(["v1:a:a"]));

    // Skip generation 2 -> the hook detects the gap, unsubscribes and re-opens.
    h.emit(delta({ generation: 3, added: [concept("v1:b:b")] }));
    await waitFor(() => expect(h.subscribeCount()).toBe(2));
    expect(h.unsubscribeCount()).toBe(1);

    // The fresh subscription's snapshot renders the current set.
    h.emit(delta({ generation: 1, reset: true, added: [concept("v1:a:a"), concept("v1:b:b")] }));
    await waitFor(() => expect(renderedIds()).toEqual(["v1:a:a", "v1:b:b"]));
  });
});

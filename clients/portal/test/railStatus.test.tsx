// The rail footer says WHICH build this connection is serving (memql#4576).
//
// The footer's whole job is one question -- is this cluster running the code I
// think it is -- and until this it answered the word "dev" to every uncut
// build ever made. A developer who rebuilt an hour ago and one who installed
// last week read the same string.
//
// jsdom sees text, not pixels, so these tests are about the STRING. The layout
// is a screenshot question, as RailStatus's own header says.

import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

import type { Connection } from "@znasllc-io/memql-sdk-core/client";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { RailStatus, buildLabel } from "../src/components/RailStatus";
import { asQueryClient } from "./support/queryFake";

describe("buildLabel", () => {
  it("names a release and its commit", () => {
    expect(buildLabel("v0.19.5", "a1b2c3d4e5f6")).toBe("v0.19.5 · a1b2c3d4e5f6");
  });

  // THE CASE THE `includes` RULE EXISTS FOR. An uncut build states
  // `dev+<commit>` because the editor's recorded version has nowhere else to
  // carry the revision, and the same fact then arrives twice.
  it("does not print the commit twice when the version already carries it", () => {
    expect(buildLabel("dev+a1b2c3d4e5f6", "a1b2c3d4e5f6")).toBe("dev+a1b2c3d4e5f6");
  });

  it("keeps the dirty suffix, which is part of the revision", () => {
    expect(buildLabel("dev+a1b2c3d4e5f6-dirty", "a1b2c3d4e5f6-dirty")).toBe(
      "dev+a1b2c3d4e5f6-dirty",
    );
  });

  // An unknown revision renders as NOTHING -- not "unknown", not a trailing
  // separator. core/buildinfo's LogAttrs makes the same call for the same
  // reason: a field that looks answered and is not is worse than an absent one.
  it("renders no separator when the commit is unknown", () => {
    expect(buildLabel("v0.19.5", "")).toBe("v0.19.5");
    expect(buildLabel("dev", "")).toBe("dev");
    expect(buildLabel("dev", "   ")).toBe("dev");
  });

  // A node predating both fields. The word has to survive, because the footer
  // showing nothing at all would read as a footer that failed to load.
  it("still says dev when the node states no version", () => {
    expect(buildLabel("", "")).toBe("dev");
    expect(buildLabel("  ", "abcdef123456")).toBe("dev · abcdef123456");
  });
});

function withCluster(conn: Partial<Connection>): ReactNode {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "v1",
        query: asQueryClient({}),
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
        ...conn,
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;
  return (
    <ClusterProvider dial={dial}>
      <RailStatus collapsed={false} />
    </ClusterProvider>
  );
}

describe("RailStatus", () => {
  it("renders the release and the commit the node reported", async () => {
    render(withCluster({ engineVersion: "v0.19.5", engineCommit: "a1b2c3d4e5f6" }));
    await waitFor(() => expect(screen.getByText("v0.19.5 · a1b2c3d4e5f6")).toBeTruthy());
  });

  // The lane this epic exists for: a cluster built from a developer's checkout.
  it("renders an uncut build's own revision instead of the bare word", async () => {
    render(
      withCluster({ engineVersion: "dev+a1b2c3d4e5f6-dirty", engineCommit: "a1b2c3d4e5f6-dirty" }),
    );
    await waitFor(() => expect(screen.getByText("dev+a1b2c3d4e5f6-dirty")).toBeTruthy());
    // And exactly once -- the duplication guard, asserted on the rendered DOM
    // rather than only on the pure function.
    expect(screen.queryAllByText(/a1b2c3d4e5f6/)).toHaveLength(1);
  });

  it("falls back to dev against a node that states neither field", async () => {
    render(withCluster({}));
    await waitFor(() => expect(screen.getByText("dev")).toBeTruthy());
  });

  // Collapsed, the footer has no room for any of it, so the tooltip is the
  // only place an operator can read the build. It must carry the same string.
  it("carries the build into the collapsed tooltip", async () => {
    const dial = vi.fn(
      async () =>
        ({
          nodeId: "bff-test",
          serverVersion: "v1",
          engineVersion: "v0.19.5",
          engineCommit: "a1b2c3d4e5f6",
          query: asQueryClient({}),
          close: vi.fn(),
          done: vi.fn(() => new Promise<void>(() => {})),
        }) as unknown as Connection,
    ) as unknown as typeof Connection.dial;
    render(
      <ClusterProvider dial={dial}>
        <RailStatus collapsed={true} />
      </ClusterProvider>,
    );
    await waitFor(() => {
      const footer = document.querySelector("[data-rail-status]");
      expect(footer?.getAttribute("title")).toContain("v0.19.5 · a1b2c3d4e5f6");
    });
  });
});

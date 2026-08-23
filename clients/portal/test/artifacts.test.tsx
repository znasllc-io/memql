// The Artifacts page end to end (task 4 of the artifacts-labels epic), against
// a fake cluster -- modelled on test/campaignAuthoring.test.tsx: the fake sits
// at executeNamed (the wire boundary every generated typed method dispatches
// through), so a test here exercises the REAL composed call string rather
// than a hand-typed copy of it.
//
// WHAT THIS FILE OWNS. dsl conformance proves the library constructs are
// shaped and gated correctly; this file proves the browser sends the named
// calls the DSL declares and renders what comes back -- the list through
// RowList's declared display card (not markup this repo drew), the label
// filter living in the URL rather than in component state, and the label
// editor calling the two builtins rather than a set-the-whole-array mutation.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const ARTIFACT = "v1:library:artifact";

// The REAL display card, because that is what RowList composes against
// (dsl/library/concepts.memql:40).
const CONCEPTS: Concept[] = [
  {
    id: ARTIFACT,
    version: "v1",
    domain: "library",
    entity: "artifact",
    type: "concept",
    description: "A single Library index row",
    displayCard: { primary: "title", secondary: "summary", tertiary: "updatedAt", status: "lens" },
  },
];

// Wire-shaped rows: payload NESTED alongside the intrinsics, the same shape
// campaignAuthoring.test.tsx uses and for the same reason -- the flatten on
// the way into a Row is part of what is exercised.
function node(id: string, payload: Record<string, unknown>): Row {
  return {
    id,
    concept: ARTIFACT,
    type: "concept",
    createdBy: "user-1",
    createdAt: "2026-08-08T10:00:00Z",
    payload,
  };
}

const BIRDS = node("artifact-aaa", {
  ownerUserId: "user-1",
  lens: "artifact",
  kind: "generated_output",
  source: "agent_generated",
  sourceConceptRef: "v1:library:generatedOutput:out-1",
  title: "Ten most beautiful birds",
  summary: "A short list",
  labels: ["birds", "fun"],
  updatedAt: "2026-08-20T10:00:00Z",
});

const BUDGET = node("artifact-bbb", {
  ownerUserId: "user-1",
  lens: "artifact",
  kind: "document",
  source: "uploaded",
  sourceConceptRef: "v1:knowledge:document:doc-1",
  title: "Q3 budget",
  summary: "Numbers",
  labels: ["finance"],
  updatedAt: "2026-08-19T10:00:00Z",
});

const ARTIFACT_ROWS: Row[] = [BIRDS, BUDGET];

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

function rowLabelsOf(row: Row): string[] {
  const payload = row["payload"] as Record<string, unknown> | undefined;
  const labels = payload?.["labels"];
  return Array.isArray(labels) ? (labels as string[]) : [];
}

// argValue pulls one quoted arg value out of a composed call string, e.g.
// `label` out of `query libraryArtifactsByLabel(label: "finance")`. The fake
// receives only (name, call) -- executeNamed's real signature -- so this is
// the only way to make its reply depend on what was actually asked for.
function argValue(call: string, key: string): string | null {
  const match = new RegExp(`${key}: "([^"]*)"`).exec(call);
  return match ? (match[1] ?? null) : null;
}

function callsNamed(calls: readonly string[], construct: string): string[] {
  return calls.filter((call) => call.includes(`${construct}(`));
}

function renderAt(
  path: string,
  rows: Row[] = ARTIFACT_ROWS,
  options: { failArtifacts?: boolean } = {},
) {
  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.com",
    clusterRole: "owner",
  };

  const calls: string[] = [];
  const executeNamed = vi.fn(async (name: string, call: string) => {
    calls.push(call);
    switch (name) {
      case "libraryArtifacts":
        if (options.failArtifacts) throw new Error("engine unreachable");
        return new Result({ bundle: { nodes: rows }, meta: { cursor: "" } });
      case "libraryArtifactsByLabel": {
        const label = argValue(call, "label") ?? "";
        const narrowed = rows.filter((row) => rowLabelsOf(row).includes(label));
        return new Result({ bundle: { nodes: narrowed }, meta: { cursor: "" } });
      }
      case "libraryArtifactById": {
        const artifactId = argValue(call, "artifactId") ?? "";
        const found = rows.filter((row) => row["id"] === artifactId);
        return new Result({ bundle: { nodes: found }, meta: { cursor: "" } });
      }
      case "libraryAddArtifactLabel":
      case "libraryRemoveArtifactLabel":
      case "createGeneratedOutput":
        return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
      default:
        return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
    }
  });

  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  });

  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query,
        dispatcher: { sendAndWait: vi.fn(async () => ({})) },
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  const utils = render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the artifacts tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );

  return { ...utils, calls };
}

describe("the artifacts list", () => {
  it("renders artifacts through RowList's declared display card", async () => {
    renderAt("/artifacts");
    await waitFor(() =>
      expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0),
    );
    expect(screen.getAllByText("Q3 budget").length).toBeGreaterThan(0);
  });

  it("reads through the named query, not the generic concept browse", async () => {
    const { calls } = renderAt("/artifacts");
    await waitFor(() =>
      expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0),
    );
    expect(callsNamed(calls, "libraryArtifacts").length).toBeGreaterThan(0);
    expect(callsNamed(calls, "libraryArtifacts")[0]).toBe("query libraryArtifacts()");
  });

  it("narrows to a label by clicking its chip, and the chip is a URL", async () => {
    const { calls } = renderAt("/artifacts");
    await waitFor(() =>
      expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0),
    );

    fireEvent.click(screen.getByRole("button", { name: "finance" }));

    await waitFor(() => expect(screen.queryByText("Ten most beautiful birds")).toBeNull());
    expect(screen.getAllByText("Q3 budget").length).toBeGreaterThan(0);
    expect(
      callsNamed(calls, "libraryArtifactsByLabel").some((call) => call.includes('label: "finance"')),
    ).toBe(true);

    // The active chip is a toggle: clicking it again clears the filter and
    // the URL, and the other row comes back.
    fireEvent.click(screen.getByRole("button", { name: "finance" }));
    await waitFor(() =>
      expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0),
    );
  });

  it("renders the label-filtered list when the URL already names one", async () => {
    renderAt("/artifacts?label=finance");
    await waitFor(() => expect(screen.getAllByText("Q3 budget").length).toBeGreaterThan(0));
    expect(screen.queryByText("Ten most beautiful birds")).toBeNull();
  });

  it("shows the empty state when the caller has no artifacts", async () => {
    renderAt("/artifacts", []);
    await waitFor(() => expect(screen.getByText(/No artifacts yet/)).toBeTruthy());
  });

  it("shows an error state when the read fails", async () => {
    renderAt("/artifacts", ARTIFACT_ROWS, { failArtifacts: true });
    await waitFor(() => expect(screen.getByText(/Could not read artifacts/)).toBeTruthy());
  });

  it("drills into a row via RowList's onSelect, landing on the artifact's own URL", async () => {
    renderAt("/artifacts");
    await waitFor(() =>
      expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0),
    );
    fireEvent.click(screen.getByText("Ten most beautiful birds"));
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Ten most beautiful birds" })).toBeTruthy(),
    );
  });

  it("creates an artifact by minting a generatedOutput, not a bare index row", async () => {
    const { calls } = renderAt("/artifacts");
    await waitFor(() =>
      expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0),
    );

    fireEvent.change(screen.getByPlaceholderText("Ten most beautiful birds"), {
      target: { value: "September notes" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create artifact" }));

    await waitFor(() => expect(callsNamed(calls, "createGeneratedOutput").length).toBe(1));
    const call = callsNamed(calls, "createGeneratedOutput")[0] ?? "";
    expect(call.startsWith("mutation createGeneratedOutput(")).toBe(true);
    expect(call).toContain(`title: "September notes"`);
    // Never libraryArtifacts / createArtifact directly -- a bare index row has
    // nothing behind it and renders as broken (D5).
    expect(callsNamed(calls, "createArtifact").length).toBe(0);
    expect(await screen.findByText(/Library folds it into an artifact automatically/)).toBeTruthy();
  });
});

describe("the artifact detail", () => {
  it("renders at /artifacts/:artifactId", async () => {
    renderAt("/artifacts/artifact-aaa");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Ten most beautiful birds" })).toBeTruthy(),
    );
  });

  it("says an unknown id is not found rather than opening a blank page", async () => {
    renderAt("/artifacts/does-not-exist");
    await waitFor(() => expect(screen.getByText(/No artifact has that id/)).toBeTruthy());
  });

  it("adds a label through the add builtin", async () => {
    const { calls } = renderAt("/artifacts/artifact-aaa");
    await waitFor(() => expect(screen.getByText("birds")).toBeTruthy());

    const input = screen.getByLabelText("Add a label");
    fireEvent.change(input, { target: { value: "urgent" } });
    fireEvent.keyDown(input, { key: "Enter" });

    await waitFor(() => expect(callsNamed(calls, "libraryAddArtifactLabel").length).toBe(1));
    expect(callsNamed(calls, "libraryAddArtifactLabel")[0]).toBe(
      'builtin libraryAddArtifactLabel(artifactId: "artifact-aaa", label: "urgent")',
    );
    expect(screen.getByText("urgent")).toBeTruthy();
    // The page-level live region announces the change (LabelChips itself
    // renders none -- see ArtifactDetailPage's header comment).
    await waitFor(() => expect(screen.getByText('Added label "urgent".')).toBeTruthy());
  });

  it("removes a label through the remove builtin, not by resending the whole array", async () => {
    const { calls } = renderAt("/artifacts/artifact-aaa");
    await waitFor(() => expect(screen.getByText("birds")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Remove label birds" }));

    await waitFor(() => expect(callsNamed(calls, "libraryRemoveArtifactLabel").length).toBe(1));
    expect(callsNamed(calls, "libraryRemoveArtifactLabel")[0]).toBe(
      'builtin libraryRemoveArtifactLabel(artifactId: "artifact-aaa", label: "birds")',
    );
    await waitFor(() => expect(screen.queryByText("birds")).toBeNull());
    expect(screen.getByText("fun")).toBeTruthy();
    await waitFor(() => expect(screen.getByText('Removed label "birds".')).toBeTruthy());
  });
});

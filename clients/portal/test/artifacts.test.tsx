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

import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { ARTIFACTS_API_ROOT } from "../src/artifacts/transport";
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

// A kind=file artifact -- the sixth backing concept (memql#4340). Its
// sourceConceptRef is the ONLY pointer to the backing row: the index carries
// no fileId field, which is what src/artifacts/concepts.ts's
// fileIdFromSourceRef exists to recover.
const HANDBOOK = node("artifact-ccc", {
  ownerUserId: "user-1",
  lens: "artifact",
  kind: "file",
  source: "uploaded",
  sourceConceptRef: "v1:library:file:file-1",
  title: "Team handbook",
  summary: "Onboarding",
  labels: ["hr"],
  updatedAt: "2026-08-18T10:00:00Z",
});

// Already archived: absent from libraryArtifacts (the server excludes it) and
// present in the per-lens reads, exactly as the DSL has it.
const OLD_DECK = node("artifact-ddd", {
  ownerUserId: "user-1",
  lens: "artifact",
  kind: "generated_output",
  source: "agent_generated",
  sourceConceptRef: "v1:library:generatedOutput:out-9",
  title: "Last year's deck",
  summary: "Superseded",
  labels: ["finance"],
  archived: true,
  updatedAt: "2026-08-01T10:00:00Z",
});

const ARTIFACT_ROWS: Row[] = [BIRDS, BUDGET, HANDBOOK, OLD_DECK];

// The backing v1:library:file for HANDBOOK, shaped by libraryFileFull.
const HANDBOOK_FILE: Row = {
  id: "file-1",
  concept: "v1:library:file",
  type: "concept",
  createdBy: "user-1",
  createdAt: "2026-08-18T10:00:00Z",
  payload: {
    ownerUserId: "user-1",
    name: "handbook.pdf",
    mimeType: "application/pdf",
    size: 20480,
    status: "ready",
    embeddingStatus: "complete",
    trainedIntoDomainIds: ["hr-policies"],
  },
};

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

// The signed-in fixture. Upload is the one act on this page that needs a real
// credential -- it is an HTTP request with an Authorization header rather than
// a call on the stream -- so its tests run against an auth-ENABLED cluster
// whose refresh probe answers with a token, the shape authFlow.test.tsx uses.
const AUTH_ENABLED_CLUSTER = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "",
  oauthClientId: "memql-portal",
  authEnabled: true,
  domain: "example.com",
};

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

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

function payloadOf(row: Row): Record<string, unknown> {
  return row["payload"] as Record<string, unknown>;
}

function isArchivedNode(row: Row): boolean {
  return payloadOf(row)["archived"] === true;
}

// One similarity hit, in the builtin's own wire shape: an OBJECT node whose
// payload is integrations/library/similarity.go's similarArtifactHit. Not an
// artifact row -- which is exactly why the page renders it through its own
// ranked list rather than through the artifact display card.
function hitNode(row: Row, score: number, snippet: string): Row {
  const payload = payloadOf(row);
  return {
    id: `library:similar:${String(row["id"])}:1`,
    concept: "integration:library:result",
    type: "object",
    createdAt: "2026-08-20T10:00:00Z",
    payload: {
      artifactId: row["id"],
      fileId: "file-1",
      score,
      seq: 3,
      snippet,
      title: payload["title"],
      kind: payload["kind"],
      summary: payload["summary"],
      labels: payload["labels"],
    },
  };
}

interface RenderOptions {
  failArtifacts?: boolean;
  // Sign the operator in against an auth-ENABLED cluster, so the upload path
  // has a bearer to send. Off by default: every read on this page rides the
  // stream, which the fake dial answers regardless.
  signedIn?: boolean;
  // The hits librarySimilarArtifacts answers with, best first.
  hits?: Row[];
  files?: Row[];
}

function renderAt(path: string, seed: Row[] = ARTIFACT_ROWS, options: RenderOptions = {}) {
  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.com",
    clusterRole: "owner",
    // The session behind this connection. Always a string on the wire
    // (the server fills it from the verified claims, empty for a
    // credential with no session); a fixture has none.
    sessionId: "",
    displayName: "Ops Person",
  };

  // A per-render COPY, because archiveArtifact writes to it: a fixture shared
  // across tests that one test mutates is a test that passes or fails
  // depending on file order.
  const rows: Row[] = seed.map((row) => ({ ...row, payload: { ...payloadOf(row) } }));
  const files: Row[] = (options.files ?? [HANDBOOK_FILE]).map((row) => ({
    ...row,
    payload: { ...payloadOf(row) },
  }));

  const calls: string[] = [];
  const executeNamed = vi.fn(async (name: string, call: string) => {
    calls.push(call);
    switch (name) {
      case "libraryArtifacts":
        if (options.failArtifacts) throw new Error("engine unreachable");
        // The server-side archive exclusion, reproduced: libraryArtifacts
        // carries `archived != true` in its own filter, which is why the
        // page has to reach the per-lens reads to show archived rows at all.
        return new Result({
          bundle: { nodes: rows.filter((row) => !isArchivedNode(row)) },
          meta: { cursor: "" },
        });
      case "libraryArtifactsByLens": {
        const lens = argValue(call, "lens") ?? "";
        // Deliberately NOT archive-filtered -- the facet reads do not carry
        // that conjunct (see libraryArtifacts' comment in queries.memql).
        return new Result({
          bundle: { nodes: rows.filter((row) => payloadOf(row)["lens"] === lens) },
          meta: { cursor: "" },
        });
      }
      case "libraryArtifactsByLabel": {
        const label = argValue(call, "label") ?? "";
        // Also not archive-filtered, for the same reason: the page drops
        // archived rows from this read client-side.
        const narrowed = rows.filter((row) => rowLabelsOf(row).includes(label));
        return new Result({ bundle: { nodes: narrowed }, meta: { cursor: "" } });
      }
      case "libraryArtifactById": {
        const artifactId = argValue(call, "artifactId") ?? "";
        const found = rows.filter((row) => row["id"] === artifactId);
        return new Result({ bundle: { nodes: found }, meta: { cursor: "" } });
      }
      case "libraryFileById": {
        const fileId = argValue(call, "fileId") ?? "";
        return new Result({
          bundle: { nodes: files.filter((row) => row["id"] === fileId) },
          meta: { cursor: "" },
        });
      }
      case "libraryFilesForOwner":
        return new Result({ bundle: { nodes: files }, meta: { cursor: "" } });
      case "librarySimilarArtifacts":
        return new Result({ bundle: { nodes: options.hits ?? [] }, meta: { cursor: "" } });
      case "libraryTrainFile": {
        const fileId = argValue(call, "fileId") ?? "";
        const domainId = argValue(call, "domainId") ?? "";
        for (const file of files) {
          if (file["id"] !== fileId) continue;
          const current = (payloadOf(file)["trainedIntoDomainIds"] as string[]) ?? [];
          if (!current.includes(domainId)) {
            payloadOf(file)["trainedIntoDomainIds"] = [...current, domainId];
          }
        }
        return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
      }
      case "archiveArtifact": {
        const artifactId = argValue(call, "artifactId") ?? "";
        for (const row of rows) {
          if (row["id"] === artifactId) payloadOf(row)["archived"] = true;
        }
        return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
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
        config={options.signedIn ? AUTH_ENABLED_CLUSTER : AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          if (options.signedIn) return jsonResponse({ access_token: "AT-1", expires_in: 900 });
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

  return { ...utils, calls, rows, files };
}

// ---------------------------------------------------------------------------
// The upload's fake XMLHttpRequest.
//
// A stubbed GLOBAL rather than an injected factory: transport.ts's
// newRequest() is the one line that runs in production, and a seam would leave
// exactly that line untested while every test exercised a path no browser
// takes.
// ---------------------------------------------------------------------------

interface XhrScript {
  status?: number;
  body?: string;
  // undefined -> "application/json". null -> no header at all, which is what
  // an SPA-fallback HTML answer looks like to this code.
  contentType?: string | null;
  networkError?: boolean;
  progress?: Array<{ loaded: number; total: number }>;
  // Fire the progress events but DO NOT settle until finish() is called. The
  // only way to observe a mid-flight frame: a request that completes in the
  // same tick renders once, at 100%, and a progress test that asserted on
  // that would pass with the progress handler deleted.
  hold?: boolean;
}

interface SentRequest {
  url: string;
  headers: Record<string, string>;
  body: FormData;
}

function installFakeXhr(script: XhrScript = {}): { sent: SentRequest[]; finish: () => void } {
  const sent: SentRequest[] = [];
  let settle: (() => void) | null = null;
  class FakeXhr {
    upload = { onprogress: null as ((event: unknown) => void) | null };
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    status = 0;
    responseText = "";
    private url = "";
    private headers: Record<string, string> = {};

    open(_method: string, url: string): void {
      this.url = url;
    }

    setRequestHeader(name: string, value: string): void {
      this.headers[name] = value;
    }

    getResponseHeader(name: string): string | null {
      if (name.toLowerCase() !== "content-type") return null;
      return script.contentType === undefined ? "application/json" : script.contentType;
    }

    send(body: FormData): void {
      sent.push({ url: this.url, headers: this.headers, body });
      for (const step of script.progress ?? []) {
        this.upload.onprogress?.({ lengthComputable: true, loaded: step.loaded, total: step.total });
      }
      const complete = () => {
        if (script.networkError) {
          this.onerror?.();
          return;
        }
        this.status = script.status ?? 201;
        this.responseText =
          script.body ?? JSON.stringify({ artifactId: "artifact-new", fileId: "file-new" });
        this.onload?.();
      };
      if (script.hold) settle = complete;
      else complete();
    }
  }
  vi.stubGlobal("XMLHttpRequest", FakeXhr);
  return {
    sent,
    finish: () => {
      const pending = settle;
      settle = null;
      pending?.();
    },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

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

  // Fix round 1 (CRITICAL/IMPORTANT review findings). Deliberately no
  // preceding await/waitFor before the first assertion in each of these two
  // tests: render() returns before the fake connection's dial() has
  // resolved, so `query` is genuinely still null at that exact point --
  // true on every mount, not a rare race. Before the fix, useArtifacts'
  // `loading` initialized to false AND its query===null effect branch
  // re-asserted false, so this frame rendered the empty state on every
  // single visit to /artifacts, including a perfectly populated one. These
  // tests fail against that code and pass against the fix (verified by
  // temporarily reverting useArtifacts.ts to its pre-fix-round-1 content and
  // re-running).
  it("does not flash the empty state before the read has even started", async () => {
    renderAt("/artifacts");
    expect(screen.queryByText(/No artifacts yet/)).toBeNull();
    // Drain the pending connect + read so no async work is left dangling
    // for a later test to trip over.
    await waitFor(() =>
      expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0),
    );
  });

  it("does not flash the label-filtered empty state before the read has even started", async () => {
    renderAt("/artifacts?label=finance");
    expect(screen.queryByText(/No artifacts labelled/)).toBeNull();
    await waitFor(() => expect(screen.getAllByText("Q3 budget").length).toBeGreaterThan(0));
  });

  it("shows an error state when the read fails", async () => {
    renderAt("/artifacts", ARTIFACT_ROWS, { failArtifacts: true });
    // The PLAIN SENTENCE is what everybody gets (memql#4653) ...
    await waitFor(() => expect(screen.getByText(/Could not read your Library/)).toBeTruthy());
    // ... and the raw string is filed rather than thrown away. This fixture
    // signs in as an owner, so the disclosure is in the tree -- collapsed,
    // which is a rendering fact rather than a DOM one.
    //
    // AWAITED, because the role arrives over the connection: ErrorNotice
    // renders with no disclosure until it does, deliberately, so nothing
    // flashes internals at somebody mid-handshake.
    await waitFor(() => expect(screen.getByText("Technical details")).toBeTruthy());
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
    // Fix round 1 (CRITICAL): "derived" persists into the artifact index
    // row's own `source` field and renders as its permanent provenance
    // label -- a person's own portal-authored artifact must never carry a
    // label claiming a tool computed it from another artifact.
    expect(call).toContain(`source: "user_created"`);
    expect(call).not.toContain(`source: "derived"`);
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

  // Fix round 1: same shape as the list page's pair above, and the same
  // reason -- useArtifactDetail's query===null branch used to force
  // loading=false and artifact=null together, so a perfectly valid id
  // rendered "No artifact has that id" on every visit, for the entire
  // window before the connection (let alone the read) resolved.
  it("does not flash 'not found' before the read has even started", async () => {
    renderAt("/artifacts/artifact-aaa");
    expect(screen.queryByText(/No artifact has that id/)).toBeNull();
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

// ===========================================================================
// UPLOAD (memql#4343)
// ===========================================================================
//
// The one act on this page that does NOT ride the stream. multipart is the
// documented gRPC exception, so these tests are about an HTTP request: where
// it goes, what it carries, and what each failure says. The engine-side
// contract they encode is component/server/artifact_handler.go's.

describe("uploading a file", () => {
  function pick(name: string, type: string): File {
    return new File(["the bytes"], name, { type });
  }

  it("posts multipart to the Library's byte route, with the connection's bearer", async () => {
    const { sent } = installFakeXhr();
    renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    await waitFor(() => expect(screen.getByLabelText("File")).toBeTruthy());

    fireEvent.change(screen.getByPlaceholderText("finance, q3"), { target: { value: "finance, q3" } });
    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("notes.txt", "text/plain")] },
    });

    await waitFor(() => expect(sent).toHaveLength(1));
    const request = sent[0]!;
    // The `/_memql` marker, not a bare /artifacts: the portal is served by
    // the edge, whose SPA fallback would answer a bare path with index.html.
    expect(request.url).toBe(ARTIFACTS_API_ROOT);
    expect(request.url).toContain("/_memql/artifacts");
    expect(request.headers["Authorization"]).toBe("Bearer AT-1");
    // Never a hand-set Content-Type -- the boundary is the browser's to write.
    expect(Object.keys(request.headers)).toEqual(["Authorization"]);
    const file = request.body.get("file");
    expect(file instanceof File ? file.name : "").toBe("notes.txt");
    expect(request.body.get("labels")).toBe("finance,q3");
  });

  it("accepts ANY type -- there is no allowlist and no 415 to handle", async () => {
    const { sent } = installFakeXhr();
    renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    await waitFor(() => expect(screen.getByLabelText("File")).toBeTruthy());

    // No `accept` attribute: a picker that filtered would be claiming a
    // restriction the route does not have (design 3.4 -- an unknown type is
    // stored opaquely, never refused).
    expect(screen.getByLabelText("File").getAttribute("accept")).toBeNull();

    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("thing.weird", "application/x-not-a-real-type")] },
    });

    await waitFor(() => expect(sent).toHaveLength(1));
    const file = sent[0]!.body.get("file");
    expect(file instanceof File ? file.name : "").toBe("thing.weird");
  });

  it("takes a dropped file as well as a picked one", async () => {
    const { sent } = installFakeXhr();
    renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    const zone = await screen.findByRole("group", { name: "Drop a file to upload" });

    fireEvent.drop(zone, { dataTransfer: { files: [pick("dropped.pdf", "application/pdf")] } });

    await waitFor(() => expect(sent).toHaveLength(1));
    const file = sent[0]!.body.get("file");
    expect(file instanceof File ? file.name : "").toBe("dropped.pdf");
  });

  it("re-reads the list once the 201 lands, so the row appears without a manual refresh", async () => {
    installFakeXhr();
    const { calls } = renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    await waitFor(() => expect(callsNamed(calls, "libraryArtifacts").length).toBe(1));

    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("notes.txt", "text/plain")] },
    });

    await waitFor(() => expect(callsNamed(calls, "libraryArtifacts").length).toBe(2));
    expect(await screen.findByText(/Uploaded "notes.txt"/)).toBeTruthy();
  });

  it("reports progress while the bytes go up, not only when they land", async () => {
    const { finish } = installFakeXhr({ hold: true, progress: [{ loaded: 512, total: 1024 }] });
    renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    await waitFor(() => expect(screen.getByLabelText("File")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("notes.txt", "text/plain")] },
    });

    // The mid-flight frame. This is the assertion that fails if the progress
    // handler is removed -- the settled 100% below would still pass.
    expect(await screen.findByText("50%")).toBeTruthy();
    expect(screen.getByLabelText("Upload progress").getAttribute("value")).toBe("0.5");

    finish();
    await waitFor(() =>
      expect(screen.getByLabelText("Upload progress").getAttribute("value")).toBe("1"),
    );
  });

  it("surfaces a 413 inline, naming the cap rather than the status", async () => {
    installFakeXhr({ status: 413, body: "request body too large", contentType: "text/plain" });
    renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    await waitFor(() => expect(screen.getByLabelText("File")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("huge.bin", "application/octet-stream")] },
    });

    expect(await screen.findByText(/over this cluster's upload limit/)).toBeTruthy();
    expect(screen.getByText(/MEMQL_LIBRARY_MAX_UPLOAD_BYTES/)).toBeTruthy();
  });

  it("surfaces a network failure inline", async () => {
    installFakeXhr({ networkError: true });
    renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    await waitFor(() => expect(screen.getByLabelText("File")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("notes.txt", "text/plain")] },
    });

    expect(await screen.findByText(/did not reach the cluster/)).toBeTruthy();
  });

  // The silent-success shape. An origin that does not route the Library's
  // byte path answers the POST with its own SPA fallback: a 200, carrying
  // HTML. Parsing that as "well, it was 2xx" is how an upload that stored
  // nothing reports success.
  it("refuses a 2xx that is not JSON, and says the path is not routed here", async () => {
    installFakeXhr({ status: 200, body: "<!doctype html><title>MemQL Portal</title>", contentType: "text/html" });
    renderAt("/artifacts", ARTIFACT_ROWS, { signedIn: true });
    await waitFor(() => expect(screen.getByLabelText("File")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("notes.txt", "text/plain")] },
    });

    expect(await screen.findByText(/answered the upload itself/)).toBeTruthy();
    expect(screen.getByText(/not routed to the API here/)).toBeTruthy();
  });

  it("refuses to upload with no credential rather than posting an unowned file", async () => {
    const { sent } = installFakeXhr();
    // Not signedIn: the auth-disabled fixture supplies no bearer, which is
    // exactly what a cluster running with MEMQL_IDENTITY_ENABLED=false gives
    // this page -- and ownerUserId is stamped from actor.userId, so there
    // would be nowhere to put the bytes.
    renderAt("/artifacts", ARTIFACT_ROWS);
    await waitFor(() => expect(screen.getByLabelText("File")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("File"), {
      target: { files: [pick("notes.txt", "text/plain")] },
    });

    expect(await screen.findByText(/No credential to upload with/)).toBeTruthy();
    expect(sent).toHaveLength(0);
  });
});

// ===========================================================================
// DOWNLOAD / EXPORT
// ===========================================================================

describe("export", () => {
  it("puts a content link on every row, files and non-files alike", async () => {
    renderAt("/artifacts");
    await waitFor(() => expect(screen.getByTitle("Export Q3 budget")).toBeTruthy());

    // A generated output, a document and a file -- one route exports all of
    // them (design D9).
    expect(screen.getByTitle("Export Ten most beautiful birds").getAttribute("href")).toBe(
      `${ARTIFACTS_API_ROOT}/artifact-aaa/content`,
    );
    expect(screen.getByTitle("Export Q3 budget").getAttribute("href")).toBe(
      `${ARTIFACTS_API_ROOT}/artifact-bbb/content`,
    );
    expect(screen.getByTitle("Export Team handbook").getAttribute("href")).toBe(
      `${ARTIFACTS_API_ROOT}/artifact-ccc/content`,
    );
  });

  it("puts one on the detail page too, for an artifact with no file behind it", async () => {
    renderAt("/artifacts/artifact-aaa");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Ten most beautiful birds" })).toBeTruthy(),
    );
    const link = screen.getByRole("link", { name: /Export/ });
    expect(link.getAttribute("href")).toBe(`${ARTIFACTS_API_ROOT}/artifact-aaa/content`);
  });
});

// ===========================================================================
// SEARCH BY MEANING
// ===========================================================================

describe("search by meaning", () => {
  it("asks the builtin and ranks what comes back, score and all", async () => {
    const { calls } = renderAt("/artifacts", ARTIFACT_ROWS, {
      hits: [hitNode(BUDGET, 0.91, "…headcount for Q3…"), hitNode(BIRDS, 0.42, "…a heron…")],
    });
    await waitFor(() => expect(screen.getByPlaceholderText("the quarterly hiring plan")).toBeTruthy());

    fireEvent.change(screen.getByPlaceholderText("the quarterly hiring plan"), { target: { value: "hiring" } });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));

    await waitFor(() => expect(callsNamed(calls, "librarySimilarArtifacts").length).toBe(1));
    expect(callsNamed(calls, "librarySimilarArtifacts")[0]).toBe(
      'builtin librarySimilarArtifacts(text: "hiring", limit: 20)',
    );
    // The score is the reason this is a ranked list rather than the ordinary
    // one, so it is rendered rather than only sorted on.
    expect(await screen.findByText("0.910")).toBeTruthy();
    expect(screen.getByText("0.420")).toBeTruthy();
    expect(screen.getByText(/headcount for Q3/)).toBeTruthy();
  });

  it("composes with the label filter rather than replacing it", async () => {
    renderAt("/artifacts?q=hiring&label=finance", ARTIFACT_ROWS, {
      hits: [hitNode(BUDGET, 0.91, "…headcount…"), hitNode(BIRDS, 0.42, "…a heron…")],
    });

    // BUDGET carries `finance`; BIRDS does not, so the hit is filtered out
    // even though the builtin returned it.
    await waitFor(() => expect(screen.getByText("0.910")).toBeTruthy());
    expect(screen.queryByText("0.420")).toBeNull();
  });

  it("returns to the list when the query is cleared", async () => {
    renderAt("/artifacts?q=hiring", ARTIFACT_ROWS, { hits: [hitNode(BUDGET, 0.91, "…x…")] });
    await waitFor(() => expect(screen.getByText("0.910")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Clear" }));

    await waitFor(() => expect(screen.queryByText("0.910")).toBeNull());
    expect(screen.getAllByText("Ten most beautiful birds").length).toBeGreaterThan(0);
  });
});

// ===========================================================================
// TRAIN INTO A KNOWLEDGE DOMAIN
// ===========================================================================

describe("training a file into a domain", () => {
  it("calls the builtin with the file id recovered from the source ref", async () => {
    const { calls } = renderAt("/artifacts/artifact-ccc");
    await waitFor(() => expect(screen.getByLabelText("Knowledge domain")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("Knowledge domain"), {
      target: { value: "onboarding" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Train/ }));

    await waitFor(() => expect(callsNamed(calls, "libraryTrainFile").length).toBe(1));
    // fileId, not artifactId: the index row carries no fileId field at all,
    // so it comes out of sourceConceptRef's last segment.
    expect(callsNamed(calls, "libraryTrainFile")[0]).toBe(
      'builtin libraryTrainFile(fileId: "file-1", domainId: "onboarding")',
    );
    // Re-read rather than assumed -- the builtin refuses on two conditions
    // this browser cannot evaluate.
    await waitFor(() => expect(screen.getByText("onboarding")).toBeTruthy());
  });

  it("shows the domains the file is already trained into, and suggests the caller's others", async () => {
    renderAt("/artifacts/artifact-ccc");
    await waitFor(() => expect(screen.getByText("hr-policies")).toBeTruthy());
    // The suggestion list is the domains this caller has used before -- the
    // only list the engine can produce, since the knowledge-domain concept is
    // product-owned. A datalist, so a first training into a NEW domain is
    // still possible.
    const options = document.querySelectorAll("#artifact-train-domains option");
    expect([...options].map((o) => o.getAttribute("value"))).toContain("hr-policies");
  });

  it("offers no training control for an artifact with no file behind it", async () => {
    renderAt("/artifacts/artifact-aaa");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Ten most beautiful birds" })).toBeTruthy(),
    );
    expect(screen.queryByLabelText("Knowledge domain")).toBeNull();
  });
});

// ===========================================================================
// ARCHIVE
// ===========================================================================

describe("archiving", () => {
  it("hides archived rows by default and brings them back behind the toggle", async () => {
    const { calls } = renderAt("/artifacts");
    await waitFor(() => expect(screen.getAllByText("Q3 budget").length).toBeGreaterThan(0));
    // OLD_DECK is archived, and libraryArtifacts excludes it server-side.
    expect(screen.queryByText("Last year's deck")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Show archived" }));

    // The default list cannot answer this -- its filter carries `archived !=
    // true` -- so the page reaches the per-lens reads, whose union is the
    // whole owned set.
    await waitFor(() => expect(screen.getAllByText("Last year's deck").length).toBeGreaterThan(0));
    expect(callsNamed(calls, "libraryArtifactsByLens").length).toBe(2);
  });

  // Found in review: the facet bar used to render only when a label existed,
  // which put the archived toggle inside that condition. A Library whose every
  // row is archived has no labels to show -- so the one control that would get
  // those rows back was hidden exactly when it was needed.
  it("keeps the toggle reachable when every row is archived and there are no labels", async () => {
    const onlyArchived = node("artifact-eee", {
      ownerUserId: "user-1",
      lens: "artifact",
      kind: "note",
      source: "user_created",
      sourceConceptRef: "v1:notes:note:n-1",
      title: "Retired note",
      archived: true,
      updatedAt: "2026-07-01T10:00:00Z",
    });
    renderAt("/artifacts", [onlyArchived]);

    await waitFor(() => expect(screen.getByText(/No artifacts yet/)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Show archived" }));
    await waitFor(() => expect(screen.getAllByText("Retired note").length).toBeGreaterThan(0));
  });

  it("archives from the list only through the confirmation, and the row leaves", async () => {
    const { calls } = renderAt("/artifacts");
    await waitFor(() => expect(screen.getByTitle("Archive Q3 budget")).toBeTruthy());

    fireEvent.click(screen.getByTitle("Archive Q3 budget"));

    // The dialog states the consequence; nothing is written until Archive.
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/"Q3 budget" drops out of this list/)).toBeTruthy();
    expect(callsNamed(calls, "archiveArtifact").length).toBe(0);

    fireEvent.click(within(dialog).getByRole("button", { name: "Archive" }));

    await waitFor(() => expect(callsNamed(calls, "archiveArtifact").length).toBe(1));
    expect(callsNamed(calls, "archiveArtifact")[0]).toBe(
      'mutation archiveArtifact(artifactId: "artifact-bbb")',
    );
    await waitFor(() => expect(screen.queryByText("Q3 budget")).toBeNull());
  });

  it("cancels without writing", async () => {
    const { calls } = renderAt("/artifacts");
    await waitFor(() => expect(screen.getByTitle("Archive Q3 budget")).toBeTruthy());

    fireEvent.click(screen.getByTitle("Archive Q3 budget"));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(callsNamed(calls, "archiveArtifact").length).toBe(0);
    expect(screen.getAllByText("Q3 budget").length).toBeGreaterThan(0);
  });

  it("archives from the detail page behind the same confirmation", async () => {
    const { calls } = renderAt("/artifacts/artifact-aaa");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Ten most beautiful birds" })).toBeTruthy(),
    );

    fireEvent.click(screen.getByRole("button", { name: "Archive" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Archive" }));

    await waitFor(() => expect(callsNamed(calls, "archiveArtifact").length).toBe(1));
    // The page says so rather than navigating away: the row is still there
    // and still readable, which is what a SOFT delete means.
    expect(await screen.findByText("Archived")).toBeTruthy();
  });
});

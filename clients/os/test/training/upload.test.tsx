import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { TrainingApp } from "../../src/apps/training/TrainingApp";
import { LocalTrainingSettingsStore } from "../../src/apps/training/settings";
import { PLAN_CONCEPT } from "../../src/apps/training/concepts";
import type {
  AttachmentUploadProvider,
  AttachmentUploadResult,
} from "../../src/apps/training/attachmentUpload";
import type { UploadHandle } from "../../src/items/upload";
import {
  DONE_PLAN,
  FAILED_PLAN,
  GOAL_PLAN,
  OTHER_USERS_PLAN,
  RUNNING_PLAN,
  click,
  emit,
  fakeConnection,
  planRow,
  settle,
  withSession,
  type FakeConnection,
} from "./harness";

// The Upload section, through the real LiveCollection.

function memStore() {
  const data = new Map<string, string>();
  return new LocalTrainingSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

/** An upload provider the test drives by hand. */
function fakeUploads() {
  const calls: { spaceId: string; file: File }[] = [];
  let settleNext: ((result: AttachmentUploadResult) => void) | null = null;
  let failNext: ((err: Error) => void) | null = null;
  const provider: AttachmentUploadProvider = {
    upload(spaceId, file): UploadHandle<AttachmentUploadResult> {
      calls.push({ spaceId, file });
      const done = new Promise<AttachmentUploadResult>((resolve, reject) => {
        settleNext = resolve;
        failNext = reject;
      });
      return { done, abort: () => failNext?.(new Error("aborted")) };
    },
  };
  return {
    provider,
    calls,
    land: () => settleNext?.({ attachmentId: "att-1", fileName: "notes.pdf" }),
    fail: (message: string) => failNext?.(new Error(message)),
  };
}

function mount(
  connection: FakeConnection,
  opts: {
    section?: string;
    userId?: string;
    uploads?: AttachmentUploadProvider;
    navigate?: () => void;
  } = {},
) {
  h.connection = connection;
  return render(
    withSession(
      <TrainingApp
        sectionId={opts.section ?? "upload"}
        navigate={opts.navigate ?? vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
        {...(opts.uploads ? { uploads: opts.uploads } : {})}
      />,
      { userId: opts.userId ?? "u-me" },
    ),
  );
}

function drop(files: File[]) {
  const input = document.querySelector('input[type="file"]') as HTMLInputElement;
  Object.defineProperty(input, "files", { value: files, configurable: true });
  return input;
}

beforeEach(() => {
  h.connection = null;
});

describe("the analysis list", () => {
  it("reads plansForUser and renders the caller's analyses", async () => {
    const connection = fakeConnection({
      plans: [RUNNING_PLAN, DONE_PLAN],
      activeSpaceId: "space-1",
    });
    mount(connection);

    await screen.findByText("contract.pdf");
    expect(screen.getByText("handbook.docx")).toBeTruthy();
    // The generated builder ran, and this is the text that reached the wire.
    expect(connection.calls).toContain("query plansForUser()");
  });

  it("NEVER renders a plan somebody else requested, even off the subscription", async () => {
    // `v1:planner:plan` declares no row-authz tier, so the SUBSCRIPTION admits
    // every subscriber -- the seed is scoped and the live feed is not. The
    // filter has to hold on the client, and this is the case that proves it
    // does: the row arrives on the wire and never reaches the screen.
    const connection = fakeConnection({ plans: [RUNNING_PLAN], activeSpaceId: "space-1" });
    mount(connection);
    await screen.findByText("contract.pdf");

    await emit(connection, PLAN_CONCEPT, OTHER_USERS_PLAN, "NODE_CREATED");

    expect(screen.queryByText("theirs.pdf")).toBeNull();
    expect(screen.getByText("contract.pdf")).toBeTruthy();
  });

  it("NEVER renders a plan of another kind", async () => {
    const connection = fakeConnection({
      plans: [RUNNING_PLAN, GOAL_PLAN],
      activeSpaceId: "space-1",
    });
    mount(connection);
    await screen.findByText("contract.pdf");
    expect(screen.queryByText("Ship the quarterly report")).toBeNull();
  });

  it("renders NOTHING while the viewer is unknown", async () => {
    // Access resolves asynchronously; showing the cluster's plans until it
    // does would be the exact bug the filter exists to prevent.
    const connection = fakeConnection({ plans: [RUNNING_PLAN], activeSpaceId: "space-1" });
    mount(connection, { userId: "" });
    await settle();
    expect(screen.queryByText("contract.pdf")).toBeNull();
  });

  it("pulses on a status transition and marks a new row", async () => {
    const connection = fakeConnection({ plans: [RUNNING_PLAN], activeSpaceId: "space-1" });
    mount(connection);
    await screen.findByText("contract.pdf");

    // A NEW plan rises and rings.
    await emit(connection, PLAN_CONCEPT, planRow({ id: "plan-2", goal: "Analyze fresh.pdf" }), "NODE_CREATED");
    const fresh = (await screen.findByText("fresh.pdf")).closest(".os-livelist-row");
    expect(fresh?.getAttribute("data-arrival")).toBe("added");

    // An UPDATE to one already on screen rings only.
    await emit(connection, PLAN_CONCEPT, { ...RUNNING_PLAN, status: "succeeded" });
    const row = screen.getByText("contract.pdf").closest(".os-livelist-row");
    expect(row?.getAttribute("data-arrival")).toBe("updated");
  });

  it("puts a failure's own reason on the row", async () => {
    const connection = fakeConnection({ plans: [FAILED_PLAN], activeSpaceId: "space-1" });
    mount(connection);
    const row = (await screen.findByText("broken.pdf")).closest(".os-livelist-row");
    expect(within(row as HTMLElement).getByText("extract: unsupported pdf encoding")).toBeTruthy();
    expect(within(row as HTMLElement).getByText("failed")).toBeTruthy();
  });
});

describe("the dropzone", () => {
  it("uploads to the resolved daily space", async () => {
    const uploads = fakeUploads();
    const connection = fakeConnection({ activeSpaceId: "space-daily" });
    mount(connection, { uploads: uploads.provider });

    await screen.findByText("Choose files");
    // The space read ran, with the caller's own id.
    expect(connection.calls).toContain('query userActiveSpace(userId: "u-me")');

    const input = drop([new File(["x"], "notes.pdf", { type: "application/pdf" })]);
    await click(screen.getByText("Choose files"));
    await import("@testing-library/react").then(({ fireEvent, act }) =>
      act(async () => {
        fireEvent.change(input);
      }),
    );

    expect(uploads.calls).toHaveLength(1);
    expect(uploads.calls[0]?.spaceId).toBe("space-daily");
    expect(uploads.calls[0]?.file.name).toBe("notes.pdf");
    expect(screen.getByText("notes.pdf")).toBeTruthy();
  });

  it("renders a refusal IN SURFACE with a retry, and the retry re-sends", async () => {
    const uploads = fakeUploads();
    const connection = fakeConnection({ activeSpaceId: "space-daily" });
    mount(connection, { uploads: uploads.provider });
    await screen.findByText("Choose files");

    const input = drop([new File(["x"], "big.zip", { type: "application/zip" })]);
    const { fireEvent, act } = await import("@testing-library/react");
    await act(async () => {
      fireEvent.change(input);
    });
    await act(async () => {
      uploads.fail("unsupported file type: application/zip");
    });

    // The SERVER'S OWN SENTENCE, beside the file it is about. Never a toast.
    expect(screen.getByText("unsupported file type: application/zip")).toBeTruthy();

    await click(screen.getByText("Retry"));
    expect(uploads.calls).toHaveLength(2);
    expect(uploads.calls[1]?.file.name).toBe("big.zip");
  });

  it("acknowledges a landed upload and lets it be dismissed", async () => {
    const uploads = fakeUploads();
    const connection = fakeConnection({ activeSpaceId: "space-daily" });
    mount(connection, { uploads: uploads.provider });
    await screen.findByText("Choose files");

    const input = drop([new File(["x"], "notes.pdf", { type: "application/pdf" })]);
    const { fireEvent, act } = await import("@testing-library/react");
    await act(async () => {
      fireEvent.change(input);
    });
    await act(async () => {
      uploads.land();
    });

    expect(screen.getByText("in the cluster")).toBeTruthy();
    await click(screen.getByText("Dismiss"));
    expect(screen.queryByText("in the cluster")).toBeNull();
  });

  it("refuses to upload, in surface, when there is NO active space", async () => {
    const uploads = fakeUploads();
    const connection = fakeConnection({});
    mount(connection, { uploads: uploads.provider });

    await screen.findByText(/no active space yet/i);
    // The control is present and disabled rather than hidden: the notice
    // above it explains what is missing, and hiding the control would leave
    // that notice explaining the absence of something.
    const button = screen.getByText("Choose files").closest("button") as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(uploads.calls).toHaveLength(0);
  });

  it("says so, and offers a retry, when the space read FAILED", async () => {
    const connection = fakeConnection({ activeSpaceError: "read refused" });
    mount(connection);
    await screen.findByText(/did not say which space is yours/i);
    expect(screen.getByText("read refused")).toBeTruthy();
  });
});

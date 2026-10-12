import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

const { useCompositions, useFolders, useLibraryFiles, useRecipes, useTemplates } = await import("../../src/apps/materializer/useCompose");
const { compositionRow, fakeConnection, folderRow, libraryFileRow, PAGE_SIZE, recipeRow, templateRow } = await import("./harness");

// The Materializer's feeds.
//
// THE HEAD'S COUNT STOPPED AT 50. The three lists seeded one page of the engine's `paginate 50`
// (`paged: false`, a cursor that was never read), so a person with 53 files saw 50 and was told
// that was all of them. Each feed now walks the cursor the engine hands back until it hands none.
//
// THE LIBRARY'S FILES AND FOLDERS ARE NOT SEEDED UNTIL SOMEONE ASKS. The Make a file wizard and a
// made file's page need them; the list alone must not read the person's whole Library to draw its
// rows. A feed that is not enabled passes `useLiveCollection` a null key, which makes no read and
// opens no subscription.

const many = (count: number, make: (index: number) => Row): Row[] => Array.from({ length: count }, (_, index) => make(index));

describe("a feed that walks every page", () => {
  it("lists every page of compositions", async () => {
    // Fifty rows with a cursor, then three with none.
    const connection = fakeConnection({ compositions: many(53, (i) => compositionRow({ id: `c${i}`, status: "ready" })) });
    h.connection = connection;
    const { result } = renderHook(() => useCompositions());

    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(53));
    expect(connection.query.compositions).toHaveBeenCalledTimes(2);
    const [first, second] = connection.query.compositions.mock.calls;
    expect(first?.[1]).not.toHaveProperty("cursor");
    expect(second?.[1]).toMatchObject({ cursor: "p2" });
    // Nothing is dropped between pages, and nothing is read twice.
    expect(new Set(result.current.snapshot.rows.map((row) => row["id"])).size).toBe(53);
  });

  it("follows the cursor for as long as the engine names a next page", async () => {
    const connection = fakeConnection({ compositions: many(PAGE_SIZE * 2 + 7, (i) => compositionRow({ id: `c${i}`, status: "ready" })) });
    h.connection = connection;
    const { result } = renderHook(() => useCompositions());

    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(PAGE_SIZE * 2 + 7));
    expect(connection.query.compositions.mock.calls.map((call) => (call[1] as { cursor?: string } | undefined)?.cursor)).toEqual([undefined, "p2", "p3"]);
  });

  it("lists every page of templates", async () => {
    const connection = fakeConnection({ templates: many(53, (i) => templateRow({ id: `t${i}` })) });
    h.connection = connection;
    const { result } = renderHook(() => useTemplates());

    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(53));
    expect(connection.query.composeTemplates).toHaveBeenCalledTimes(2);
    expect(connection.query.composeTemplates.mock.calls[0]?.[1]).not.toHaveProperty("cursor");
    expect(connection.query.composeTemplates.mock.calls[1]?.[1]).toMatchObject({ cursor: "p2" });
  });

  it("lists every page of recipes", async () => {
    const connection = fakeConnection({ recipes: many(53, (i) => recipeRow({ id: `r${i}` })) });
    h.connection = connection;
    const { result } = renderHook(() => useRecipes());

    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(53));
    expect(connection.query.composeRecipes).toHaveBeenCalledTimes(2);
    expect(connection.query.composeRecipes.mock.calls[0]?.[1]).not.toHaveProperty("cursor");
    expect(connection.query.composeRecipes.mock.calls[1]?.[1]).toMatchObject({ cursor: "p2" });
  });

  it("reads a list that fits one page once", async () => {
    const connection = fakeConnection({ compositions: many(3, (i) => compositionRow({ id: `c${i}`, status: "ready" })) });
    h.connection = connection;
    const { result } = renderHook(() => useCompositions());

    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(3));
    expect(connection.query.compositions).toHaveBeenCalledTimes(1);
  });
});

describe("the Library's files and folders, read only when asked for", () => {
  // The wait is for the feed to have had every chance to read: a few turns of the event loop in which
  // a collection that was going to seed would already have called.
  const settle = () => act(async () => { await new Promise((resolve) => setTimeout(resolve, 20)); });

  it("reads no Library files or folders until asked", async () => {
    const connection = fakeConnection({
      libraryFiles: many(60, (i) => libraryFileRow({ id: `f${i}`, name: `notes-${i}.md` })),
      folders: many(3, (i) => folderRow({ id: `d${i}` })),
    });
    h.connection = connection;
    const files = renderHook(({ enabled }) => useLibraryFiles(enabled), { initialProps: { enabled: false } });
    const folders = renderHook(({ enabled }) => useFolders(enabled), { initialProps: { enabled: false } });
    await settle();

    expect(connection.query.libraryFilesForOwner).not.toHaveBeenCalled();
    expect(connection.query.libraryFolders).not.toHaveBeenCalled();
    expect(files.result.current.source).toBeNull();
    expect(files.result.current.snapshot.rows).toEqual([]);
    expect(folders.result.current.source).toBeNull();
    expect(folders.result.current.snapshot.rows).toEqual([]);

    // Asked for, the files walk both pages and the folders their one.
    files.rerender({ enabled: true });
    folders.rerender({ enabled: true });
    await waitFor(() => expect(files.result.current.snapshot.rows).toHaveLength(60));
    await waitFor(() => expect(folders.result.current.snapshot.rows).toHaveLength(3));
    expect(connection.query.libraryFilesForOwner).toHaveBeenCalledTimes(2);
    expect(connection.query.libraryFilesForOwner.mock.calls[0]?.[1]).not.toHaveProperty("cursor");
    expect(connection.query.libraryFilesForOwner.mock.calls[1]?.[1]).toMatchObject({ cursor: "p2" });
    expect(connection.query.libraryFolders).toHaveBeenCalledTimes(1);
  });

  it("reads the files a wizard needs the moment it opens, with the rows' own facts", async () => {
    const connection = fakeConnection({
      libraryFiles: [libraryFileRow({ id: "f1", name: "notes.md", size: 2048 }), libraryFileRow({ id: "f2", name: "old.md", size: 10, archived: true })],
    });
    h.connection = connection;
    const { result } = renderHook(() => useLibraryFiles(true));

    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(2));
    // The feed holds the rows as the cluster gave them: name, size and archived are what the routing reads, and the id is the file's own.
    expect(result.current.snapshot.rows).toEqual([
      expect.objectContaining({ id: "f1", name: "notes.md", size: 2048, archived: false }),
      expect.objectContaining({ id: "f2", name: "old.md", size: 10, archived: true }),
    ]);
  });

  it("reads the folders again as pages when the engine pages them", async () => {
    const connection = fakeConnection({ folders: many(PAGE_SIZE + 4, (i) => folderRow({ id: `d${i}` })) });
    h.connection = connection;
    const { result } = renderHook(() => useFolders(true));

    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(PAGE_SIZE + 4));
    expect(connection.query.libraryFolders).toHaveBeenCalledTimes(2);
    expect(connection.query.libraryFolders.mock.calls[1]?.[1]).toMatchObject({ cursor: "p2" });
  });

  it("lets go of the rows when the wizard closes", async () => {
    const connection = fakeConnection({ libraryFiles: [libraryFileRow({ id: "f1" })] });
    h.connection = connection;
    const { result, rerender } = renderHook(({ enabled }) => useLibraryFiles(enabled), { initialProps: { enabled: true } });
    await waitFor(() => expect(result.current.snapshot.rows).toHaveLength(1));

    rerender({ enabled: false });
    expect(result.current.source).toBeNull();
    expect(result.current.snapshot.rows).toEqual([]);
  });
});

describe("the runs behind unfinished compositions", () => {
  const run = (over: Row) => ({ ownerUserId: "v1:identity:user:me", goalId: "v1:work:goal:g1", status: "running", startedAt: "2026-09-05T12:00:05Z", ...over }) as Row;

  it("knows the run of an unfinished composition", async () => {
    const connection = fakeConnection({
      compositions: [compositionRow({ id: "c1", status: "composing", runId: "r1", outputFileId: "" })],
      runs: [run({ id: "r1" })],
    });
    h.connection = connection;
    const { result } = renderHook(() => useCompositions());

    await waitFor(() => expect(result.current.runOf("r1")).toBeDefined());
    expect(result.current.runOf("r1")).toMatchObject({ id: "r1", status: "running", startedAt: "2026-09-05T12:00:05Z" });
  });

  it("does not read the run of a finished composition, or know one it was not asked about", async () => {
    const connection = fakeConnection({
      compositions: [
        compositionRow({ id: "c1", status: "composing", runId: "r1", outputFileId: "" }),
        compositionRow({ id: "c2", status: "ready", runId: "r2" }),
      ],
      runs: [run({ id: "r1" }), run({ id: "r2", status: "succeeded" })],
    });
    h.connection = connection;
    const { result } = renderHook(() => useCompositions());

    await waitFor(() => expect(result.current.runOf("r1")).toBeDefined());
    expect(result.current.runOf("r2")).toBeUndefined();
    expect(result.current.runOf("nobody")).toBeUndefined();
    expect(result.current.runOf("")).toBeUndefined();
    expect(connection.query.workRunForOwner).toHaveBeenCalledTimes(1);
  });

  it("finds the run whether the id is given bare or canonical", async () => {
    const connection = fakeConnection({
      compositions: [compositionRow({ id: "c1", status: "composing", runId: "r1", outputFileId: "" })],
      runs: [run({ id: "r1" })],
    });
    h.connection = connection;
    const { result } = renderHook(() => useCompositions());

    await waitFor(() => expect(result.current.runOf("r1")).toBeDefined());
    expect(result.current.runOf("v1:work:run:r1")).toBe(result.current.runOf("r1"));
  });

  it("still lists every composition, joined with its run's fate", async () => {
    const connection = fakeConnection({
      compositions: [compositionRow({ id: "c1", status: "composing", runId: "r1", goalId: "v1:work:goal:g1", outputFileId: "" })],
      runs: [run({ id: "r1", status: "abandoned" })],
    });
    h.connection = connection;
    const { result } = renderHook(() => useCompositions());

    // A run the cluster lost makes the composition read as failed, as it did before runOf was exposed.
    await waitFor(() => expect(result.current.snapshot.rows[0]).toMatchObject({ id: "c1", status: "failed" }));
    expect(result.current.runOf("r1")).toMatchObject({ status: "abandoned" });
  });
});

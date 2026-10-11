import { useCallback, useRef, useState } from "react";
import {
  isDescribedMarkdown,
  rowString,
  sameEntityId,
  startMaterialize,
  type MaterializeExecute,
  type MaterializeRequest,
  type MaterializeSource,
} from "@znasllc-io/memql-sdk-core/client";

import type { ComposeSource } from "../../items/materialize";
import type { CompositionRow, SourceRow, SelectorRow } from "./rows";

import { useOsConnection } from "../../live/connection";
import { FORMATS } from "./words";

// Every write the Materializer makes, and the busy/error pair each one
// owns.
//
// ===========================================================================
// NOTHING HERE CHECKS A ROLE, AND NOTHING HERE IS THE AUTHORIZATION
// ===========================================================================
// Every `v1:compose:*` concept declares the composite tier
// (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so a person acts on
// their own compositions and the engine decides which those are. The
// builtins repeat their own gate in Go -- a builtin's annotation set
// carries no `@requiresRank`, so the floor is the handler's, and these
// are caller-scoped rather than ranked. None of that is decided in a
// browser.
//
// ===========================================================================
// A REFUSAL IS THE SERVER'S OWN SENTENCE AND IT RENDERS BESIDE THE CONTROL
// ===========================================================================
// Never a toast; this shell has none. Each write owns its own error slot
// rather than sharing one, because the composer's bar, the list's acts
// and the template form are three different places on screen -- and a
// shared slot puts a refusal under a control somebody is looking at,
// naming a failure they did not cause.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface WriteState {
  busy: boolean;
  /** The server's own sentence, verbatim. "" when the last attempt worked. */
  error: string;
  reset: () => void;
}

const NOT_CONNECTED = "Not connected to the cluster, so nothing was written.";

// ---------------------------------------------------------------------------
// materialize
// ---------------------------------------------------------------------------

export interface MaterializeFacts {
  name: string;
  statement: string;
  format: string;
  sources: ComposeSource[];
  templateId: string;
  /**
   * A folder somebody ELSE named (another app's starting setup), or "" for the
   * Library root. It is a claim: the call checks the person can read it before
   * anything is filed there, and drops it when they cannot.
   */
  folderId: string;
  accountIds: string[];
  deployableKind: string;
  /** "email_template" for an email template, or "" for an ordinary file. */
  outputKind: string;
}

/**
 * A Markdown file asked for by describing it, whose description the Library
 * has not been given yet.
 *
 * The cluster files such a file EMPTY and keeps the statement on its
 * composition, because only the person's own client may ask the Library to
 * write it (a revision request made from inside a work run is refused). This is
 * what the hand-off needs once the file exists, remembered from the request
 * that made it -- including the request id, because the hand-off derives its
 * own ids from it and a repeat must find the first one's receipts.
 */
export interface DescribedFile {
  compositionId: string;
  statement: string;
  sources: MaterializeSource[];
  requestId: string;
}

export interface MaterializeState extends WriteState {
  /** What the last successful call produced, so the surface can say which. */
  compositionId: string;
  /** Described Markdown files this window has started, for descriptionHandOff.ts. */
  described: DescribedFile[];
  materialize: (facts: MaterializeFacts) => Promise<string>;
}

type Query = NonNullable<ReturnType<typeof useOsConnection>>["query"];

/**
 * `execute` for the SDK's helpers: one named construct, run as the person who
 * is signed in, answered with its rows. The helpers never import a transport;
 * this is the one place the OS gives them one.
 */
export function executeOver(query: Pick<Query, "executeNamed">): MaterializeExecute {
  return (name, call) => query.executeNamed(name, call).then((result) => result.rows());
}

/**
 * The folder, when the person can read it; "" when they cannot.
 *
 * A folder id arrives from another app, and an id from another app can be
 * stale (the folder was archived since), foreign (it is not theirs) or wrong.
 * NOTHING DOWNSTREAM CHECKS IT: `composeMaterialize` hands the id to the file's
 * creation, which stores whatever string it is given, so a file filed under a
 * folder the person cannot see would simply be somewhere they cannot find it.
 * Filing at the top, where they look first, is the better miss -- so the claim
 * is checked here, as the person, and dropped when it fails. An archived folder
 * is somewhere a file was put away, not somewhere to put one; and a read that
 * could not be made proves nothing either way, so it is dropped too.
 */
async function readableFolder(query: Query, folderId: string): Promise<string> {
  try {
    const row = (await query.libraryFolderById({ folderId })).rows()[0];
    if (row === undefined || !sameEntityId(rowString(row, "id"), folderId) || row["archived"] === true) return "";
    return folderId;
  } catch {
    return "";
  }
}

/**
 * Create a file: the Materializer opens a composition, a goal and the goal's
 * first run in ONE call, and this returns the composition's id. FOLLOWING it is
 * not this hook's job and is not done by polling: the app reads its live
 * composition feed, which says when the file is ready.
 *
 * ONE call is the whole act -- the builtin opens the composition, the goal and
 * the goal's first run together, so there is no client-side follow-up write to
 * get half-done. That is the property the Deployables app records about
 * `packageDeploy` and it is the reason this is a builtin rather than three
 * mutations.
 *
 * ===========================================================================
 * A REQUEST ID BELONGS TO ONE ASK
 * ===========================================================================
 * The id is how a repeat of a request finds the composition the first one made.
 * It is kept across attempts at the SAME ask -- a retry after a refusal, or
 * after a reply that never arrived, must not make a second file -- and renewed
 * when:
 *   - the cluster answers with a composition (the next press is another file),
 *   - the facts change (that is a different ask, and reusing the id would hand
 *     back the file the old facts made), or
 *   - the form is reset.
 * A FAILED composition has already answered, so "try again" after one carries a
 * new id by the first rule: a repeat with the old id would return the failure.
 *
 * The cluster looks the id up and then writes; it does not claim it, so two
 * starts in flight at once can still make two files. Hence ONE start at a time:
 * a press made while another is on its way is that start's answer, not another
 * request.
 */
export function useMaterialize(): MaterializeState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [compositionId, setCompositionId] = useState("");
  const [described, setDescribed] = useState<DescribedFile[]>([]);
  const ask = useRef<{ key: string; id: string } | null>(null);
  const inFlight = useRef<Promise<string> | null>(null);

  const start = useCallback(
    async (facts: MaterializeFacts): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return "";
      }
      const name = facts.name.trim();
      if (name === "") {
        // The one rule a browser can answer, answered here rather than
        // sent. `name` is `string!`, so the server would refuse it too --
        // but a round trip to be told what this form already knows is a
        // round trip somebody waits for.
        setError("Give it a name first — it becomes the file's name too.");
        return "";
      }
      if (facts.accountIds.length !== 1 || !facts.accountIds[0]?.trim()) {
        setError("Choose an organization for this file.");
        return "";
      }
      const format = FORMATS.find((candidate) => candidate === facts.format);
      if (format === undefined) {
        setError("Choose what kind of file to make.");
        return "";
      }
      setBusy(true);
      setError("");
      setCompositionId("");
      try {
        const statement = facts.statement.trim();
        const folderId = facts.folderId.trim() === "" ? "" : await readableFolder(query, facts.folderId.trim());
        // AN UNANSWERED OPTIONAL IS OMITTED, never sent empty: an optional
        // field given "" is a value the engine writes, and "the caller said
        // nothing" and "the caller said empty" are different statements.
        const request: Omit<MaterializeRequest, "requestId"> = {
          name,
          format,
          ...(statement ? { statement } : {}),
          ...(facts.sources.length > 0 ? { sources: facts.sources } : {}),
          ...(facts.templateId ? { templateId: facts.templateId } : {}),
          ...(folderId ? { folderId } : {}),
          ...(facts.outputKind === "email_template" ? { outputKind: "email_template" as const } : {}),
          ...(facts.accountIds.length > 0 ? { accountIds: facts.accountIds } : {}),
          ...(facts.deployableKind ? { deployableKind: facts.deployableKind } : {}),
        };
        const key = JSON.stringify(request);
        if (ask.current?.key !== key) ask.current = { key, id: newId() };
        const requestId = ask.current.id;
        const id = await startMaterialize(executeOver(query), { ...request, requestId });
        ask.current = null;
        setCompositionId(id);
        if (isDescribedMarkdown(request)) {
          setDescribed((prev) => [...prev, { compositionId: id, statement: request.statement ?? "", sources: request.sources ?? [], requestId }]);
        }
        return id;
      } catch (err) {
        setError(describe(err));
        return "";
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  const materialize = useCallback(
    (facts: MaterializeFacts): Promise<string> => {
      if (inFlight.current !== null) return inFlight.current;
      const run = start(facts).finally(() => {
        inFlight.current = null;
      });
      inFlight.current = run;
      return run;
    },
    [start],
  );

  return {
    busy,
    error,
    compositionId,
    described,
    materialize,
    reset: () => {
      ask.current = null;
      setError("");
    },
  };
}

// ---------------------------------------------------------------------------
// The acts on an existing composition
// ---------------------------------------------------------------------------

export interface CompositionActsState extends WriteState {
  stop: (compositionId: string) => Promise<void>;
  archive: (compositionId: string) => Promise<void>;
  restore: (compositionId: string) => Promise<void>;
  saveRecipe: (facts: NewRecipeFacts) => Promise<string>;
}

export interface NewRecipeFacts {
  name: string;
  description: string;
  format: string;
  templateId: string;
  folderId: string;
  sourceSelectors: SelectorRow[];
  accountIds: string[];
  outputKind: string;
}

/** Preserve reference files and their explicit content/image choices when an
 * email composition is saved as a recipe. Queries resolve again on each run. */
export function recipeFacts(composition: CompositionRow, sources: SourceRow[]): NewRecipeFacts {
  return {
    name: composition.name, description: composition.statement, format: composition.format,
    templateId: composition.templateId, folderId: composition.folderId,
    accountIds: composition.accountIds, outputKind: composition.outputKind,
    sourceSelectors: sources.filter(source => source.kind === "query" || source.kind === "library_file").map(source => ({
      kind: source.kind === "query" ? "concept_query" : source.kind, selector: source.ref, label: source.label,
      ...(source.content !== undefined ? { content: source.content } : {}),
      ...(source.includeImages !== undefined ? { includeImages: source.includeImages } : {}),
    })),
  };
}

export function useCompositionActs(): CompositionActsState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const run = useCallback(
    async <T,>(fn: (query: NonNullable<typeof connection>["query"]) => Promise<T>, fallback: T): Promise<T> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return fallback;
      }
      setBusy(true);
      setError("");
      try {
        return await fn(query);
      } catch (err) {
        setError(describe(err));
        return fallback;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    reset: () => setError(""),
    stop: async (compositionId) => {
      await run(async (query) => {
        await query.composeCancel({ compositionId });
      }, undefined);
    },
    archive: async (compositionId) => {
      await run(async (query) => {
        await query.archiveComposition({ compositionId });
      }, undefined);
    },
    restore: async (compositionId) => {
      await run(async (query) => {
        await query.restoreComposition({ compositionId });
      }, undefined);
    },
    saveRecipe: async (facts) =>
      run(async (query) => {
        // THE ID IS THE CLIENT'S because `createComposeRecipe` takes one:
        // an insert{} stamps `id: args.recipeId`, so a retried call
        // re-versions the same row instead of appending a duplicate to
        // somebody's recipe list.
        const recipeId = newId();
        await query.createComposeRecipe({
          recipeId,
          name: facts.name.trim(),
          format: facts.format,
          accountIds: facts.accountIds,
          ...(facts.outputKind ? { outputKind: facts.outputKind } : {}),
          ...(facts.description.trim() ? { description: facts.description.trim() } : {}),
          ...(facts.templateId ? { templateId: facts.templateId } : {}),
          ...(facts.folderId ? { folderId: facts.folderId } : {}),
          ...(facts.sourceSelectors.length > 0 ? { sourceSelectors: facts.sourceSelectors.map(source => ({ ...source })) } : {}),
        });
        return recipeId;
      }, ""),
  };
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

export interface TemplateActsState extends WriteState {
  create: (facts: NewTemplateFacts) => Promise<string>;
  archive: (templateId: string) => Promise<void>;
  restore: (templateId: string) => Promise<void>;
}

export interface NewTemplateFacts {
  accountId: string;
  name: string;
  description: string;
  fileId: string;
  format: string;
}

export function useTemplateActs(): TemplateActsState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const create = useCallback(
    async (facts: NewTemplateFacts): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return "";
      }
      if (facts.accountId.trim() === "") {
        setError("Choose an organization for this template.");
        return "";
      }
      if (facts.name.trim() === "" || facts.fileId.trim() === "") {
        setError("A template needs a name and a file from your Library.");
        return "";
      }
      setBusy(true);
      setError("");
      try {
        const templateId = newId();
        await query.createComposeTemplate({
          templateId,
          name: facts.name.trim(),
          fileId: facts.fileId.trim(),
          accountIds: [facts.accountId],
          format: facts.format,
          ...(facts.description.trim() ? { description: facts.description.trim() } : {}),
        });
        return templateId;
      } catch (err) {
        setError(describe(err));
        return "";
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  const flip = useCallback(
    async (templateId: string, restore: boolean): Promise<void> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return;
      }
      setBusy(true);
      setError("");
      try {
        if (restore) await query.restoreComposeTemplate({ templateId });
        else await query.archiveComposeTemplate({ templateId });
      } catch (err) {
        setError(describe(err));
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    reset: () => setError(""),
    create,
    archive: (templateId) => flip(templateId, false),
    restore: (templateId) => flip(templateId, true),
  };
}

// ---------------------------------------------------------------------------
// Recipes
// ---------------------------------------------------------------------------

export interface RecipeActsState extends WriteState {
  runRecipe: (recipeId: string, name: string) => Promise<string>;
  archive: (recipeId: string) => Promise<void>;
  restore: (recipeId: string) => Promise<void>;
}

export function useRecipeActs(): RecipeActsState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const call = useCallback(
    async <T,>(fn: (query: NonNullable<typeof connection>["query"]) => Promise<T>, fallback: T): Promise<T> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError(NOT_CONNECTED);
        return fallback;
      }
      setBusy(true);
      setError("");
      try {
        return await fn(query);
      } catch (err) {
        setError(describe(err));
        return fallback;
      } finally {
        setBusy(false);
      }
    },
    [connection],
  );

  return {
    busy,
    error,
    reset: () => setError(""),
    runRecipe: (recipeId, name) =>
      call(async (query) => {
        const result = await query.composeRunRecipe({
          recipeId,
          ...(name.trim() ? { name: name.trim() } : {}),
        });
        const reply = result.rows()[0] ?? null;
        return typeof reply?.["compositionId"] === "string" ? (reply["compositionId"] as string) : "";
      }, ""),
    archive: async (recipeId) => {
      await call(async (query) => {
        await query.archiveComposeRecipe({ recipeId });
      }, undefined);
    },
    restore: async (recipeId) => {
      await call(async (query) => {
        await query.restoreComposeRecipe({ recipeId });
      }, undefined);
    },
  };
}

/**
 * A client-side id for the rows whose mutations take one.
 *
 * `crypto.randomUUID` where it exists, and a bounded fallback where it
 * does not -- the id is a row key rather than a secret, and a failed
 * `randomUUID` on an insecure origin must not stop somebody saving a
 * template.
 */
function newId(): string {
  const c = globalThis.crypto as Crypto | undefined;
  if (c && typeof c.randomUUID === "function") return c.randomUUID().replaceAll("-", "").slice(0, 24);
  return `c${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
}

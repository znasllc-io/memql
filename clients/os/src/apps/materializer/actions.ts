import { useCallback, useRef, useState } from "react";
import {
  fileNameProblem,
  readDescribedMarkdown,
  refusalSentence,
  startMaterialize,
  rowBool,
  type LibraryFileLookup,
  type MaterializeExecute,
  type MaterializeRequest,
  type MaterializeSource,
} from "@znasllc-io/memql-sdk-core/client";

import { recipeKeeps, type CompositionRow, type SourceRow, type SelectorRow } from "./rows";

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
//
// THE SENTENCE AND NOTHING ELSE. A refusal arrives with the call's name in
// front and inside the engine's frame ("composeCancel: MemQL engine failed to
// execute query. Details: compose: ..."); the SDK's refusalSentence takes those
// off, so a person reads only what the cluster wrote for them.

function describe(err: unknown): string {
  return refusalSentence(err);
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
  start: (request: Omit<MaterializeRequest, "requestId">, files?: LibraryFileLookup) => Promise<string>;
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
    async (request: Omit<MaterializeRequest, "requestId">, files?: LibraryFileLookup): Promise<string> => {
      const query = connection?.query ?? null;
      if (query === null) { setError(NOT_CONNECTED); return ""; }
      const format = FORMATS.find(candidate => candidate === request.format);
      if (!format) { setError("Choose what kind of file to make."); return ""; }
      const problem = fileNameProblem(request.name, format);
      if (problem) { setError(problem); return ""; }
      if (request.outputKind === "email_template" && (request.accountIds?.length !== 1 || !request.accountIds[0]?.trim())) {
        setError("Choose an organization for this email template."); return "";
      }
      setBusy(true);
      setError("");
      setCompositionId("");
      try {
        // THE KEY IS THE FACTS AS THEY WERE CLAIMED, the folder included, and nothing
        // above rewrites them: a retry of the same form is the same ask whether or
        // not the cluster accepted its folder the first time.
        const key = JSON.stringify(request);
        if (ask.current?.key !== key) ask.current = { key, id: newId() };
        const requestId = ask.current.id;
        // WHETHER THE FILE IS DESCRIBED IS DECIDED HERE, FROM THE PICKED FILES' ROWS, as the cluster
        // decides it: a description is handed to the Library only when every picked file is one it
        // can attach, and any other file sends the request to the composer. It reads one row per
        // picked file and nothing at all for a request that cannot be described. A refused read is
        // the cluster's sentence and starts nothing, so the same ask can be pressed again.
        const execute = executeOver(query);
        const isDescribed = await readDescribedMarkdown(execute, request, files);
        const id = await startMaterialize(execute, { ...request, requestId });
        ask.current = null;
        setCompositionId(id);
        if (isDescribed) {
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

  const submit = useCallback(
    (request: Omit<MaterializeRequest, "requestId">, files?: LibraryFileLookup): Promise<string> => {
      if (inFlight.current !== null) return inFlight.current;
      const run = start(request, files).finally(() => {
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
    start: submit,
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
  stop: (compositionId: string) => Promise<"stopped" | "finished" | "refused">;
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
    sourceSelectors: sources.filter(recipeKeeps).map(source => ({
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
    stop: async (compositionId) => run(async (query) => {
      const result = await query.composeCancel({ compositionId, reason: "Stopped by the person" });
      const row = result.rows()[0];
      if (!row || typeof row.cancelled !== "boolean") throw new Error("The cluster did not confirm whether the file was stopped.");
      return rowBool(row, "cancelled") ? "stopped" as const : "finished" as const;
    }, "refused" as const),
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
          ...(facts.accountId ? { accountIds: [facts.accountId] } : {}),
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

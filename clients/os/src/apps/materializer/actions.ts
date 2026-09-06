import { useCallback, useState } from "react";

import { useOsConnection } from "../../live/connection";

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
  sources: { kind: string; ref: string; label: string }[];
  draft: string;
  templateId: string;
  folderId: string;
  accountIds: string[];
  deployableKind: string;
}

export interface MaterializeState extends WriteState {
  /** What the last successful call produced, so the surface can say which. */
  compositionId: string;
  materialize: (facts: MaterializeFacts) => Promise<string>;
}

/**
 * Compose, render, stamp and file, in one call.
 *
 * ONE CALL IS THE WHOLE ACT -- the builtin opens the composition, the
 * goal and the goal's first run together, so there is no client-side
 * follow-up write to get half-done. That is the property the Deployables
 * app records about `packageDeploy` and it is the reason this is a
 * builtin rather than three mutations.
 */
export function useMaterialize(): MaterializeState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [compositionId, setCompositionId] = useState("");

  const materialize = useCallback(
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
      setBusy(true);
      setError("");
      setCompositionId("");
      try {
        const result = await query.composeMaterialize({
          name,
          format: facts.format,
          ...(facts.statement.trim() ? { statement: facts.statement.trim() } : {}),
          ...(facts.sources.length > 0 ? { sources: facts.sources } : {}),
          ...(facts.draft.trim() ? { draft: facts.draft } : {}),
          ...(facts.templateId ? { templateId: facts.templateId } : {}),
          ...(facts.folderId ? { folderId: facts.folderId } : {}),
          ...(facts.accountIds.length > 0 ? { accountIds: facts.accountIds } : {}),
          ...(facts.deployableKind ? { deployableKind: facts.deployableKind } : {}),
        });
        const reply = result.rows()[0] ?? null;
        const id = typeof reply?.["compositionId"] === "string" ? (reply["compositionId"] as string) : "";
        setCompositionId(id);
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

  return { busy, error, compositionId, materialize, reset: () => setError("") };
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
  sourceSelectors: { kind: string; selector: string; label: string }[];
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
          ...(facts.description.trim() ? { description: facts.description.trim() } : {}),
          ...(facts.templateId ? { templateId: facts.templateId } : {}),
          ...(facts.folderId ? { folderId: facts.folderId } : {}),
          ...(facts.sourceSelectors.length > 0 ? { sourceSelectors: facts.sourceSelectors } : {}),
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

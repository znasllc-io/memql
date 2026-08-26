import { useCallback, useEffect, useMemo, useState } from "react";
import type { Arrangement } from "@znasllc-io/memql-view-kit";

import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { serializeArrangement } from "../compose/savedViews";
import type { PageManifest } from "./manifest";
import {
  ORIGINAL_VERSION,
  fetchPageOverride,
  fetchPageVersions,
  type PageVersion,
} from "./overrides";

// Resolving what a page LOOKS LIKE right now (epic memql#4661, task
// memql#4668).
//
// ===========================================================================
// THE ORDER, AND WHAT IS NOT IN IT
// ===========================================================================
//
//   the caller's active override for this page id
//     -> else the manifest's seed
//        -> and NEVER a model.
//
// The third line is the load-bearing one. AI runs on ONE explicit action --
// the regenerate button -- and never here (spec D3). A render path that asked
// a provider what a page should look like would cost money on every page view,
// change a page under somebody mid-read, and make the console unusable on a
// cluster with no provider configured. Nothing in this file can reach a
// provider, which is the point rather than an omission.
//
// ===========================================================================
// THE VERSIONS ARE LOADED WITH THE PAGE, NOT ON DEMAND
// ===========================================================================
// Because the newest version IS the resolution: the walk's first step is the
// read that answers "is there an override at all", so a page that loaded the
// override and then loaded the strip separately would issue that read twice.
// The remaining steps are bounded (MAX_VERSIONS) and only run when an override
// exists, which for most pages and most people is never.

export interface PageArrangements {
  // Per section concept id. A section with no entry renders its seed, which is
  // also what every section of a page nobody has regenerated does.
  readonly arrangements: Readonly<Record<string, Arrangement>>;
  // Original, then v1..vN oldest-first. Length 1 (Original alone) when there
  // is no override, which is when the strip does not render.
  readonly versions: readonly PageVersion[];
  // Which version is on screen. The newest by default -- a person opening a
  // page they regenerated sees what they regenerated it to.
  readonly selected: number;
  // Preview an older version WITHOUT writing anything. Reverting is a separate,
  // explicit act (see the strip), because a strip that wrote on click would
  // make browsing your own history a way to lose it.
  readonly select: (version: number) => void;
  readonly loading: boolean;
  readonly error: string;
  // Re-read after a regenerate. The write path calls it rather than mutating
  // state here, so the page always renders what the cluster stored rather than
  // what the client hoped it stored.
  readonly reload: () => void;
}

export function usePageArrangements(
  manifest: PageManifest,
  pageId: string,
): PageArrangements {
  const { query } = useCluster();
  const [versions, setVersions] = useState<readonly PageVersion[]>([ORIGINAL_VERSION]);
  const [selected, setSelected] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  useEffect(() => {
    if (query === null || pageId === "") {
      setVersions([ORIGINAL_VERSION]);
      setSelected(0);
      setLoading(false);
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void fetchPageVersions(query, pageId)
      .then((found) => {
        if (!live) return;
        setVersions(found);
        // The NEWEST, so a person opening a page they regenerated sees what
        // they regenerated it to rather than the Original with their work one
        // click away.
        setSelected(found.length > 0 ? found[found.length - 1]!.version : 0);
        setLoading(false);
      })
      .catch((err: unknown) => {
        if (!live) return;
        // A FAILED OVERRIDE READ LEAVES THE SEED STANDING. The page is not
        // broken -- it is the page it has always been -- so the failure is
        // reported beside it rather than instead of it.
        setVersions([ORIGINAL_VERSION]);
        setSelected(0);
        setError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      });

    return () => {
      live = false;
    };
    // `epoch` is the reload trigger; `query` changes on reconnect.
  }, [query, pageId, epoch]);

  const arrangements = useMemo(() => {
    const chosen = versions.find((v) => v.version === selected);
    // Original, or a selection that no longer exists after a reload: the seed
    // is the answer, and it is the answer that always exists.
    if (chosen === undefined || chosen.version === 0) return {};
    const out: Record<string, Arrangement> = {};
    for (const arrangement of chosen.arrangements) {
      out[arrangement.conceptId] = arrangement;
    }
    // A section the override does not mention keeps its seed. An override that
    // covered section one and not section two is a partial page, and rendering
    // section two blank would lose it -- so `arrangements` is a per-section
    // override rather than a whole-page replacement.
    return out;
  }, [versions, selected]);

  // A page whose manifest changed under a live override is a real state (a
  // release added a section). Nothing to do about it here beyond the
  // per-section fallback above, which handles it: the new section has no
  // override entry and renders its seed.
  void manifest;

  return { arrangements, versions, selected, select: setSelected, loading, error, reload };
}

// usePageOverrideWriter is the ONE write behind "Use this version".
//
// REVERT IS AN APPEND, NOT A DELETE. The history is append-only, so restoring
// an older arrangement means writing it as the NEWEST version -- which is why
// reverting twice is coherent, why a person who reverts and changes their mind
// can go forward again, and why nothing a regeneration produced is ever lost.
// A rollback that destroyed the versions after it would make the strip a trap:
// the one gesture for exploring your own history would be the one that ends
// it.
//
// Reverting to ORIGINAL is the one case with nothing to write. Original is the
// manifest's seed and has no row, so "go back to how the page ships" is
// expressed as an override that carries the SEED's arrangements -- an explicit
// version saying "this is what I want", rather than the absence of a row,
// which would mean "I never regenerated this" and would be undone by the next
// regeneration in a way the person did not ask for.
export interface PageOverrideWriter {
  readonly write: (arrangements: readonly Arrangement[]) => Promise<void>;
  readonly busy: boolean;
  readonly error: string;
}

export function usePageOverrideWriter(pageId: string): PageOverrideWriter {
  const { query } = useCluster();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const write = useCallback(
    async (arrangements: readonly Arrangement[]): Promise<void> => {
      if (query === null) {
        setError("Not connected to a cluster, so nothing was saved.");
        return;
      }
      if (arrangements.length === 0) return;
      setBusy(true);
      setError("");
      try {
        // The existing row's id when there is one: a write is an APPEND onto
        // it, which is what makes the version list a history rather than a
        // pile of unrelated rows.
        const existing = await fetchPageOverride(query, pageId);
        await query.writePageOverride({
          viewId: existing?.id ?? newShortId(),
          targetPageId: pageId,
          arrangements: arrangements.map(serializeArrangement),
          conceptIds: arrangements.map((a) => a.conceptId),
          // MANUAL, not suggested: a person choosing a version from the strip
          // made this arrangement theirs, whatever produced it originally.
          // origin is provenance, and the provenance of this write is a click.
          origin: "manual",
        });
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    },
    [query, pageId],
  );

  return { write, busy, error };
}

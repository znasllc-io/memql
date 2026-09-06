import { CONTENT_KINDS, SOURCE_VALUES } from "./concepts";
import { artifactName, isContentKind, type ArtifactRow } from "./rows";

// The list derivation (design D1, reshaped by epic memql#4842): place / kind /
// source / search / folder scope / sort, as ONE pure fold over the artifacts
// snapshot.
//
// CLIENT-SIDE ON PURPOSE. The owner-scoped set is small, and `folderId == ""`
// cannot match pre-field rows server-side -- a row promoted before folders
// exist has no member at all, and only a client-side fold reads absence and
// the empty string as the same answer: the root. (The `archived != true`
// lesson, applied to the next field.)
//
// ===========================================================================
// PLACES, NOT A CHECKBOX (epic memql#4842, #4846)
// ===========================================================================
// The rail offers four places and the fold answers for the one being looked
// at. Library is what you have; Desktop is the subset sitting on your desks;
// the Bin is what you archived; the Materializer is what was composed from
// the memory graph (epic memql#4981, #4983). The old `showArchived` toggle is
// gone -- archived rows have a place now, not a filter.
//
// TWO OF THE FOUR ARE ID SETS HANDED IN BY THE CALLER, and that is what keeps
// this module pure: desks are shell state and compositions are a fifth feed,
// neither of which a filter should know how to read. The Desktop's set comes
// from the roamed desktop document; the Materializer's comes from joining the
// composition rows' `outputFileId` against each artifact's backing file.
//
// THE PLACE IS `bin`, NOT `archive` (epic memql#4981). One destination had
// two names: the row menu's verb has always been "Move to Bin", the dock
// fixture it lands in is the Bin app, and the rail alone called it Archive.
// A person who archives a file from the menu and then goes looking for
// "Bin" in the rail found "Archive" instead and had to work out that they
// are the same place. ARCHIVE STAYS AS THE VERB -- archiving is what you do,
// the Bin is where it goes -- so `confirmBeforeArchive`, `planArchive` and
// the confirm's own wording are deliberately untouched.

/** No client constraint. */
export const ACCOUNT_ANY = "all";
/** Only rows tied to no client at all. */
export const ACCOUNT_NONE = "none";

export type KindFilter = "all" | (typeof CONTENT_KINDS)[number];
export type SourceFilter = "all" | (typeof SOURCE_VALUES)[number];

export const FILES_PLACES = ["library", "desktop", "bin", "materializer"] as const;
export type FilesPlace = (typeof FILES_PLACES)[number];

export function isFilesPlace(value: unknown): value is FilesPlace {
  return (FILES_PLACES as readonly unknown[]).includes(value);
}

/** What the desk holds, folded from shell state by the caller. */
export interface DeskMembership {
  /** Artifact ids of loose desk file icons (uploads in flight excluded). */
  fileArtifactIds: ReadonlySet<string>;
  /** Library folder ids with a shortcut on any desk. */
  folderIds: ReadonlySet<string>;
}

export const EMPTY_DESK: DeskMembership = {
  fileArtifactIds: new Set(),
  folderIds: new Set(),
};

export interface FilesFilter {
  /** Which of the rail's three places is being looked at. */
  place: FilesPlace;
  /**
   * The folder scope: "" = the place's own root, an id = that folder, null =
   * everywhere. A non-empty search widens to the whole PLACE on its own --
   * someone searching is asking about the place, not about the folder they
   * happen to be in. In the Bin "" already means everything archived:
   * archived rows are a flat population with folders as an optional
   * narrowing, not a tree with a root.
   */
  folderId: string | null;
  kind: KindFilter;
  source: SourceFilter;
  /**
   * The client scope (epic memql#4800): "all" = no account constraint,
   * "none" = only rows with NO client, an id = rows labelled with it.
   *
   * "none" is a real value rather than an absence because "show me what is
   * not filed to anybody" is the question somebody asks when they are trying
   * to file things -- and it is the only one the other two cannot express.
   */
  accountId: string;
  /**
   * The label constraint (epic memql#5009): "" = no constraint, otherwise the
   * exact label a row must carry.
   *
   * =========================================================================
   * A LABEL IS A FACET BEHIND `Refine`, NOT A SECOND RAIL (DESIGN.md rule 2)
   * =========================================================================
   * The portal filtered by `?label=`, and the obvious port of that is a
   * standing label list beside the folder tree. Two rules refuse it. Rule 2:
   * search and facet controls live behind ONE affordance on the Head line,
   * and a permanent label list is precisely the filter chrome that rule
   * exists to remove -- it would also stand over an empty Library, which the
   * same rule forbids outright. And the rail is this app's PLACE LANGUAGE:
   * Library, Desktop, Bin, Materializer, and the folders inside them. A label
   * is not a place -- one file carries several at once -- so a second
   * navigational axis beside the one that answers "where is this" would make
   * a rail click ambiguous about what it means. The label rides in the
   * Refine, and an active one shows as a removable chip beside it exactly as
   * kind, source and client do.
   *
   * "" IS THE SENTINEL rather than a word like ACCOUNT_ANY's "all", because
   * labels are FREE TEXT: "all" is a label somebody may genuinely have used,
   * and a blank one is not -- the editor refuses an empty string, so "" can
   * never collide with a real value.
   */
  label: string;
  search: string;
  /** false = newest first, the default. */
  sortAscending: boolean;
}

/** No label constraint. */
export const LABEL_ANY = "";

export const DEFAULT_FILTER: FilesFilter = {
  place: "library",
  folderId: "",
  kind: "all",
  source: "all",
  accountId: ACCOUNT_ANY,
  label: LABEL_ANY,
  search: "",
  sortAscending: false,
};

function matchesSearch(row: ArtifactRow, needle: string): boolean {
  const q = needle.trim().toLowerCase();
  if (q === "") return true;
  if (artifactName(row).toLowerCase().includes(q)) return true;
  if (row.summary.toLowerCase().includes(q)) return true;
  return row.labels.some((label) => label.toLowerCase().includes(q));
}

/** Whether a row belongs to the Desktop place's population at all: a loose
 *  desk file, or a file inside a folder that has a desk shortcut. */
function inDesktop(row: ArtifactRow, desk: DeskMembership): boolean {
  return desk.fileArtifactIds.has(row.id) || desk.folderIds.has(row.folderId);
}

/**
 * Membership only, structurally.
 *
 * The caller hands in a MAP from artifact id to the composition that made it,
 * because the row menu needs the record's id to hand off with. This module
 * only ever asks whether a row is an output, and saying so in the type keeps
 * the pure fold from depending on the shape of a concept another app owns.
 */
export interface MaterializedSet {
  has(artifactId: string): boolean;
}

/** No composition has claimed anything -- the answer before the feed lands. */
export const NOTHING_MATERIALIZED: MaterializedSet = new Set<string>();

export function applyFilters(
  rows: readonly ArtifactRow[],
  filter: FilesFilter,
  desk: DeskMembership = EMPTY_DESK,
  materialized: MaterializedSet = NOTHING_MATERIALIZED,
): ArtifactRow[] {
  const searching = filter.search.trim() !== "";
  const kept = rows.filter((row) => {
    // The records lens never renders here, under ANY combination (design D2).
    if (!isContentKind(row.kind)) return false;

    // The place decides the population before any facet narrows it.
    if (filter.place === "bin") {
      if (!row.archived) return false;
      if (!searching && filter.folderId !== null && filter.folderId !== "" && row.folderId !== filter.folderId) {
        return false;
      }
    } else if (filter.place === "desktop") {
      if (row.archived) return false;
      if (searching) {
        if (!inDesktop(row, desk)) return false;
      } else if (filter.folderId === "" || filter.folderId === null) {
        // The Desktop root mirrors the desk: loose file icons. A desk
        // folder's contents live one click away, exactly as on the desk.
        if (!desk.fileArtifactIds.has(row.id)) return false;
      } else if (row.folderId !== filter.folderId) {
        return false;
      }
    } else if (filter.place === "materializer") {
      // ARCHIVED OUTPUTS ARE THE BIN'S, not this place's. One file offering
      // Restore from two places is the ambiguity the Bin rename removed, and
      // this place is about what a person HAS.
      if (row.archived) return false;
      if (!materialized.has(row.id)) return false;
      // "" is everything materialized, as in the Bin: these files are spread
      // across the Library's folders and the place is the whole subset, not
      // a tree with a root of its own.
      if (!searching && filter.folderId !== null && filter.folderId !== "" && row.folderId !== filter.folderId) {
        return false;
      }
    } else {
      if (row.archived) return false;
      if (!searching && filter.folderId !== null && row.folderId !== filter.folderId) return false;
    }

    if (filter.kind !== "all" && row.kind !== filter.kind) return false;
    if (filter.source !== "all" && row.source !== filter.source) return false;
    // EXACT, never a prefix or a case-fold. A label is a name somebody chose;
    // two labels differing only in case are two labels, and quietly folding
    // them would show rows under a name that is not written on them. The
    // fuzzy reading already exists one line down -- `matchesSearch` looks
    // inside labels -- so the two questions stay separable.
    if (filter.label !== LABEL_ANY && !row.labels.includes(filter.label)) return false;
    if (!matchesSearch(row, filter.search)) return false;
    // THE CLIENT SCOPE READS ABSENCE AND THE EMPTY LIST AS ONE ANSWER, and it
    // has to: every row promoted before `accountIds` existed carries no key at
    // all, so a filter that distinguished them would hide the entire
    // pre-existing Library from the "no client" view -- which is exactly the
    // view somebody uses to find what still needs filing.
    if (filter.accountId === ACCOUNT_NONE && row.accountIds.length > 0) return false;
    if (
      filter.accountId !== ACCOUNT_ANY &&
      filter.accountId !== ACCOUNT_NONE &&
      !row.accountIds.includes(filter.accountId)
    ) {
      return false;
    }
    return true;
  });
  const direction = filter.sortAscending ? 1 : -1;
  return kept.sort(
    (a, b) => direction * (a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id)),
  );
}

/**
 * Every label present in a population, de-duplicated and alphabetical.
 *
 * A CLIENT-SIDE FOLD OVER THE SEEDED SET, like every other facet here, and
 * for the reason the README gives about `libraryArtifactsByLens`: the seed is
 * picked so one paged read holds the complete truth, and the engine's own
 * `libraryArtifactsByLabel` carries the default `archived != true` conjunct,
 * so a server-side label read would disagree with the archived, kind, source,
 * client and search filters standing beside it in the same Refine panel.
 *
 * The caller passes the rows that pass every OTHER facet, so a label offered
 * always has at least one row behind it in the view being looked at -- and
 * the one currently selected stays offered, because clearing the label facet
 * is what produces that population.
 */
export function labelsOf(rows: readonly ArtifactRow[]): string[] {
  const seen = new Set<string>();
  for (const row of rows) {
    for (const label of row.labels) {
      const text = label.trim();
      if (text !== "") seen.add(text);
    }
  }
  return [...seen].sort((a, b) => a.localeCompare(b));
}

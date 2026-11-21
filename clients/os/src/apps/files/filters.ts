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
// The rail offers five places and the fold answers for the one being looked
// at (Shared with me answers with nothing: see FILES_PLACES). Library is what you have; Desktop is the subset sitting on your desks;
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
// two names: the row menu's verb was "Move to Bin", the dock
// fixture it lands in is the Bin app, and the rail alone called it Archive.
// A person who archives a file from the menu and then goes looking for
// "Bin" in the rail found "Archive" instead and had to work out that they
// are the same place. In CODE, archive stays the verb -- `confirmBeforeArchive`,
// `planArchive` -- because it is the engine's mutation. What a person reads
// is "Delete" (José, Nov 18 2026), and the Bin is where a deleted file waits.

/** No client constraint. */
export const ACCOUNT_ANY = "all";
/** Only rows tied to no client at all. */
export const ACCOUNT_NONE = "none";

export type KindFilter = "all" | (typeof CONTENT_KINDS)[number];
export type SourceFilter = "all" | (typeof SOURCE_VALUES)[number];

// `shared` is what other people shared WITH the caller (Nov 2026). Its tiles are
// share notices, read on their own feed (useSharing.ts): none of the caller's
// own artifacts belongs to it, so the fold below keeps nothing there.
export const FILES_PLACES = ["library", "desktop", "materializer", "shared", "bin"] as const;
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

/**
 * `source`, `accountId` AND `label` WERE REFINE'S FACETS, and they are back
 * (Jesús, Nov 20 2026, Option C of draft p17): Label, Source and Client are
 * groups in the search panel's kind menu, under the kinds of file (see
 * `filterMenuPick`). `kind` (the artifact's index kind) has no control and stays
 * at its default.
 */
export interface FilesFilter {
  /** Which of the rail's places is being looked at. */
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
   * A LABEL IS A FACET IN THE SEARCH PANEL, NOT A SECOND RAIL (DESIGN.md rule 2)
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
   * search panel's kind menu, as its Label group (see `filterMenuPick`).
   *
   * "" IS THE SENTINEL rather than a word like ACCOUNT_ANY's "all", because
   * labels are FREE TEXT: "all" is a label somebody may genuinely have used,
   * and a blank one is not -- the editor refuses an empty string, so "" can
   * never collide with a real value.
   */
  label: string;
  search: string;
  /** The search panel's kind of file (see FILE_TYPES). */
  type: FileType;
  /** The search panel's Added menu. */
  added: AddedWithin;
  /** "newest" is the default; Settings may start a window on "oldest". */
  sort: FilesSort;
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
  type: "all",
  added: "any",
  sort: "newest",
};

// ===========================================================================
// THE SEARCH PANEL'S QUESTIONS (José, Nov 19 2026; Jesús's picks, Nov 20)
// ===========================================================================
// A kind of file, when it was added, and an order. The kind is read off the
// file's NAME first (its extension), then its MIME type, then the format the
// index recorded -- the order the tile's glyph reads a name in (Tiles.tsx
// `typeLabel`), so a tile that says PDF is never filed under Misc. This table
// is the ONE place the mapping lives.

export const FILE_TYPES = [
  ["all", "All"],
  ["folders", "Folders"],
  ["documents", "Documents"],
  ["images", "Images"],
  ["media", "Media"],
  ["sheets", "Sheets"],
  ["archives", "Archives"],
  ["misc", "Misc"],
] as const;
export type FileType = (typeof FILE_TYPES)[number][0];
/** What one file is: every FileType except the two that are not a file's own. */
export type FileKind = Exclude<FileType, "all" | "folders">;

const EXTENSIONS: Record<Exclude<FileKind, "misc">, readonly string[]> = {
  // Slides count as documents: a deck is something you read, and Misc is for
  // files nothing else claims.
  documents: ["pdf", "doc", "docx", "txt", "md", "markdown", "rtf", "odt", "pages", "epub", "ppt", "pptx", "key", "odp"],
  images: ["jpg", "jpeg", "png", "gif", "webp", "svg", "heic", "heif", "bmp", "tif", "tiff", "avif", "ico"],
  media: ["mp3", "wav", "m4a", "aac", "flac", "ogg", "oga", "opus", "aiff", "mp4", "m4v", "mov", "webm", "mkv", "avi", "wmv", "mpg", "mpeg"],
  sheets: ["xlsx", "xls", "csv", "tsv", "ods", "numbers"],
  // A .tar.gz reads as "gz", its last extension, as the tile's glyph does.
  archives: ["zip", "tar", "gz", "tgz", "7z", "rar", "bz2", "xz"],
};
const KIND_OF_EXTENSION = new Map<string, FileKind>(
  Object.entries(EXTENSIONS).flatMap(([kind, exts]) => exts.map((ext) => [ext, kind as FileKind] as const)),
);

function kindOfMime(mime: string): FileKind | null {
  const m = mime.toLowerCase().split(";")[0]?.trim() ?? "";
  if (m === "") return null;
  // Sheets and archives first: text/csv is a sheet, not text, and
  // application/x-zip-compressed is an archive whatever else it says.
  if (/spreadsheet|excel|csv|tab-separated|numbers/.test(m)) return "sheets";
  if (/zip|x-tar|gzip|x-7z|x-rar|rar-compressed|bzip|x-xz/.test(m)) return "archives";
  if (m.startsWith("image/")) return "images";
  if (m.startsWith("audio/") || m.startsWith("video/")) return "media";
  if (m === "text/plain" || m === "text/markdown" || /pdf|msword|wordprocessing|opendocument\.text|rtf|presentation|powerpoint|epub/.test(m)) {
    return "documents";
  }
  return null;
}

const KIND_OF_FORMAT: Record<string, FileKind> = {
  markdown: "documents",
  document: "documents",
  pdf: "documents",
  text: "documents",
  spreadsheet: "sheets",
  image: "images",
};

/** Which kind of file a row is; "misc" when neither its name, its MIME type nor its format says. */
export function fileKindOf(row: Pick<ArtifactRow, "title" | "mimeType" | "format">): FileKind {
  const name = row.title.trim().toLowerCase();
  const dot = name.lastIndexOf(".");
  const ext = dot > 0 ? name.slice(dot + 1) : "";
  return KIND_OF_EXTENSION.get(ext) ?? kindOfMime(row.mimeType) ?? KIND_OF_FORMAT[row.format] ?? "misc";
}

export const ADDED_WITHIN = [
  ["any", "Any time"],
  ["today", "Today"],
  ["7d", "Last 7 days"],
  ["30d", "Last 30 days"],
] as const;
export type AddedWithin = (typeof ADDED_WITHIN)[number][0];

const DAY_MS = 24 * 60 * 60 * 1000;

/** Whether a creation time falls in the Added window; Today starts at local midnight, and an unreadable time never does. */
export function addedWithin(createdAt: string | undefined, added: AddedWithin, now: number): boolean {
  if (added === "any") return true;
  const day = new Date(now);
  const since = added === "today"
    ? new Date(day.getFullYear(), day.getMonth(), day.getDate()).getTime()
    : now - (added === "7d" ? 7 : 30) * DAY_MS;
  return Date.parse(createdAt ?? "") >= since;
}

export const FILES_SORTS = [
  ["name", "Name"],
  ["newest", "Newest first"],
  ["oldest", "Oldest first"],
  ["size", "Size"],
  ["kind", "Kind"],
] as const;
export type FilesSort = (typeof FILES_SORTS)[number][0];

const KIND_ORDER: readonly FileType[] = FILE_TYPES.map(([type]) => type);
const byName = (a: ArtifactRow, b: ArtifactRow) =>
  artifactName(a).localeCompare(artifactName(b), undefined, { numeric: true, sensitivity: "base" }) || a.id.localeCompare(b.id);

/**
 * The order a sort names. Newest and Oldest keep the server's own page order
 * (created-at, then id). Size is largest first, and a file whose size is not
 * known (a document or generated output with no stored bytes) goes last.
 */
export function compareRows(sort: FilesSort, sizeOf: (row: ArtifactRow) => number | undefined = () => undefined) {
  return (a: ArtifactRow, b: ArtifactRow): number => {
    if (sort === "newest" || sort === "oldest") {
      return (sort === "oldest" ? 1 : -1) * (a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id));
    }
    if (sort === "size") return (sizeOf(b) ?? -1) - (sizeOf(a) ?? -1) || byName(a, b);
    if (sort === "kind") return KIND_ORDER.indexOf(fileKindOf(a)) - KIND_ORDER.indexOf(fileKindOf(b)) || byName(a, b);
    return byName(a, b);
  };
}

/**
 * Whether the view needs the place's WHOLE population rather than the pages
 * scrolled so far. The server pages newest (or oldest) first and narrows only
 * by what `libraryBrowseArgs` sends, so a name, size or kind order, a kind of
 * file and an Added window are answered here, over every page.
 */
export function needsWholePlace(filter: Pick<FilesFilter, "sort" | "type" | "added">): boolean {
  return (filter.sort !== "newest" && filter.sort !== "oldest") ||
    (filter.type !== "all" && filter.type !== "folders") ||
    filter.added !== "any";
}

function matchesSearch(row: ArtifactRow, needle: string): boolean {
  const q = needle.trim().toLowerCase();
  if (q === "") return true;
  if (artifactName(row).toLowerCase().includes(q)) return true;
  if (row.summary.toLowerCase().includes(q)) return true;
  if (row.id.toLowerCase() === q || row.producedByWorkerName.toLowerCase().includes(q)) return true;
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
  sizeOf?: (row: ArtifactRow) => number | undefined,
  now: number = Date.now(),
): ArtifactRow[] {
  const searching = filter.search.trim() !== "";
  // Folders is a question about folders: no file answers it.
  if (filter.type === "folders") return [];
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
    } else if (filter.place === "shared") {
      return false;
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
      if (!searching && filter.folderId && row.folderId !== filter.folderId) return false;
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
    if (filter.type !== "all" && fileKindOf(row) !== filter.type) return false;
    if (!addedWithin(row.createdAt, filter.added, now)) return false;
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
  return kept.sort(compareRows(filter.sort, sizeOf));
}

/**
 * Every label present in a population, de-duplicated and alphabetical.
 *
 * A CLIENT-SIDE FOLD OVER THE SEEDED SET, like every other facet here, and
 * for the reason the README gives about `libraryArtifactsByLens`: the seed is
 * picked so one paged read holds the complete truth, and the engine's own
 * `libraryArtifactsByLabel` carries the default `archived != true` conjunct,
 * so a server-side label read would disagree with the archived, kind, source,
 * client and search filters standing beside it in the same search panel.
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

/**
 * The sources at least one of these rows has, in the concept's own order: the
 * Source group offers only these, the way the Label group is absent when
 * nothing is labelled. ponytail: folds the rows LOADED so far (labels come from
 * a server facet read; sources have none), so a source only on a not-yet-loaded
 * page appears once its page loads; add a facet to libraryBrowseFacets if needed.
 */
export function sourcesOf(rows: readonly Pick<ArtifactRow, "source">[], picked: SourceFilter = "all"): (typeof SOURCE_VALUES)[number][] {
  const seen = new Set<string>(rows.map((row) => row.source));
  // The one picked stays listed, so the person can see it and undo it.
  return SOURCE_VALUES.filter((source) => seen.has(source) || source === picked);
}

// ===========================================================================
// THE KIND MENU HOLDS FOUR SINGLE CHOICES (Jesús, Nov 20 2026, draft p17 C)
// ===========================================================================
// Label, source and client came back INSIDE the search panel's kind menu, as
// groups under the kinds of file: Kind, Label, Source, Client. Each group is
// its own single choice -- one kind AND one label AND one source AND one
// client can be on together -- so one option value carries its group in front
// of a colon ("label:invoices") and `filterMenuPick` turns it back into the
// one field it sets. Only the first colon splits: a label is free text.

/** Words for the source values the old Refine offered (it showed the raw values). */
export const SOURCE_LABELS: Record<SourceFilter, string> = {
  all: "Any source",
  uploaded: "Uploaded",
  exported: "Exported",
  workbench_generated: "Made on the workbench",
  computer_use: "Computer use",
  agent_generated: "Made by an agent",
  derived: "Derived",
  user_created: "Written here",
  live: "Live",
  app_session: "Recorded from an app session",
  vision_input: "Image sent to an app",
  pipeline: "Pipeline",
};

export type FilterMenuGroup = "type" | "label" | "source" | "client";

export function filterMenuValue(group: FilterMenuGroup, value: string): string {
  return `${group}:${value}`;
}

/** The one filter field an option of the kind menu sets. */
export function filterMenuPick(option: string): Partial<FilesFilter> {
  const at = option.indexOf(":");
  const value = option.slice(at + 1);
  switch (option.slice(0, at)) {
    case "type": return { type: value as FileType };
    case "label": return { label: value };
    case "source": return { source: value as SourceFilter };
    case "client": return { accountId: value };
    default: return {};
  }
}

/** How many of label, source and client are on: the kind menu's button adds "+N" to the kind. */
export function otherFiltersOn(filter: Pick<FilesFilter, "label" | "source" | "accountId">): number {
  return [filter.label !== LABEL_ANY, filter.source !== "all", filter.accountId !== ACCOUNT_ANY].filter(Boolean).length;
}

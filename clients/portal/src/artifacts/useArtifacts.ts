import { useCallback, useEffect, useMemo, useState } from "react";
import { newShortId, rowArray, rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useAuth } from "../auth/AuthProvider";
import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";
import { ARTIFACT_LENSES, FILE_KIND, fileIdFromSourceRef } from "./concepts";
import { uploadArtifact, type ArtifactUploadResult } from "./transport";

// The artifacts surface's hooks: useArtifacts for the LIST screen (read,
// upload, archive, the archived toggle), useArtifactSearch for search by
// meaning, and useArtifactDetail for the DETAIL screen's row, label editor,
// backing file and training. One file because the brief scopes one hooks
// module for this feature (contrast sites/, which splits useSites /
// useSiteDetail / useSiteHistory across three).
//
// ===========================================================================
// THE TIER, AS IT ACTUALLY IS (memql#4340 / D8)
// ===========================================================================
//
// v1:library:artifact declares the COMPOSITE tier,
// `@rowAuthz(owner="ownerUserId", clusterOwner)` -- the owner, OR a cluster
// owner (dsl/library/concepts.memql). v1:library:file and
// v1:library:fileChunk declare the same form. The engine therefore injects a
// per-row filter on every read of this concept and admits every row on the
// way out through rowAuthzAdmits (component/memql/rowauthz_enforce.go), which
// is the half no filter spelling can steer -- it covers a raw query string,
// graph expansion and a subscription.
//
// (An earlier version of this comment said the concept declared NO tier and
// was therefore "not gated, not measured", and built both decisions below on
// top of that. #4340 closed that gap at the moment this page became a primary
// surface. Both decisions survive -- the reasons do not.)
//
// STILL A NAMED QUERY, FOR TWO REASONS THAT ARE NOW ABOUT MEANING, NOT ABOUT
// A MISSING GATE. useConceptRows(ARTIFACT_CONCEPT_ID) -- the generic concept
// browse -- would no longer leak another user's rows to an ordinary caller.
// It would still be the wrong read:
//
//   1. The composite tier's second half is a BYPASS. A cluster owner is
//      admitted for EVERY row, so the generic browse hands an operator the
//      whole cluster's Library on a page whose title, blurb and empty state
//      all say "yours". That is the reach the composite tier is FOR -- an
//      operator console that wants the cross-user view needs a read that does
//      not scope itself -- and this page is not that surface. The named
//      queries carry `ownerUserId==actor.userId` as an explicit top-level
//      conjunct, so they mean "mine" for every caller including the operator.
//   2. The named reads carry what the generic browse cannot express: the
//      archived exclusion (`archived != true`, and the spelling is
//      load-bearing -- see libraryArtifacts' own comment), the sort, the
//      page size, and the artifactFull projection.
//
// STILL NO LIVE CDC BAND, and this reason is new. Row admission gates
// SUBSCRIPTIONS too (memql#4309): a graph.node.* event reaches a stream only
// if rowAuthzAdmits admits it for that stream's actor. So a live band here
// would be correctly scoped for an ordinary user -- and would deliver every
// OTHER user's artifacts to a cluster owner, by the same bypass as (1). A
// band that is right for most readers and wrong for the operator is not a
// band worth having on this page. Writes bump an epoch and re-run the named
// reads instead, the shape useCampaigns.ts uses.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// rowLabels reads a row's `labels` field as a plain string[], defensively --
// the wire carries a JSON array and a non-string entry should be dropped
// rather than rendered as "[object Object]".
function rowLabels(row: Row | null): string[] {
  const raw = rowArray(row, "labels") ?? [];
  return raw.filter((entry): entry is string => typeof entry === "string");
}

function rowStrings(row: Row | null, key: string): string[] {
  const raw = rowArray(row, key) ?? [];
  return raw.filter((entry): entry is string => typeof entry === "string");
}

function rowId(row: Row): string {
  return rowString(row, "id");
}

// isArchived reads the soft-delete flag the artifactFull shape now projects.
// `!== true` rather than a truthiness test, and never `=== false`: a row
// written before the field existed carries no `archived` member at all, and
// the two spellings disagree about exactly those rows -- the same trap
// libraryArtifacts' filter comment documents on the SQL side.
function isArchived(row: Row | null): boolean {
  return row?.["archived"] === true;
}

// -------------------- the LIST screen --------------------

export interface CreateArtifactInput {
  title: string;
  summary: string;
  body: string;
}

export interface UploadArtifactInput {
  file: File;
  labels: string[];
}

export interface ArtifactsState {
  // The rows to render: libraryArtifacts's full set when no label filter is
  // active, libraryArtifactsByLabel's narrower read when one is.
  rows: Row[];
  // Every label seen across the caller's WHOLE library, independent of the
  // active filter -- narrowing to one label must not make the others
  // disappear from the control that would get you back out of it (mirrors
  // ConceptsPage's domain chips, whose own comment states the same rule).
  labels: string[];
  loading: boolean;
  error: string;
  reload: () => void;
  createBusy: boolean;
  createError: string;
  // The outcome of the last create, once it succeeds. Distinct from
  // createError so both can be shown to a form that keeps its errors and
  // successes on separate lines, same shape as useSites' createError alone
  // (this surface additionally needs a success line because -- unlike a
  // site, which is live the moment its row exists -- a created artifact is
  // not visible until the indexing automation promotes it, which is a fact
  // worth saying rather than leaving the operator to guess why the list
  // did not change).
  createMessage: string;
  createArtifact: (input: CreateArtifactInput) => void;
  // The upload half (memql#4343). Progress is a 0..1 fraction, or -1 for
  // "started, length not yet known" -- distinct from 0 ("nothing sent yet")
  // because a bar pinned at zero and a bar with no measurement are different
  // things to say.
  uploadBusy: boolean;
  uploadProgress: number;
  uploadError: string;
  uploadMessage: string;
  uploadFile: (input: UploadArtifactInput) => void;
  // Archive: the first write that takes something OUT of the Library, and a
  // soft one. Behind a ConfirmDialog at the page level.
  archiveBusy: boolean;
  archiveError: string;
  archiveArtifact: (artifactId: string) => void;
}

export function useArtifacts(label: string, includeArchived: boolean): ArtifactsState {
  const { query } = useCluster();
  const { authSource } = useAuth();
  const [allRows, setAllRows] = useState<Row[]>([]);
  const [filteredRows, setFilteredRows] = useState<Row[]>([]);
  // Starts true: a fetch is effectively in flight from the moment this hook
  // mounts (or is about to be, the instant a connection exists), so "loading"
  // is the honest initial state -- not "confirmed empty," which is what
  // false would silently claim before any read has even been attempted. See
  // the useEffect below for why `query === null` must not override this
  // back to false either.
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState("");
  const [createMessage, setCreateMessage] = useState("");
  const [uploadBusy, setUploadBusy] = useState(false);
  const [uploadProgress, setUploadProgress] = useState(0);
  const [uploadError, setUploadError] = useState("");
  const [uploadMessage, setUploadMessage] = useState("");
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [archiveError, setArchiveError] = useState("");

  useEffect(() => {
    if (query === null) {
      // Not yet connected -- NOT a definitive "no artifacts" answer. Leave
      // `loading` exactly as it is (true from mount, or from a read this
      // effect already has outstanding) rather than asserting emptiness
      // before a read has even been attempted. Fix round 1: this branch
      // used to force loading=false here, which is what let the empty
      // state render on the very first paint, before the connection (let
      // alone the read) had a chance to happen -- on every single visit,
      // not just a rare race, because `query` is genuinely null for at
      // least the first render of every mount.
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    // THE UNFILTERED READ DEPENDS ON WHETHER ARCHIVED ROWS ARE WANTED, and
    // that is a fact about the DSL rather than a preference. libraryArtifacts
    // -- the default list -- carries `archived != true` in its own filter, so
    // there is no argument that widens it. The facet reads deliberately do
    // NOT carry that conjunct (a caller who asked for one lens asked about
    // that lens), so the union of the two lens reads IS the caller's whole
    // Library including archived rows. `lens` is a required enum of exactly
    // two members, so the union is total rather than a sample.
    const base: Promise<Row[]> = includeArchived
      ? Promise.all(ARTIFACT_LENSES.map((lens) => query.libraryArtifactsByLens({ lens }))).then(
          (results) => mergeById(results.flatMap((result) => result.rows())),
        )
      : query.libraryArtifacts({}).then((result) => result.rows());

    // The narrower read joins it only when a filter is active. One settle for
    // both, same reasoning useCampaigns.ts gives for its three-way
    // Promise.all: the alternative is two staggered arrivals fighting over
    // which one the loading skeleton is honest about.
    void Promise.all([
      base,
      label === "" ? null : query.libraryArtifactsByLabel({ label }),
    ])
      .then(([full, narrowed]) => {
        if (!live) return;
        setAllRows(full);
        // The LABEL read has no archive filter either, so the exclusion is
        // applied here for that path. Client-side, and only for the label
        // path: the default list is already narrowed server-side and must
        // not be re-filtered into disagreeing with its own page size.
        const rows = narrowed
          ? narrowed.rows().filter((row) => includeArchived || !isArchived(row))
          : full;
        setFilteredRows(rows);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, label, includeArchived, epoch]);

  const labels = useMemo(() => {
    const set = new Set<string>();
    for (const row of allRows) {
      for (const one of rowLabels(row)) set.add(one);
    }
    return [...set].sort((a, b) => a.localeCompare(b));
  }, [allRows]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  const createArtifact = useCallback(
    (input: CreateArtifactInput) => {
      if (query === null) return;
      setCreateBusy(true);
      setCreateError("");
      setCreateMessage("");
      void query
        .createGeneratedOutput({
          outputId: newShortId(),
          title: input.title,
          summary: omitBlank(input.summary),
          body: omitBlank(input.body),
          source: "user_created",
        })
        .then(() => {
          setCreateMessage(
            "Created. The Library folds it into an artifact automatically -- reload in a moment " +
              "if it has not appeared below yet.",
          );
          setEpoch((n) => n + 1);
        })
        .catch((err: unknown) => setCreateError(describe(err)))
        .finally(() => setCreateBusy(false));
    },
    [query],
  );

  const uploadFile = useCallback(
    (input: UploadArtifactInput) => {
      setUploadBusy(true);
      setUploadProgress(0);
      setUploadError("");
      setUploadMessage("");
      // The credential is read at CALL TIME through the PortalAuthSource
      // seam (src/cluster/auth.ts), never held: an upload issued after a
      // rotation must carry the rotated token, exactly as sdk/ts's
      // uploadAttachment reads its connection's current bearer rather than
      // the one it dialed with.
      void authSource
        .bearer()
        .then((bearer) =>
          uploadArtifact({
            file: input.file,
            labels: input.labels,
            bearer,
            onProgress: (fraction) => setUploadProgress(fraction),
          }),
        )
        .then((result: ArtifactUploadResult) => {
          setUploadProgress(1);
          setUploadMessage(`Uploaded "${input.file.name}". It is in your Library below.`);
          // The handler waits for the promotion automation before it answers,
          // so the index row exists by the time this resolves -- the re-read
          // finds it rather than racing it.
          setEpoch((n) => n + 1);
          return result;
        })
        .catch((err: unknown) => {
          setUploadProgress(0);
          setUploadError(describe(err));
        })
        .finally(() => setUploadBusy(false));
    },
    [authSource],
  );

  const archiveArtifact = useCallback(
    (artifactId: string) => {
      if (query === null) return;
      setArchiveBusy(true);
      setArchiveError("");
      void query
        .archiveArtifact({ artifactId })
        .then(() => setEpoch((n) => n + 1))
        .catch((err: unknown) => setArchiveError(describe(err)))
        .finally(() => setArchiveBusy(false));
    },
    [query],
  );

  return {
    rows: filteredRows,
    labels,
    loading,
    error,
    reload,
    createBusy,
    createError,
    createMessage,
    createArtifact,
    uploadBusy,
    uploadProgress,
    uploadError,
    uploadMessage,
    uploadFile,
    archiveBusy,
    archiveError,
    archiveArtifact,
  };
}

// mergeById folds the two lens reads into one set, newest first. Both reads
// sort by createdAt descending server-side; interleaving them keeps that
// order without a second sort key the queries do not guarantee.
function mergeById(rows: Row[]): Row[] {
  const seen = new Set<string>();
  const out: Row[] = [];
  for (const row of rows) {
    const id = rowId(row);
    if (id !== "" && seen.has(id)) continue;
    if (id !== "") seen.add(id);
    out.push(row);
  }
  out.sort((a, b) => rowString(b, "createdAt").localeCompare(rowString(a, "createdAt")));
  return out;
}

// -------------------- search by meaning --------------------

// A hit is NOT an artifact row and must not be rendered as one. It is the
// builtin's own result shape (integrations/library/similarity.go's
// similarArtifactHit): the artifact folded up from its best-scoring chunk,
// carrying the score and the matched text that explains the score. The
// artifact fields it repeats -- title, kind, summary, labels -- come from the
// owner-gated re-read the builtin performs per hit, so a hit is already the
// answer to "may this caller see this row".
export interface ArtifactHit {
  artifactId: string;
  fileId: string;
  score: number;
  seq: number;
  snippet: string;
  title: string;
  kind: string;
  summary: string;
  labels: string[];
}

export interface ArtifactSearchState {
  hits: ArtifactHit[];
  loading: boolean;
  error: string;
  // True once a non-empty query has settled -- what tells "no matches" apart
  // from "nothing asked yet", which look identical from an empty hit list.
  searched: boolean;
}

export function useArtifactSearch(text: string): ArtifactSearchState {
  const { query } = useCluster();
  const [hits, setHits] = useState<ArtifactHit[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [searched, setSearched] = useState(false);

  useEffect(() => {
    const phrase = text.trim();
    if (phrase === "") {
      // An empty query is not a search that found nothing -- it is the list
      // screen. Clearing here is what makes deleting the query return to it.
      setHits([]);
      setLoading(false);
      setError("");
      setSearched(false);
      return;
    }
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");

    void query
      .librarySimilarArtifacts({ text: phrase, limit: 20 })
      .then((result) => {
        if (!live) return;
        setHits(result.rows().map(toHit));
        setSearched(true);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, text]);

  return { hits, loading, error, searched };
}

function toHit(row: Row): ArtifactHit {
  return {
    artifactId: rowString(row, "artifactId"),
    fileId: rowString(row, "fileId"),
    score: rowNumber(row, "score"),
    seq: rowNumber(row, "seq"),
    snippet: rowString(row, "snippet"),
    title: rowString(row, "title"),
    kind: rowString(row, "kind"),
    summary: rowString(row, "summary"),
    labels: rowLabels(row),
  };
}

// -------------------- the DETAIL screen --------------------

export interface ArtifactDetailState {
  artifact: Row | null;
  loading: boolean;
  error: string;
  // The label list this screen renders. Held SEPARATELY from
  // artifact.labels and updated optimistically by addLabel/removeLabel --
  // the caller already knows which single label changed (LabelChips.tsx's
  // own header comment makes the same point about its onAdd/onRemove
  // contract), so waiting on a re-read to reflect an add/remove that the
  // builtin idempotently guarantees would just be added latency.
  labels: string[];
  // Disables every LabelChips control while an add/remove is in flight --
  // threaded straight through to LabelChips' own `busy` prop.
  labelBusy: boolean;
  labelError: string;
  // A one-line announcement for a role="status" live region at the page
  // level (LabelChips itself renders no live region -- see its own header
  // comment on why that convention lives one level up). Overwritten by each
  // successful add/remove so a screen reader hears the most recent one.
  announcement: string;
  addLabel: (label: string) => void;
  removeLabel: (label: string) => void;
  // The backing v1:library:file, for a kind=file artifact only. null for the
  // other five backing concepts, which is a different KIND of row rather than
  // a failed read -- see fileIdFromSourceRef.
  file: Row | null;
  fileId: string;
  // Domains this file has been trained into, and every domain this caller has
  // trained ANY of their files into. See the picker's own comment on
  // ArtifactDetailPage for why the second list is the best the engine can
  // offer and why the server stays the authority.
  trainedDomains: string[];
  knownDomains: string[];
  trainBusy: boolean;
  trainError: string;
  train: (domainId: string) => void;
  archived: boolean;
  archiveBusy: boolean;
  archiveError: string;
  archive: () => void;
}

export function useArtifactDetail(artifactId: string): ArtifactDetailState {
  const { query } = useCluster();
  const [artifact, setArtifact] = useState<Row | null>(null);
  // Starts true, for the same reason useArtifacts' does: a fetch is
  // effectively in flight from mount, so "loading" is the truthful initial
  // state. See the useEffect below for why an empty artifactId and a null
  // query are handled as two DIFFERENT cases rather than one combined
  // guard -- only the first is a genuine terminal answer.
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [labels, setLabels] = useState<string[]>([]);
  const [labelBusy, setLabelBusy] = useState(false);
  const [labelError, setLabelError] = useState("");
  const [announcement, setAnnouncement] = useState("");
  const [file, setFile] = useState<Row | null>(null);
  const [knownDomains, setKnownDomains] = useState<string[]>([]);
  const [trainBusy, setTrainBusy] = useState(false);
  const [trainError, setTrainError] = useState("");
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [archiveError, setArchiveError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (artifactId === "") {
      // Structurally nothing to fetch, ever, given the current route -- a
      // genuine terminal state, unlike the connection race below.
      setArtifact(null);
      setLoading(false);
      setError("");
      setLabels([]);
      return;
    }
    if (query === null) {
      // Not yet connected -- NOT a definitive "no artifact" answer. Leave
      // `loading` exactly as it is (true from mount, or from a read this
      // effect already has outstanding) rather than asserting "not found"
      // before a read has even been attempted. Fix round 1: this used to be
      // one combined guard with the branch above, which forced loading=false
      // and artifact=null here too -- rendering "No artifact has that id"
      // for a perfectly valid id, on every single visit (query is null for
      // at least the first render of every mount), not just a rare race.
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void query
      .libraryArtifactById({ artifactId })
      .then((result) => {
        if (!live) return null;
        const row = result.rows()[0] ?? null;
        setArtifact(row);
        setLabels(rowLabels(row));
        // The backing file, when there is one. Sequential rather than
        // parallel because the file's id is not knowable until the index row
        // has been read -- sourceConceptRef IS the pointer.
        const backingId = fileIdFromSourceRef(rowString(row, "sourceConceptRef"));
        if (backingId === "") {
          setFile(null);
          return null;
        }
        return query.libraryFileById({ fileId: backingId });
      })
      .then((fileResult) => {
        if (!live || fileResult === null) return;
        setFile(fileResult.rows()[0] ?? null);
      })
      .catch((err: unknown) => {
        if (live) setError(describe(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });

    return () => {
      live = false;
    };
  }, [query, artifactId, epoch]);

  // The domains this caller has trained anything into. A separate read
  // because it is about the CALLER, not about this artifact, and it must
  // still answer for an artifact that has never been trained.
  useEffect(() => {
    if (query === null) return;
    let live = true;
    void query
      .libraryFilesForOwner({})
      .then((result) => {
        if (!live) return;
        const set = new Set<string>();
        for (const row of result.rows()) {
          for (const domain of rowStrings(row, "trainedIntoDomainIds")) set.add(domain);
        }
        setKnownDomains([...set].sort((a, b) => a.localeCompare(b)));
      })
      .catch(() => {
        // A failed read here costs the SUGGESTIONS, not the control: the
        // picker still accepts a domain id typed in full, and the server is
        // the authority on whether the caller may write to it either way. So
        // this is deliberately not surfaced as a page error.
      });
    return () => {
      live = false;
    };
  }, [query, epoch]);

  const addLabel = useCallback(
    (label: string) => {
      if (query === null || artifactId === "") return;
      setLabels((current) => [...current, label]);
      setLabelBusy(true);
      setLabelError("");
      void query
        .libraryAddArtifactLabel({ artifactId, label })
        .then(() => setAnnouncement(`Added label "${label}".`))
        .catch((err: unknown) => {
          // Roll back the optimistic add -- the write did not happen, so the
          // chip must not keep claiming it did.
          setLabels((current) => current.filter((one) => one !== label));
          setLabelError(describe(err));
        })
        .finally(() => setLabelBusy(false));
    },
    [query, artifactId],
  );

  const removeLabel = useCallback(
    (label: string) => {
      if (query === null || artifactId === "") return;
      setLabels((current) => current.filter((one) => one !== label));
      setLabelBusy(true);
      setLabelError("");
      void query
        .libraryRemoveArtifactLabel({ artifactId, label })
        .then(() => setAnnouncement(`Removed label "${label}".`))
        .catch((err: unknown) => {
          // Roll back the optimistic remove, guarding against a duplicate in
          // case the chip is still there for another reason.
          setLabels((current) => (current.includes(label) ? current : [...current, label]));
          setLabelError(describe(err));
        })
        .finally(() => setLabelBusy(false));
    },
    [query, artifactId],
  );

  const fileId = rowString(file, "id");
  const trainedDomains = useMemo(() => rowStrings(file, "trainedIntoDomainIds"), [file]);

  const train = useCallback(
    (domainId: string) => {
      const target = domainId.trim();
      if (query === null || fileId === "" || target === "") return;
      setTrainBusy(true);
      setTrainError("");
      void query
        .libraryTrainFile({ fileId, domainId: target })
        .then(() => {
          setAnnouncement(`Trained into "${target}".`);
          // NOT optimistic, unlike the label chips. The builtin refuses on
          // two independent conditions this browser cannot evaluate -- the
          // file must be the caller's, and the domain must be one the
          // cluster's authorizer vouches for -- so the only honest source
          // for "which domains is this trained into" is the row afterwards.
          setEpoch((n) => n + 1);
        })
        .catch((err: unknown) => setTrainError(describe(err)))
        .finally(() => setTrainBusy(false));
    },
    [query, fileId],
  );

  const archive = useCallback(() => {
    if (query === null || artifactId === "") return;
    setArchiveBusy(true);
    setArchiveError("");
    void query
      .archiveArtifact({ artifactId })
      .then(() => {
        setAnnouncement("Archived.");
        setEpoch((n) => n + 1);
      })
      .catch((err: unknown) => setArchiveError(describe(err)))
      .finally(() => setArchiveBusy(false));
  }, [query, artifactId]);

  return {
    artifact,
    loading,
    error,
    labels,
    labelBusy,
    labelError,
    announcement,
    addLabel,
    removeLabel,
    file,
    fileId,
    trainedDomains,
    knownDomains,
    trainBusy,
    trainError,
    train,
    archived: isArchived(artifact),
    archiveBusy,
    archiveError,
    archive,
  };
}

// isFileArtifact says whether an index row has bytes behind it. Exported so
// the pages agree on the one comparison rather than each spelling the kind.
export function isFileArtifact(row: Row | null): boolean {
  return rowString(row, "kind") === FILE_KIND;
}

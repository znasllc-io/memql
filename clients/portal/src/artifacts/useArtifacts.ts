import { useCallback, useEffect, useMemo, useState } from "react";
import { newShortId, rowArray, type Row } from "@znasllc-io/memql-sdk-core/client";

import { omitBlank } from "../cluster/args";
import { useCluster } from "../cluster/ClusterProvider";

// The artifacts surface's two hooks: useArtifacts for the LIST screen,
// useArtifactDetail for the DETAIL screen's row + label editor. One file
// because the brief scopes exactly one hooks module for this feature
// (contrast sites/, which splits useSites / useSiteDetail / useSiteHistory
// across three -- this surface has less state to carry, so one file states
// the same split without three modules).
//
// BOTH READ THROUGH NAMED QUERIES, DELIBERATELY, NOT useConceptRows /
// browseConceptPage (the generic concept browse sites/ and the rest of the
// portal reach for). v1:library:artifact declares no @rowAuthz tier
// (dsl/library/concepts.memql has none for `artifact`, unlike
// generatedOutput / documentVersion / memory in the same file, each of which
// carries @rowAuthz(owner="ownerUserId")) -- so the engine's automatic
// per-row filter injection (component/memql/rowauthz_enforce.go) has nothing
// to inject for this concept, and it is one of the ~170 constructs the
// per-row-authz audit calls "undeclared": not gated, not measured. Every
// artifact read here goes through a NAMED query instead
// (dsl/library/queries.memql: libraryArtifacts / libraryArtifactsByLabel /
// libraryArtifactById), each hand-carrying `ownerUserId==actor.userId` as an
// explicit top-level conjunct. Reading this concept through the generic
// browse -- which is what useConceptRows(ARTIFACT_CONCEPT_ID) would do --
// would show every user's artifacts to every user.
//
// Consequence: no live CDC band, unlike sites/. subscribeGraph's server-side
// delivery is not documented as owner-scoped either, and wiring a live
// subscription for an undeclared-tier concept risks the same leak through a
// different door. Writes instead bump an epoch and re-run the named reads --
// the same shape useCampaigns.ts uses for exactly this reason.

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

// -------------------- the LIST screen --------------------

export interface CreateArtifactInput {
  title: string;
  summary: string;
  body: string;
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
}

export function useArtifacts(label: string): ArtifactsState {
  const { query } = useCluster();
  const [allRows, setAllRows] = useState<Row[]>([]);
  const [filteredRows, setFilteredRows] = useState<Row[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState("");
  const [createMessage, setCreateMessage] = useState("");

  useEffect(() => {
    if (query === null) {
      setAllRows([]);
      setFilteredRows([]);
      setLoading(false);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    // The unfiltered read always runs (it is what backs the label facet);
    // the narrower read joins it only when a filter is active. One settle
    // for both, same reasoning useCampaigns.ts gives for its three-way
    // Promise.all: the alternative is two staggered arrivals fighting over
    // which one the loading skeleton is honest about.
    void Promise.all([
      query.libraryArtifacts({}),
      label === "" ? null : query.libraryArtifactsByLabel({ label }),
    ])
      .then(([full, narrowed]) => {
        if (!live) return;
        setAllRows(full.rows());
        setFilteredRows(narrowed ? narrowed.rows() : full.rows());
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
  }, [query, label, epoch]);

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
          // None of the four provenance values names "typed directly into
          // the portal's create form" -- the enum was written for
          // workbench / computer-use / agent producers
          // (dsl/library/mutations.memql). "derived" is the least-wrong of
          // the four: it is the one value that does not falsely attribute
          // the output to a pipeline that did not produce it.
          source: "derived",
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
}

export function useArtifactDetail(artifactId: string): ArtifactDetailState {
  const { query } = useCluster();
  const [artifact, setArtifact] = useState<Row | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [labels, setLabels] = useState<string[]>([]);
  const [labelBusy, setLabelBusy] = useState(false);
  const [labelError, setLabelError] = useState("");
  const [announcement, setAnnouncement] = useState("");

  useEffect(() => {
    if (query === null || artifactId === "") {
      setArtifact(null);
      setLoading(false);
      setError("");
      setLabels([]);
      return;
    }
    let live = true;
    setLoading(true);
    setError("");

    void query
      .libraryArtifactById({ artifactId })
      .then((result) => {
        if (!live) return;
        const row = result.rows()[0] ?? null;
        setArtifact(row);
        setLabels(rowLabels(row));
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
  }, [query, artifactId]);

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
  };
}

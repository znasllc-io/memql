import { useMemo, useState } from "react";

import { Chip, Head, LiveList, Notice, Refine, Row, useLiveView, type LiveListSource } from "../../kit";
import type { Row as SdkRow } from "@znasllc-io/memql-sdk-core/client";
import { ProvenanceMark } from "./Provenance";
import {
  compositionFingerprint,
  compositionFromRow,
  modelsOf,
  sourcesOf,
  type CompositionRow,
} from "./rows";
import { MATERIALIZED_EMPTY, formatWord, statusTone, statusWord } from "./words";

// MaterializedSection -- everything materialized in this instance, with
// its authoring and origin metadata (the epic's own words).
//
// ===========================================================================
// ONE LiveList OVER ONE RETAINED FEED
// ===========================================================================
// The feed is the app root's; this renders it. A second `useCompositions()`
// here would open a second subscription and run a second seed over the
// same concept, free to disagree with the first about what the cluster
// holds -- the Deployables map-and-list failure.
//
// ===========================================================================
// THE ARCHIVED SPLIT IS A CLIENT-SIDE FOLD, AND THE FILTER RE-BASELINES
// ===========================================================================
// `compositions` carries no archive conjunct, so one paged seed holds the
// complete truth. Flipping the preference reveals rows the browser ALREADY
// HAD, which is not the cluster sending them -- so the view re-baselines
// on the filter through `useLiveView`'s key, and the arrival cue does not
// fire for every newly-visible row.
//
// ===========================================================================
// WHAT THE ROW SAYS, AND WHAT IT DOES NOT
// ===========================================================================
// A row carries the name, the state, the format and the provenance mark.
// It does NOT carry the template or the model names: those are facts you
// open a composition to read, and repeating them on a row somebody is
// scanning would be the surface saying it twice (rule 7).

export interface MaterializedSectionProps {
  source: LiveListSource<SdkRow> | null;
  showArchived: boolean;
  selectedId: string;
  onSelect: (id: string) => void;
  /** Set when a read or write refused; rendered in surface, never as a toast. */
  error: string;
}

export function MaterializedSection({
  source,
  showArchived,
  selectedId,
  onSelect,
  error,
}: MaterializedSectionProps) {
  const [search, setSearch] = useState("");
  const [format, setFormat] = useState("");

  // THE VIEW KEY IS WHERE A RE-BASELINE IS EXPRESSED. Revealing rows the
  // browser already had is not news, so a filter change rebuilds the view
  // rather than animating every row that just became visible.
  const viewKey = `${showArchived ? "all" : "live"}|${format}|${search.trim().toLowerCase()}`;
  const view = useLiveView<SdkRow>(source, viewKey, (rows) =>
    rows.filter((raw) => {
      const c = compositionFromRow(raw);
      if (!showArchived && c.archived) return false;
      if (format !== "" && c.format !== format) return false;
      const q = search.trim().toLowerCase();
      if (q !== "" && !c.name.toLowerCase().includes(q) && !c.statement.toLowerCase().includes(q)) {
        return false;
      }
      return true;
    }),
  );

  const chips = useMemo(() => {
    const out: { id: string; label: string; onRemove: () => void }[] = [];
    if (format !== "") {
      out.push({ id: "format", label: formatWord(format), onRemove: () => setFormat("") });
    }
    return out;
  }, [format]);

  return (
    <div className="os-mz-list-section">
      <Head title="Materialized" meta={<CountMeta view={view} showArchived={showArchived} />}>
        <Refine search={search} onSearch={setSearch} label="Refine what is listed" chips={chips}>
          <label className="os-mz-refine-format">
            <span>Kind of file</span>
            <select value={format} onChange={(e) => setFormat(e.target.value)}>
              <option value="">Any</option>
              {["markdown", "html", "txt", "csv", "json", "docx", "pdf"].map((f) => (
                <option key={f} value={f}>
                  {formatWord(f)}
                </option>
              ))}
            </select>
          </label>
        </Refine>
      </Head>

      {error ? <Notice tone="error" sentence={error} /> : null}

      <LiveList<SdkRow>
        source={view}
        label="Everything materialized"
        emptyText={
          showArchived
            ? MATERIALIZED_EMPTY
            : // THE EMPTY STATE POINTS AT THE SETTING when hiding is why it
              // is empty (rule 4's second half). Without it, somebody who
              // archived everything sees an invitation to start over.
              `${MATERIALIZED_EMPTY} Archived ones are hidden — Settings lists them.`
        }
        rowId={(raw) => compositionFromRow(raw).id}
        fingerprint={(raw) => compositionFingerprint(compositionFromRow(raw))}
        renderRow={(raw) => {
          const c = compositionFromRow(raw);
          return (
            <CompositionListRow
              composition={c}
              sources={sourcesOf(raw)}
              models={modelsOf(raw)}
              current={c.id === selectedId}
              onOpen={() => onSelect(c.id)}
            />
          );
        }}
      />
    </div>
  );
}

function CountMeta({
  view,
  showArchived,
}: {
  view: ReturnType<typeof useLiveView<SdkRow>>;
  showArchived: boolean;
}) {
  const n = view?.snapshot.rows.length ?? 0;
  // The scope note says WHAT is being counted, so a number that looks low
  // has its explanation beside it rather than in somebody's head.
  return (
    <>
      {n} {n === 1 ? "composition" : "compositions"}
      {showArchived ? ", archived included" : ""}
    </>
  );
}

function CompositionListRow({
  composition,
  sources,
  models,
  current,
  onOpen,
}: {
  composition: CompositionRow;
  sources: ReturnType<typeof sourcesOf>;
  models: ReturnType<typeof modelsOf>;
  current: boolean;
  onOpen: () => void;
}) {
  return (
    <Row
      name={composition.name || "Untitled"}
      current={current}
      dim={composition.archived}
      onOpen={onOpen}
      state={
        <>
          <Chip tone={statusTone(composition.status)}>{statusWord(composition.status)}</Chip>
          <Chip tone="neutral">
            {composition.deployableKind ? "Package" : formatWord(composition.format)}
          </Chip>
        </>
      }
    >
      {/* THE QUIET MIDDLE IS THE PROVENANCE, for the reason the Bin's is
          where a file came from: it is the fact that decides whether this
          row is the one you are looking for. A composition made from four
          invoices by one model is a different thing from one somebody
          replayed from a recipe with no model at all, and the mark says
          which at a glance. */}
      <ProvenanceMark sources={sources} models={models} />
    </Row>
  );
}

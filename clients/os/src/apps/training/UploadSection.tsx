import { useCallback, useMemo, useRef, useState, type DragEvent } from "react";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";
import { FileUp, ScrollText, Upload } from "lucide-react";

import { Button, Caption, Chip, Head, Notice, ProvenanceDot, Row as ListRow, Select } from "../../kit";
import { formatBytes, formatFreshness } from "../../kit/format";
import { useNow } from "../../kit/useNow";
import { LiveList } from "../../live/LiveList";
import type { LiveView } from "../../live/liveView";
import { MAX_UPLOAD_BYTES, UPLOAD_ACCEPT_ATTRIBUTE, type FileStage } from "./concepts";
import { fileDotTone, stageOf, type AnalysisRun, type TrainingFile } from "./rows";
import type { DomainsFeed } from "./useDomains";
import type { TrainAct } from "./useTrain";
import type { UploadsState } from "./useUploads";

// Teach MemQL from a file.
//
// ===========================================================================
// A WORKLIST, NOT A LOG
// ===========================================================================
// The surface this replaced was a dropzone above a list of "analyses" -- a
// history of things that had happened, with nothing to do about any of them.
// It could not have offered anything: the space attachment route it used
// produced a summary and no knowledge chunks at all, so the app named
// "Training" never trained.
//
// Keyed to the Library, every file is a small pipeline with exactly one legal
// act at each stage, so the list is a WORKLIST: each row says where its file
// is and offers the act that is legal from there, and nothing else. That is
// the shell's rule 12 ("acts follow the state, an act that is not legal is
// ABSENT rather than disabled") applied per row rather than per page, because
// here the state is per row.
//
// The six stages and their acts:
//
//   uploading    bytes in flight, with real progress    Cancel
//   reading      the cluster is reading it              --
//   unreadable   stored, and there is no text in it     --
//   untrained    read, and teaching nothing yet         Teach a domain
//   trained      teaching one or more domains           Teach another
//   failed       the cluster's own sentence             Try again
//
// `unreadable` deliberately offers NOTHING. A photograph is a perfectly good
// thing to keep in the Library and there is no act that would make it teach
// something, so a disabled "Teach" beside it would be a control whose only
// purpose is to be refused.

export function UploadSection({
  uploads,
  files,
  snapshot,
  runsByFileId,
  domains,
  train,
  onReseed,
  onAsk,
}: {
  uploads: UploadsState;
  files: LiveView<TrainingFile> | null;
  snapshot: LiveSnapshot<TrainingFile>;
  runsByFileId: Map<string, AnalysisRun>;
  domains: DomainsFeed;
  train: TrainAct;
  onReseed: () => void;
  onAsk?: (tag: string) => void;
}) {
  const now = useNow(30_000);
  const [over, setOver] = useState(false);
  const picker = useRef<HTMLInputElement | null>(null);

  const accept = useCallback(
    (chosen: FileList | null) => {
      if (chosen === null || chosen.length === 0) return;
      uploads.start([...chosen]);
      onAsk?.("training:upload");
    },
    [onAsk, uploads],
  );

  // THE DROPZONE CONSUMES THE DROP, ALWAYS, AND THAT IS NOT A DETAIL.
  //
  // A WindowFrame renders INSIDE the desk plate, and the desk plate is itself
  // a file drop target: `Desktop.tsx`'s `onHostDrop` turns a dropped file into
  // a Library artifact and a desk icon. So a drop here that merely called
  // `preventDefault` would bubble, and ONE file would be uploaded TWICE --
  // once by this surface and once as a desktop item nobody asked for.
  //
  // Both handlers therefore stop propagation. (They now reach the SAME route,
  // which makes the double upload a duplicate rather than a split-brain, and
  // that is still a bug: it would file the same bytes twice under two
  // artifacts and read them twice.)
  const onDragOver = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    event.stopPropagation();
    setOver(true);
  };

  const onDrop = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    event.stopPropagation();
    setOver(false);
    accept(event.dataTransfer?.files ?? null);
  };

  return (
    <div className="os-app-stack">
      <Head
        title="Teach from a file"
        meta={snapshot.rows.length > 0 ? `${snapshot.rows.length} recent` : undefined}
      >
        <Button
          tone="primary"
          onClick={() => picker.current?.click()}
          ariaLabel="Choose files to teach from"
        >
          Choose files
        </Button>
      </Head>

      <div
        className="os-train-drop"
        data-over={over || undefined}
        onDragOver={onDragOver}
        onDragLeave={() => setOver(false)}
        onDrop={onDrop}
      >
        <FileUp size={22} aria-hidden />
        <p className="os-train-drop-line">Drop a document here.</p>
        <input
          ref={picker}
          className="os-sr-only"
          type="file"
          multiple
          accept={UPLOAD_ACCEPT_ATTRIBUTE}
          onChange={(e) => {
            accept(e.target.files);
            // Cleared so picking the SAME file twice fires a second change.
            e.target.value = "";
          }}
        />
        {/* WHAT HAPPENS TO IT, before anybody drops one. A file lands in your
            Library, the cluster reads it, and then you choose what it should
            teach -- three sentences because they are three separate things,
            and the third one is the act this surface exists for. */}
        <Caption>
          It lands in your Library, the cluster reads it, and then you choose which domain it
          teaches.
        </Caption>
        <Caption>
          Documents, spreadsheets, text and images, up to{" "}
          {Math.round(MAX_UPLOAD_BYTES / (1024 * 1024 * 1024))} GB. Anything over 32 MB uploads in
          pieces and resumes if you drop it again.
        </Caption>
      </div>

      {uploads.entries.length > 0 ? (
        <ul className="os-train-uploads" aria-label="Files being uploaded">
          {uploads.entries.map((entry) => (
            <li key={entry.key} className="os-train-upload" data-phase={entry.phase}>
              <span className="os-train-upload-head">
                <Upload size={14} aria-hidden />
                <span className="os-row-name">{entry.name}</span>
                <Chip tone="muted">{formatBytes(entry.size)}</Chip>
                <span className="os-row-state">
                  {entry.phase === "uploading" ? (
                    <>
                      <span className="os-caption">{sendingLine(entry.sentBytes, entry.size)}</span>
                      <Button onClick={() => uploads.cancel(entry.key)}>Cancel</Button>
                    </>
                  ) : null}
                  {entry.phase === "landed" ? (
                    <span className="os-caption">in your Library</span>
                  ) : null}
                  {entry.phase === "failed" ? (
                    <>
                      <Button tone="primary" onClick={() => uploads.retry(entry.key)}>
                        Try again
                      </Button>
                      <Button onClick={() => uploads.dismiss(entry.key)}>Dismiss</Button>
                    </>
                  ) : null}
                </span>
              </span>
              {/* The bar is the one piece of motion on this surface, and it
                  answers a question somebody is actively asking: is this
                  4 GB file moving? It is absent for a landed or failed
                  entry, where there is nothing left to watch. */}
              {entry.phase === "uploading" && entry.size > 0 ? (
                <span
                  className="os-train-progress"
                  role="progressbar"
                  aria-label={`Uploading ${entry.name}`}
                  aria-valuemin={0}
                  aria-valuemax={entry.size}
                  aria-valuenow={entry.sentBytes}
                >
                  <span
                    className="os-train-progress-fill"
                    style={{ width: `${percent(entry.sentBytes, entry.size)}%` }}
                  />
                </span>
              ) : null}
              {entry.phase === "uploading" && entry.resumedChunks > 0 ? (
                <Caption>
                  {entry.resumedChunks} {entry.resumedChunks === 1 ? "piece" : "pieces"} were
                  already in the cluster, so only the rest is being sent.
                </Caption>
              ) : null}
              {/* THE SERVER'S OWN SENTENCE, in place, beside the file it is
                  about. Never a toast: somebody who looked away would have
                  lost the only account of what happened. */}
              {entry.phase === "failed" ? (
                <p className="os-notice-detail os-mono">{entry.error}</p>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}

      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return your files."
          next="Files you upload still arrive -- this is the list of them that could not be read."
        >
          <Button onClick={onReseed}>Try again</Button>
        </Notice>
      ) : null}

      <div className="os-train-files">
        <LiveList<TrainingFile>
        source={files}
        rowId={(file) => file.id}
        fingerprint={(file) => fileLineFingerprint(file, runsByFileId.get(file.id))}
        label="Your recent files"
        emptyText="Nothing here yet. Drop a file above and it appears as the cluster reads it."
        renderRow={(file, tick) => (
          <FileLine
            file={file}
            run={runsByFileId.get(file.id)}
            tick={tick}
            now={now}
            domains={domains}
            train={train}
          />
        )}
        />
      </div>

      {/* The scope note, and only over content. An empty list has no "most
          recent" to be showing part of, and the empty state above already
          says what to do -- a second sentence under it would be answering a
          question nobody has yet. */}
      {snapshot.rows.length > 0 ? (
        <Caption>Your most recent files. Older ones are in the Files app.</Caption>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------

function FileLine({
  file,
  run,
  tick,
  now,
  domains,
  train,
}: {
  file: TrainingFile;
  run: AnalysisRun | undefined;
  tick: "added" | "updated" | null;
  now: Date;
  domains: DomainsFeed;
  train: TrainAct;
}) {
  const stage = stageOf(file, run);
  const [picking, setPicking] = useState(false);
  const [domainId, setDomainId] = useState("");
  const refusal = train.refusals[file.id] ?? "";
  const busy = train.busyFileId === file.id;

  // Only domains this cluster actually holds, named. A picker offering an id
  // is a picker nobody can use, and the domain feed is already loaded at the
  // app root for the queue beside this one.
  // TWO SOURCES, MERGED, because either one alone leaves somebody unable to
  // teach a domain that plainly exists.
  //
  // The CATALOG (`knowledgeDomainsAll`) is where a domain's name lives. But
  // `createKnowledgeDomain` is declared in no `.memql` file in this tree, so
  // an engine-only cluster has chunks in domains and no catalog rows for
  // them -- and a picker built from the catalog alone would be EMPTY on
  // exactly the cluster this app is for.
  //
  // The ROLLUPS are the other half: they are derived from chunks that
  // actually exist, so every id in them is a domain something is already
  // filed under. An id with no catalog row renders AS ITS ID, which is what
  // the cards beside it have always done -- an unresolvable lookup is the id,
  // never blank.
  const options = useMemo(() => {
    const byId = new Map<string, string>();
    for (const rollup of domains.rollups) {
      if (rollup.domainId !== "") byId.set(rollup.domainId, rollup.domainId);
    }
    for (const meta of domains.domains.values()) {
      if (meta.id !== "") byId.set(meta.id, meta.name || meta.id);
    }
    const named = [...byId.entries()].map(([id, name]) => ({ id, name }));
    // Sorted by NAME, not by chunk count: a domain that gains a chunk must
    // not jump the list under somebody choosing from it, and the alphabet is
    // the one ordering that is stable against the data.
    named.sort((a, b) => a.name.localeCompare(b.name));
    return named;
  }, [domains.domains, domains.rollups]);

  // A domain this cluster no longer names still renders, as its id. An
  // unresolvable lookup is the id, never blank -- the view-kit rule, and the
  // honest one: the file WAS taught to something.
  const taught = file.trainedIntoDomainIds
    .map((id) => domains.domains.get(id)?.name || id)
    .join(", ");

  async function teach() {
    const accepted = await train.train(file.id, domainId);
    if (accepted) {
      setPicking(false);
      setDomainId("");
    }
  }

  return (
    <ListRow
      icon={<ScrollText size={16} aria-hidden />}
      name={file.name || file.id}
      current={stage === "reading"}
      dim={stage === "unreadable"}
      state={
        <>
          {/* The dot and the word say one thing, so only one of them says it
              to a screen reader: the dot is aria-hidden and the word is right
              there. */}
          <span className="os-deploy-status" data-tone={toneFor(stage)}>
            <ProvenanceDot tone={fileDotTone(stage)} />
            {stageWord(stage)}
          </span>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
          {/* THE ACT, and only when it is legal. */}
          {(stage === "untrained" || stage === "trained") && !picking ? (
            <Button onClick={() => setPicking(true)}>
              {stage === "trained" ? "Teach another" : "Teach a domain"}
            </Button>
          ) : null}
        </>
      }
    >
      {/* A SPAN carrying the caption class, not <Caption>: these sit inside
          the row, and a <p> here is invalid markup React warns about. */}
      <span className="os-caption">
        {formatBytes(file.size)}
        {passageLine(run)}
        {" · "}
        {formatFreshness(file.createdAt, now)}
      </span>

      {stage === "trained" && taught !== "" ? (
        <span className="os-caption">Teaching {taught}</span>
      ) : null}

      {stage === "unreadable" ? (
        <span className="os-caption">Stored and downloadable; there is no text in it to read.</span>
      ) : null}

      {stage === "failed" ? (
        <>
          <span className="os-train-plan-error">
            {file.failureReason || run?.errorMessage || "The cluster did not say why."}
          </span>
          <span className="os-caption" data-line>
            Drop it again to retry.
          </span>
        </>
      ) : null}

      {/* AN EMPTY PICKER IS A DEAD END, so it says what to do instead rather
          than offering a control with nothing in it. This is the honest state
          on a cluster whose knowledge domains have never been seeded, and the
          Domains section is where somebody would go to look. */}
      {picking && options.length === 0 ? (
        <span className="os-caption">
          This cluster has no knowledge domains yet, so there is nothing to teach into. The Domains
          section lists what it has.
        </span>
      ) : null}

      {picking && options.length > 0 ? (
        <span className="os-train-teach">
          <Select
            id={`teach-${file.id}`}
            label={`Knowledge domain to teach from ${file.name || "this file"}`}
            value={domainId}
            onChange={setDomainId}
          >
            <option value="">Choose a domain</option>
            {options.map((option) => (
              <option key={option.id} value={option.id}>
                {option.name}
              </option>
            ))}
          </Select>
          <Button tone="primary" disabled={busy || domainId === ""} onClick={() => void teach()}>
            {busy ? "Teaching..." : "Teach"}
          </Button>
          <Button onClick={() => setPicking(false)}>Cancel</Button>
        </span>
      ) : null}

      {/* The refusal belongs on the row that produced it. */}
      {refusal !== "" ? <span className="os-train-plan-error">{refusal}</span> : null}

      {domains.error !== "" && picking ? (
        <span className="os-caption">
          The domain list could not be read, so this picker may be incomplete.
        </span>
      ) : null}
    </ListRow>
  );
}

// ---------------------------------------------------------------------------
// Wording
// ---------------------------------------------------------------------------

/** What the row says it is doing, in a person's words rather than a status
 *  enum's. "reading" is the whole pipeline: extracting, summarising and
 *  indexing are stages of one act nobody asked to see separately. */
function stageWord(stage: FileStage): string {
  switch (stage) {
    case "uploading":
      return "sending";
    case "reading":
      return "reading";
    case "unreadable":
      return "nothing to read";
    case "untrained":
      return "ready to teach";
    case "trained":
      return "teaching";
    case "failed":
      return "could not be read";
  }
}

function toneFor(stage: FileStage): "ok" | "warn" | "muted" {
  if (stage === "failed") return "warn";
  if (stage === "reading") return "ok";
  return "muted";
}

/**
 * The passage count, which is the run's to say and nobody else's.
 *
 * ABSENT UNTIL THE RUN FINISHES, and absent for a file with no run at all.
 * Rendering "0 passages" while a file is being read would be a number that is
 * true only in the sense that nothing has happened yet.
 */
function passageLine(run: AnalysisRun | undefined): string {
  if (run === undefined || run.status !== "succeeded" || !run.readable) return "";
  const passages = `${run.passages} ${run.passages === 1 ? "passage" : "passages"}`;
  // Partly-embedded is worth saying: the file is readable and only some of it
  // is searchable, and `embeddingStatus` on the file row says "partial"
  // without saying how partial.
  if (run.embedded < run.passages) {
    return ` · ${passages}, ${run.embedded} searchable`;
  }
  return ` · ${passages}`;
}

function sendingLine(sent: number, total: number): string {
  if (total <= 0) return "sending";
  return `${percent(sent, total)}%`;
}

function percent(sent: number, total: number): number {
  if (total <= 0) return 0;
  return Math.min(100, Math.max(0, Math.round((sent / total) * 100)));
}

/**
 * What a person would call a change on a file LINE.
 *
 * The file's own fingerprint plus the run's outcome, because this row renders
 * both -- a run finishing changes the passage count and the stage word while
 * the file row is untouched, and a line that did not announce it would settle
 * silently under somebody watching it.
 */
function fileLineFingerprint(file: TrainingFile, run: AnalysisRun | undefined): string {
  const runPart = run === undefined ? "" : `${run.status}|${run.passages}|${run.embedded}`;
  return [
    file.status,
    file.embeddingStatus,
    file.failureReason,
    file.trainedIntoDomainIds.join(","),
    runPart,
  ].join("|");
}

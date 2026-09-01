import { useCallback, useRef, useState, type DragEvent } from "react";
import type { LiveSnapshot } from "@znasllc-io/memql-sdk-core/client";
import { FileUp, ScrollText, Upload } from "lucide-react";

import { Button, Caption, Chip, Head, LiveList, Notice, ProvenanceDot, Row as ListRow } from "../../kit";
import { formatFreshness } from "../../kit/format";
import { useNow } from "../../kit/useNow";
import type { LiveView } from "../../live/liveView";
import { MAX_ATTACHMENT_BYTES, UPLOAD_ACCEPT_ATTRIBUTE } from "./concepts";
import {
  planDotTone,
  planFileName,
  planFingerprint,
  planIsTerminal,
  type AnalysisPlan,
} from "./rows";
import type { ActiveSpace } from "./useActiveSpace";
import type { UploadsState } from "./useUploads";

// Drop a file, watch the analysis run.
//
// The section is two halves that deliberately do not merge (see
// `useUploads.ts`): the dropzone owns the bytes-reached-the-cluster question,
// which this browser can answer from an HTTP response, and the list below owns
// everything after it, which lives in the cluster and arrives on the plan feed
// with the arrival cue.

export function UploadSection({
  space,
  uploads,
  source,
  snapshot,
  onReseed,
  onAsk,
}: {
  space: ActiveSpace;
  uploads: UploadsState;
  source: LiveView<AnalysisPlan> | null;
  snapshot: LiveSnapshot<AnalysisPlan>;
  onReseed: () => void;
  onAsk?: (tag: string) => void;
}) {
  const now = useNow(30_000);
  const [over, setOver] = useState(false);
  const picker = useRef<HTMLInputElement | null>(null);
  const ready = space.state === "ready";

  const accept = useCallback(
    (files: FileList | null) => {
      if (!ready || files === null || files.length === 0) return;
      uploads.start([...files], space.spaceId);
      onAsk?.("training:upload");
    },
    [onAsk, ready, space.spaceId, uploads],
  );

  // THE DROPZONE CONSUMES THE DROP, ALWAYS, AND THAT IS NOT A DETAIL.
  //
  // A WindowFrame renders INSIDE the desk plate, and the desk plate is itself
  // a file drop target: `Desktop.tsx`'s `onHostDrop` turns a dropped file into
  // a Library artifact and a desk icon. So a drop here that merely called
  // `preventDefault` would bubble, and ONE file would be uploaded TWICE, to
  // two different places -- once into the caller's space for analysis, and
  // once into the Library as a desktop item nobody asked for.
  //
  // Both handlers therefore stop propagation, and both run even when the
  // dropzone is DISABLED. Letting the desk have the drop in that case would
  // mean dropping a file on a visibly-disabled control produced a desktop
  // icon, which is a stranger answer than nothing happening -- and "nothing
  // happens where nothing is offered" is the shell's own rule.
  const onDragOver = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    event.stopPropagation();
    if (ready) setOver(true);
  };

  const onDrop = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    event.stopPropagation();
    setOver(false);
    accept(event.dataTransfer?.files ?? null);
  };

  return (
    <div className="os-app-stack">
      <Head title="Upload to train" />

      {/* WHERE THE FILE GOES, stated before anybody drops one. An upload here
          lands in the caller's own daily space, which is a fact about how the
          analyzer is reached rather than a choice this app is making for them. */}
      {space.state === "resolving" ? <Caption>Finding your space...</Caption> : null}

      {space.state === "absent" ? (
        <Notice
          tone="warn"
          sentence="This account has no active space yet, so there is nowhere to put a file."
          next="A space is provisioned on sign-in. Signing out and back in usually resolves it."
        >
          <Button onClick={space.reload}>Look again</Button>
        </Notice>
      ) : null}

      {space.state === "error" ? (
        <Notice
          tone="error"
          sentence="The cluster did not say which space is yours."
          next="Nothing was uploaded."
          detail={space.error}
        >
          <Button onClick={space.reload}>Try again</Button>
        </Notice>
      ) : null}

      {/* The dropzone. It is a real button as well as a drop target: a surface
          reachable only by dragging is unreachable to anybody who cannot drag. */}
      <div
        className="os-train-drop"
        data-over={over || undefined}
        data-disabled={!ready || undefined}
        onDragOver={onDragOver}
        onDragLeave={() => setOver(false)}
        onDrop={onDrop}
      >
        <FileUp size={22} aria-hidden />
        <p className="os-train-drop-line">Drop a document here to teach MemQL from it.</p>
        <Button
          tone="primary"
          disabled={!ready}
          onClick={() => picker.current?.click()}
          ariaLabel="Choose files to upload for training"
        >
          Choose files
        </Button>
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
        <Caption>
          PDFs, Word and Excel documents, CSV and TSV, JSON, plain text, Markdown, HTML and
          images, up to {Math.round(MAX_ATTACHMENT_BYTES / (1024 * 1024))} MB each. The cluster
          decides -- anything it will not read comes back with its own reason.
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
                      <span className="os-caption">sending</span>
                      <Button onClick={() => uploads.cancel(entry.key)}>Cancel</Button>
                    </>
                  ) : null}
                  {entry.phase === "landed" ? (
                    <>
                      <span className="os-caption">in the cluster</span>
                      <Button onClick={() => uploads.dismiss(entry.key)}>Dismiss</Button>
                    </>
                  ) : null}
                  {entry.phase === "failed" ? (
                    <>
                      <Button tone="primary" onClick={() => uploads.retry(entry.key)}>
                        Retry
                      </Button>
                      <Button onClick={() => uploads.dismiss(entry.key)}>Dismiss</Button>
                    </>
                  ) : null}
                </span>
              </span>
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

      <h4 className="os-subhead">Recent analyses</h4>

      {snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return your analyses."
          next="Files you upload are still analyzed -- this is the list of them that could not be read."
        >
          <Button onClick={onReseed}>Try again</Button>
        </Notice>
      ) : null}

      <LiveList<AnalysisPlan>
        source={source}
        rowId={(plan) => plan.id}
        fingerprint={planFingerprint}
        label="Analyses you have started"
        emptyText="No analyses yet. Drop a file above and the work appears here as it runs."
        renderRow={(plan, tick) => <PlanLine plan={plan} tick={tick} now={now} />}
      />

      {/* The list is one page of the caller's newest plans, and says so rather
          than implying it is every analysis they have ever run. */}
      <Caption>Your most recent analyses. Older ones are not loaded.</Caption>
    </div>
  );
}

function PlanLine({
  plan,
  tick,
  now,
}: {
  plan: AnalysisPlan;
  tick: "added" | "updated" | null;
  now: Date;
}) {
  const terminal = planIsTerminal(plan);
  return (
    <ListRow
      icon={<ScrollText size={16} aria-hidden />}
      name={planFileName(plan)}
      current={plan.status === "running"}
      dim={plan.status === "cancelled"}
      state={
        <>
          {/* The dot and the word say one thing, so only one of them says it
              to a screen reader: the dot is aria-hidden and the word is right
              there. */}
          <span className="os-deploy-status" data-tone={toneWord(plan.status)}>
            <ProvenanceDot tone={planDotTone(plan)} />
            {plan.status || "unknown"}
          </span>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {/* A SPAN carrying the caption class, not <Caption>: this sits inside
          the row's own <button>, and a <p> there is invalid markup React
          warns about at render time. */}
      <span className="os-caption">
        {terminal && plan.completedAt !== ""
          ? formatFreshness(plan.completedAt, now)
          : formatFreshness(plan.createdAt, now)}
      </span>
      {/* A FAILURE'S REASON IS ON THE ROW, not behind a click. It is the one
          thing somebody needs from this list, and the plan carries it. */}
      {plan.status === "failed" && plan.errorMessage !== "" ? (
        <span className="os-train-plan-error os-mono">{plan.errorMessage}</span>
      ) : null}
    </ListRow>
  );
}

function toneWord(status: string): "ok" | "warn" | "muted" {
  if (status === "failed" || status === "cancelled") return "warn";
  if (status === "running") return "ok";
  return "muted";
}

/** Bytes as somebody would say them. Two decimals never help at this size. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "--";
  if (bytes < 1024) return `${bytes} B`;
  const kb = bytes / 1024;
  if (kb < 1024) return `${Math.round(kb)} KB`;
  return `${(kb / 1024).toFixed(1)} MB`;
}

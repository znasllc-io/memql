import { useCallback, useEffect, useState } from "react";
import { rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";
import { FolderOpen, RotateCcw } from "lucide-react";

import { useAuthSource } from "../../auth/context";
import {
  Button,
  Chip,
  Chips,
  Fact,
  Facts,
  Notice,
  ProvenanceDot,
  formatBytes,
  formatMoment,
} from "../../kit";
import { useOsConnection } from "../../live/connection";
import { OVER_LIMIT_SENTENCE, planDownload, runBufferedDownload } from "../files/actions/download";
import { downloadWorkerRegistration, runWorkerDownload } from "../files/actions/downloadWorker";
import { kindGlyph } from "../files/glyphs";
import { fileStory } from "../files/rows";
import type { ArtifactRow } from "../files/rows";
import { restoreNote } from "./restore";
import type { BinItem } from "./rows";

// The Bin's detail panel -- and the reason this app is not a list of names
// with a date beside each one.
//
// ===========================================================================
// THE STORY LEADS, BECAUSE THE STORY IS WHAT DECIDES
// ===========================================================================
// Every trash can shows you a name and when it went in. Neither answers the
// question somebody standing in one is actually asking, which is whether they
// still want it -- and for a file that came off a client's laptop in March,
// the fact that decides it is the laptop, not the date. Where something came
// from is the thing this platform can say that a folder of bytes cannot, so it
// is the header here, exactly as it is in the Files inspector.
//
// THE MACHINE BLOCK IS READ ON DEMAND. The absolute path a file occupied, its
// size and its link state all live on v1:library:file and deliberately never
// reach the index (the path is not promoted at all, by design). So selecting
// an item reads one file row -- which is what the Files inspector already does
// for size, and what keeps this window from subscribing to the analysis churn
// of the entire Library to render one panel.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

interface FileFacts {
  size: number;
  name: string;
  uploadedFromPath: string;
  uploadedFromWorkerName: string;
  linkState: string;
  linkCheckedAt: string;
  /** "" while the read is in flight or nothing was asked for. */
  error: string;
  loaded: boolean;
}

const NO_FILE_FACTS: FileFacts = {
  size: 0,
  name: "",
  uploadedFromPath: "",
  uploadedFromWorkerName: "",
  linkState: "",
  linkCheckedAt: "",
  error: "",
  loaded: false,
};

/** What a link state MEANS, in the reader's terms rather than the enum's.
 *  The tone is deliberately not "error" for origin_gone: the copy is fine,
 *  which is the entire point of keeping it. */
const LINK_SENTENCE: Record<string, string> = {
  synced: "Matched the file on that machine when it was last checked",
  stale: "That machine has a newer copy that has not arrived here",
  origin_gone: "No longer at that path on that machine -- this copy is all there is",
};

export function BinDetail({
  item,
  artifact,
  folderNameOf,
  presence,
  onRestore,
  restoring,
  restoreError,
  onClose,
}: {
  item: BinItem;
  /** The full index row, for the kinds that have one. Null for a folder. */
  artifact: ArtifactRow | null;
  folderNameOf: (folderId: string) => string;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  onRestore: () => void;
  restoring: boolean;
  restoreError: string;
  onClose: () => void;
}) {
  const connection = useOsConnection();
  const authSource = useAuthSource();
  const [facts, setFacts] = useState<FileFacts>(NO_FILE_FACTS);
  const [downloadError, setDownloadError] = useState("");
  const [downloadBusy, setDownloadBusy] = useState(false);

  const fileId = item.fileId;
  useEffect(() => {
    setFacts(NO_FILE_FACTS);
    setDownloadError("");
    if (fileId === "" || connection === null) return;
    let live = true;
    void (async () => {
      try {
        const result = await connection.query.libraryFileById({ fileId });
        const row = result.rows()[0] ?? null;
        if (!live) return;
        if (row === null) {
          // The index row is here and its backing file is not readable. That
          // is a real answer, not a blank: it says the bytes are gone from
          // this caller's reach while the entry survives.
          setFacts({ ...NO_FILE_FACTS, loaded: true, error: "This file's record could not be read." });
          return;
        }
        setFacts({
          size: rowNumber(row, "size"),
          name: rowString(row, "name"),
          uploadedFromPath: rowString(row, "uploadedFromPath"),
          uploadedFromWorkerName: rowString(row, "uploadedFromWorkerName"),
          linkState: rowString(row, "linkState"),
          linkCheckedAt: rowString(row, "linkCheckedAt"),
          error: "",
          loaded: true,
        });
      } catch (err: unknown) {
        if (live) setFacts({ ...NO_FILE_FACTS, loaded: true, error: describe(err) });
      }
    })();
    return () => {
      live = false;
    };
  }, [fileId, connection]);

  const download = useCallback(async () => {
    setDownloadBusy(true);
    setDownloadError("");
    try {
      const registration = await downloadWorkerRegistration();
      const plan = planDownload({ workerAvailable: registration !== null, sizeBytes: facts.size });
      if (plan.path === "refused") {
        setDownloadError(OVER_LIMIT_SENTENCE);
        return;
      }
      const fileName = facts.name || item.name;
      if (plan.path === "worker" && registration !== null) {
        await runWorkerDownload({
          artifactId: item.id,
          fileName,
          sizeBytes: facts.size,
          bearer: () => authSource.bearer(),
          registration,
        });
      } else {
        await runBufferedDownload({ artifactId: item.id, fileName, bearer: () => authSource.bearer() });
      }
    } catch (err: unknown) {
      setDownloadError(describe(err));
    } finally {
      setDownloadBusy(false);
    }
  }, [facts.size, facts.name, item.id, item.name, authSource]);

  const machine = item.producedByWorkerId ? presence(item.producedByWorkerId) : null;
  const story = artifact === null ? null : fileStory(artifact, machine);
  const machineName =
    facts.uploadedFromWorkerName || item.producedByWorkerName || machine?.name || "";

  return (
    <aside className="os-bin-detail" aria-label="Archived item details">
      <header className="os-files-inspector-head">
        <span className="os-files-inspector-glyph">
          {item.kind === "folder" ? <FolderOpen size={18} aria-hidden /> : kindGlyph(item.contentKind, 18)}
        </span>
        <h3 className="os-files-inspector-name" title={item.name}>
          {item.name}
        </h3>
        <Button onClick={onClose} ariaLabel="Close details">
          Close
        </Button>
      </header>

      {/* THE STORY LEADS -- but exactly ONCE. Where a machine is named the
          block below says everything this line would and more (the path, the
          link state, the presence dot), so the line stands down rather than
          announcing the same machine twice in eighty pixels. A folder has no
          story to tell at all: it is an organizational row somebody made here,
          which it says rather than borrowing a sentence about bytes it never
          had. */}
      {story === null ? (
        <p className="os-bin-story">
          <span>A folder you made here. It came to the Bin with what was in it.</span>
        </p>
      ) : machineName === "" ? (
        <p className="os-bin-story">
          <ProvenanceDot tone={story.tone} />
          <span>{story.sentence}</span>
        </p>
      ) : null}

      <Facts>
        {item.kind === "artifact" ? <Fact label="Kind" value={item.contentKind} mono /> : null}
        <Fact label="Was filed in" value={folderNameOf(item.folderId)} />
        {artifact !== null ? (
          // "other" is what every unclassified type resolves to, and it names
          // nothing -- a .mov and a .zip are both "other". The mime type is
          // the answer in that case, and the enum in every other.
          <Fact
            label="Format"
            value={artifact.format === "other" || artifact.format === "" ? artifact.mimeType : artifact.format}
            mono
          />
        ) : null}
        {facts.loaded && facts.size > 0 ? <Fact label="Size" value={formatBytes(facts.size)} /> : null}
        {item.producedByPlanId !== "" ? (
          <Fact label="Plan" value={item.producedByPlanId} mono />
        ) : null}
        {item.changedAt !== "" ? (
          <Fact
            label="Last changed"
            value={formatMoment(item.changedAt)}
            title="An archive re-writes the row, so for most items in the Bin this is when it was archived."
          />
        ) : null}
        <Fact label="Id" value={item.id} mono />
      </Facts>

      {/* WHERE IT CAME FROM, when a machine is named. This is the block the
          issue asked for by name, and it is the reason the panel reads one
          file row: the absolute path lives there and is deliberately never
          promoted to the index. */}
      {machineName !== "" ? (
        <section className="os-bin-machine" aria-label="Where this came from">
          <p className="os-bin-machine-head">
            <ProvenanceDot tone={machine === null ? "unknown" : machine.online ? "reachable" : "unreachable"} />
            <span className="os-bin-machine-name">{machineName}</span>
            {machine === null ? null : (
              <span className="os-caption">{machine.online ? "online" : "offline"}</span>
            )}
          </p>
          {facts.uploadedFromPath !== "" ? (
            <p className="os-bin-machine-path os-mono" title={facts.uploadedFromPath}>
              {facts.uploadedFromPath}
            </p>
          ) : null}
          {facts.linkState !== "" ? (
            <p className="os-caption">
              {LINK_SENTENCE[facts.linkState] ?? facts.linkState}
              {facts.linkCheckedAt !== "" ? ` -- checked ${formatMoment(facts.linkCheckedAt)}` : ""}
            </p>
          ) : null}
        </section>
      ) : null}

      {facts.error !== "" ? (
        <Notice tone="warn" sentence="Some of this item's details could not be read." detail={facts.error} />
      ) : null}

      {item.labels.length > 0 ? (
        <Chips label="Labels">
          {item.labels.map((label) => (
            <Chip key={label}>{label}</Chip>
          ))}
        </Chips>
      ) : null}

      <div className="os-bin-actions">
        <Button tone="primary" onClick={onRestore} busy={restoring} busyLabel="Restoring">
          <RotateCcw size={13} aria-hidden /> Restore
        </Button>
        {item.kind === "artifact" ? (
          <Button onClick={() => void download()} busy={downloadBusy} busyLabel="Downloading">
            Download
          </Button>
        ) : null}
      </div>
      <p className="os-caption">{restoreNote(item)}</p>

      {restoreError !== "" ? (
        <Notice
          tone="error"
          sentence="The restore was refused."
          next="It is still in the Bin, with everything it had."
          detail={restoreError}
        />
      ) : null}
      {downloadError !== "" ? (
        <Notice tone="error" sentence="The download did not land." detail={downloadError}>
          <Button onClick={() => void download()}>Try again</Button>
        </Notice>
      ) : null}
    </aside>
  );
}

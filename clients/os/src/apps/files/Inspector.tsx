import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Trash2, CornerUpRight, Download, RotateCcw } from "lucide-react";

import { useAuthSource } from "../../auth/context";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { canOpen } from "../../system/registry";
import { openInVsCode, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { editorFilename, isZipArtifact } from "../../items/editorPreference";
import { binItemFromArtifact } from "../bin/rows";
import { planRestore, runRestore } from "../bin/restore";
import { Button, CopyValue, Fact, Facts, Notice, Head, Subhead, formatBytes, formatMoment } from "../../kit";
import { ActionBar } from "../../kit/ActionBar";
import { LabelEditor } from "./LabelEditor";
import { AccountLabelPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { useArtifactAccounts } from "./actions/accounts";
import { useFileVersions } from "./actions/versions";
import { VersionHistory } from "./VersionHistory";
import { fileHeadFromRow, type VersionEntry } from "./versions";
import { useOsConnection } from "../../live/connection";
import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import type { Breadcrumb } from "../../kit/Breadcrumbs";
import { MATERIALIZER_APP, MATERIALIZER_COMPOSER } from "./materializer";
import {
  downloadArtifact,
  OVER_LIMIT_SENTENCE,
  planDownload,
  runBufferedDownload,
} from "./actions/download";
import { downloadWorkerRegistration, runWorkerDownload } from "./actions/downloadWorker";
import { artifactName, fileStory, type ArtifactRow, type CompositionRow } from "./rows";

// File details occupy the page. Origin belongs with the other facts;
// labels, clients and version history scroll above the pinned action footer.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function Inspector({
  row,
  files,
  composition,
  folderNameOf,
  archivedFolderIds,
  presence,
  confirmBeforeArchive,
  onClose,
  backLabel = "Files",
  breadcrumbs,
}: {
  row: ArtifactRow;
  files: readonly Row[];
  /**
   * The composition that produced this file, if one did (epic memql#4981,
   * #4983). ONE SENTENCE AND ONE ACT is the whole of what Files says about
   * it: the record -- the sources, the template, the models that contributed,
   * the provenance -- belongs to the Materializer, and restating any of it
   * here would be a second reading free to disagree with the app whose
   * subject it is.
   */
  composition: CompositionRow | null;
  folderNameOf: (folderId: string) => string;
  /** Ids of KNOWN-archived folders -- the restore re-file predicate. */
  archivedFolderIds: ReadonlySet<string>;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  confirmBeforeArchive: boolean;
  onClose: () => void;
  backLabel?: string;
  breadcrumbs?: readonly Breadcrumb[];
}) {
  const { config } = useSession();
  const { actions, registry } = useOs();
  const connection = useOsConnection();
  const authSource = useAuthSource();

  const name = artifactName(row);
  const machine = row.producedByWorkerId ? presence(row.producedByWorkerId) : null;
  const story = fileStory(row, machine, composition);

  // Every action reports beside itself, in surface. One error slot per
  // action, so a download refusal never sits under the archive button.
  const [vsNoAnswer, setVsNoAnswer] = useState(false);
  const [downloadError, setDownloadError] = useState("");
  const [downloadBusy, setDownloadBusy] = useState(false);
  const [deskNote, setDeskNote] = useState("");
  const accounts = useAccountOptions();
  const accountTie = useArtifactAccounts();
  const [archiveError, setArchiveError] = useState("");
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [confirmingArchive, setConfirmingArchive] = useState(false);
  const cancelHandoff = useRef<(() => void) | null>(null);

  // --- versions (epic memql#4806) ---
  //
  // The backing file id, taken from the artifact's own source ref. Blank for
  // every non-file kind, which reads nothing and shows no panel: a note has
  // no upload versions, and offering an empty history for one would answer a
  // question nobody asked.
  const fileId = useMemo(
    () => (row.kind === "file" ? (row.sourceConceptRef.split(":").pop() ?? "") : ""),
    [row.kind, row.sourceConceptRef],
  );
  const liveHead = files.find(file => rowString(file, "id") === fileId);
  const liveVersion = liveHead ? fileHeadFromRow(liveHead) : null;
  const headRevision = liveVersion ? `${liveVersion.versionNumber}:${liveVersion.sha256}:${liveVersion.versionUploadedAt}` : "";
  const versions = useFileVersions(fileId, headRevision);
  const [downloadingVersion, setDownloadingVersion] = useState(0);

  useEffect(() => () => cancelHandoff.current?.(), []);
  useEffect(() => {
    if (!vsNoAnswer) return;
    const t = setTimeout(() => setVsNoAnswer(false), 8000);
    return () => clearTimeout(t);
  }, [vsNoAnswer]);

  const openVsCode = useCallback(() => {
    cancelHandoff.current?.();
    setVsNoAnswer(false);
    cancelHandoff.current = openInVsCode(config.domain, row.id, () => setVsNoAnswer(true), undefined, editorFilename(row));
  }, [config.domain, row.id, row.title]);

  const sendToDesk = useCallback(() => {
    const outcome = actions.sendFileToDesk({
      artifactId: row.id,
      title: name,
      fileKind: row.kind,
      source: row.source,
      ...(row.producedByWorkerId ? { producedByWorkerId: row.producedByWorkerId } : {}),
    });
    setDeskNote(
      outcome === "full"
        ? "The desk is full -- remove something from it first."
        : outcome === "focused"
          ? "Already on the desk; it is selected there now."
          : "On the desk.",
    );
  }, [actions, row, name]);

  useEffect(() => {
    if (deskNote === "") return;
    const t = setTimeout(() => setDeskNote(""), 5000);
    return () => clearTimeout(t);
  }, [deskNote]);

  const download = useCallback(async () => {
    setDownloadBusy(true);
    setDownloadError("");
    try {
      await downloadArtifact({
        artifactId: row.id,
        name,
        fileId: row.kind === "file" ? (row.sourceConceptRef.split(":").pop() ?? "") : "",
        readFile: connection
          ? async (id) => {
              const result = await connection.query.libraryFileById({ fileId: id });
              const fileRow = result.rows()[0] ?? null;
              if (!fileRow) return null;
              return { sizeBytes: rowNumber(fileRow, "size"), name: rowString(fileRow, "name") };
            }
          : null,
        bearer: () => authSource.bearer(),
      });
    } catch (err: unknown) {
      setDownloadError(describe(err));
    } finally {
      setDownloadBusy(false);
    }
  }, [row, name, connection, authSource]);

  const downloadVersion = useCallback(
    async (entry: VersionEntry) => {
      setDownloadingVersion(entry.versionNumber);
      setDownloadError("");
      try {
        const registration = await downloadWorkerRegistration();
        const plan = planDownload({ workerAvailable: registration !== null, sizeBytes: entry.size });
        if (plan.path === "refused") {
          setDownloadError(OVER_LIMIT_SENTENCE);
          return;
        }
        // The version rides the SAME two runners the current version does --
        // worker when one exists, buffered otherwise -- so the over-limit
        // sentence, the refusal wording and the save behaviour cannot fork
        // between "this file" and "an older copy of this file".
        const version = entry.current ? undefined : entry.versionNumber;
        if (plan.path === "worker" && registration !== null) {
          await runWorkerDownload({
            artifactId: row.id,
            fileName: entry.name || name,
            sizeBytes: entry.size,
            bearer: () => authSource.bearer(),
            registration,
            ...(version === undefined ? {} : { version }),
          });
        } else {
          await runBufferedDownload({
            artifactId: row.id,
            fileName: entry.name || name,
            bearer: () => authSource.bearer(),
            ...(version === undefined ? {} : { version }),
          });
        }
      } catch (err: unknown) {
        setDownloadError(describe(err));
      } finally {
        setDownloadingVersion(0);
      }
    },
    [row.id, name, authSource],
  );

  const archive = useCallback(async () => {
    const query = connection?.query ?? null;
    if (query === null) {
      setArchiveError("Not connected to the cluster, so nothing was archived.");
      return;
    }
    setConfirmingArchive(false);
    setArchiveBusy(true);
    setArchiveError("");
    try {
      await query.archiveArtifact({ artifactId: row.id });
    } catch (err: unknown) {
      setArchiveError(describe(err));
    } finally {
      setArchiveBusy(false);
    }
  }, [connection, row.id]);

  // Restore, for a row being read in the Bin place (epic memql#4842,
  // #4846): the Bin's client-driven pair, verbatim -- the index first, then
  // the backing file -- so the two surfaces cannot drift apart on what
  // "putting back" means. The automation mirror deliberately does not exist
  // (apps/bin/restore.ts says why), which is why this is two writes.
  //
  // ONE addition over the Bin's flow (#4846 AC): a file whose folder is
  // KNOWN-ARCHIVED re-files to the Library root, because a row restored into
  // an invisible folder is invisible everywhere except search. Fail-closed on
  // archived-list membership -- an absence test against the live tree would
  // re-file out of live folders while the feed is still seeding.
  const restore = useCallback(async () => {
    const query = connection?.query ?? null;
    if (query === null) {
      setArchiveError("Not connected to the cluster, so nothing was restored.");
      return;
    }
    setArchiveBusy(true);
    setArchiveError("");
    try {
      await runRestore(planRestore(binItemFromArtifact(row)), {
        restoreArtifact: async (artifactId) => {
          await query.restoreArtifact({ artifactId });
        },
        restoreFile: async (fileId) => {
          await query.restoreLibraryFile({ fileId });
        },
        restoreFolder: async (folderId) => {
          await query.restoreLibraryFolder({ folderId });
        },
      });
      if (row.folderId !== "" && archivedFolderIds.has(row.folderId)) {
        await query.moveArtifactToFolder({ artifactId: row.id, folderId: "" });
        setDeskNote("Restored to the Library root -- its folder is still archived.");
      }
    } catch (err: unknown) {
      setArchiveError(describe(err));
    } finally {
      setArchiveBusy(false);
    }
  }, [connection, row, archivedFolderIds]);

  // Download is offered only where bytes or a body exist: a file always, the
  // rendered kinds by construction of the content route.
  const filedIn = folderNameOf(row.folderId);

  return (
    <section className="os-action-pane os-file-detail" aria-label="File details" data-os-page-context={JSON.stringify({ page: "File details", fileId: row.id, name, kind: row.kind })}>
      <div className="os-action-body os-file-detail-body">
      <Head title={name} breadcrumbs={breadcrumbs} back={{label: backLabel, onSelect: onClose}} />

      {/* A summary describes the contents; origin is a fact below. */}
      {row.summary.trim() !== "" ? <p className="os-files-summary">{row.summary}</p> : null}

      <div className="os-files-group">
        <Subhead>Details</Subhead>
        <Facts>
          <Fact label="Origin" value={story.sentence} />
          <Fact label="Kind" value={row.kind.replaceAll("_", " ")} />
          <Fact label="Filed in" value={filedIn} />
          <Fact label="Format" value={row.format || row.mimeType} mono />
          {row.kind === "document" ? (
            <Fact label="Validation" value={row.validationStatus} />
          ) : null}
          <Fact label="Created" value={formatMoment(row.createdAt)} />
          {/* THE TWO OPAQUE VALUES COME LAST, AND THEY ARE COPYABLE. Neither
              is readable at a glance and neither is retypeable, so each stays
              on one line and hands the whole string over on a click. Putting
              them below the human facts also keeps the two longest values out
              of the way of the four somebody actually reads. */}
          {row.producedByRunId !== "" ? (
            <Fact
              label="Run"
              value={<CopyValue value={row.producedByRunId} label="Run" />}
              mono
            />
          ) : null}
          <Fact label="Id" value={<CopyValue value={row.id} label="Id" />} mono />
        </Facts>

        {/* MADE IN THE MATERIALIZER. A fact and an act, and deliberately not a
            panel: what this file was made FROM is the record's question, and
            the record is one click away in the app that owns it. The act is
            ABSENT rather than disabled when that app is not open-able
            (DESIGN.md rule 12) -- `openApp` no-ops on an app the registry
            does not hold, and a button that silently does nothing is worse
            than one that is not there. */}
        {composition !== null ? (
          <Facts>
            <Fact
              label="Made in"
              value={
                canOpen(registry, MATERIALIZER_APP) ? (
                  <button
                    type="button"
                    className="os-link"
                    onClick={() =>
                      actions.openApp(MATERIALIZER_APP, MATERIALIZER_COMPOSER, {
                        compositionId: composition.id,
                      })
                    }
                  >
                    Open in Materializer
                  </button>
                ) : (
                  "the Materializer"
                )
              }
            />
          </Facts>
        ) : null}

      </div>

      {/* LABELS (epic memql#5009). They were a read-only chip row inside
          Details; they are editable now, so they take a group of their own
          for the reason Clients does -- an editable thing on a surface that
          is otherwise a reading is not a fact row. The browse's label FACET
          asks a different question of the same field and lives behind Refine
          (DESIGN.md rule 2); this is where the value is set. */}
      <div className="os-files-group">
        <Subhead>Labels</Subhead>
        <LabelEditor artifactId={row.id} labels={row.labels} />
      </div>

      {/* WHO THIS IS FOR (epic memql#4800, D5). MULTIPLE, because the index's
          `accountIds` is a list -- a contract naming two clients is one file,
          and making it pick would be the schema disagreeing with the filing
          cabinet. Toggles rather than a multi-select: that control drops every
          selection on an unmodified click, which is the most destructive
          interaction available on a picker whose job is "one or two".

          The write is ordinary and ungated. An account is a record with no
          read effect, so labelling a file changes who it is ABOUT and nothing
          about who may read it.

          It is the one EDITABLE thing on a surface that is otherwise a
          reading, which is why it is its own group rather than a fact row. */}
      <div className="os-files-group">
        <Subhead>Clients</Subhead>
        <AccountLabelPicker
          selected={row.accountIds}
          accounts={accounts}
          label="The clients this file is about"
          disabled={accountTie.busy}
          onChange={(next) => void accountTie.setAccounts(row.id, next)}
        />
        {accountTie.error === "" ? null : (
          <Notice
            tone="error"
            sentence="The client labels were not changed."
            next="This file still carries whatever labels it had."
            detail={accountTie.error}
          />
        )}
      </div>

      {row.kind === "file" ? (
        <VersionHistory
          history={versions.history}
          headName={versions.head?.name ?? name}
          loading={versions.loading}
          error={versions.error}
          readAt={versions.readAt}
          presence={presence}
          onRefresh={versions.refresh}
          onDownload={(entry) => void downloadVersion(entry)}
          downloadingVersion={downloadingVersion}
        />
      ) : null}
      </div>
      <footer className="os-file-detail-footer" data-confirming={confirmingArchive || undefined}>
        {vsNoAnswer || downloadError || archiveError ? (
          <div className="os-file-detail-notices">
            {vsNoAnswer ? <Notice tone="warn" sentence={VSCODE_NO_ANSWER_MESSAGE} /> : null}
            {downloadError ? (
              <Notice tone="error" sentence="The download did not land." detail={downloadError}>
                <Button onClick={() => void download()} busy={downloadBusy}>Try again</Button>
              </Notice>
            ) : null}
            {archiveError ? (
              <Notice tone="error" sentence={row.archived ? "The file was not restored." : "The file was not deleted."} detail={archiveError} />
            ) : null}
          </div>
        ) : null}
        <ActionBar
          state={archiveBusy ? (row.archived ? "Restoring" : "Deleting")
            : confirmingArchive ? `Delete "${name}"?`
            : downloadBusy || downloadingVersion > 0 ? "Downloading"
            : row.archived ? "In Bin" : "In Library"}
          detail={confirmingArchive ? "You can restore it from the Bin until it is purged."
            : deskNote || (versions.head ? `Version ${versions.head.versionNumber} · ${formatBytes(versions.head.size)}` : undefined)}
          live
        >
          {confirmingArchive ? (
            <>
              <button type="button" className="os-actbar-text" onClick={() => setConfirmingArchive(false)}>Cancel</button>
              <Button tone="danger" onClick={() => void archive()}>Delete</Button>
            </>
          ) : (
            <>
              {row.archived ? (
                <Button busy={archiveBusy} busyLabel="Restoring" onClick={() => void restore()}>
                  <RotateCcw size={16} aria-hidden /> Restore
                </Button>
              ) : (
                <Button tone="danger" busy={archiveBusy} ariaLabel={archiveBusy ? "Deleting" : "Delete"} title="Delete"
                  onClick={() => (confirmBeforeArchive ? setConfirmingArchive(true) : void archive())}>
                  <Trash2 size={16} aria-hidden />
                </Button>
              )}
              <Button onClick={sendToDesk} disabled={archiveBusy} ariaLabel="Send to desktop" title="Send to desktop">
                <CornerUpRight size={16} aria-hidden />
              </Button>
              <Button onClick={() => void download()} busy={downloadBusy} disabled={archiveBusy}
                tone={isZipArtifact(row) ? "primary" : "quiet"}
                ariaLabel={downloadBusy ? "Downloading" : isZipArtifact(row) ? "Download ZIP" : "Download"} title={isZipArtifact(row) ? "Download ZIP" : "Download"}>
                <Download size={16} aria-hidden />
              </Button>
              {!isZipArtifact(row) ? <Button tone="primary" onClick={openVsCode} disabled={archiveBusy}>Open</Button> : null}
            </>
          )}
        </ActionBar>
      </footer>
    </section>
  );
}

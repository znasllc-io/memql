import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Archive, CornerUpRight, Download, FilePlus2, RotateCcw, X } from "lucide-react";

import { useAuthSource } from "../../auth/context";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { openInVsCode, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { binItemFromArtifact } from "../bin/rows";
import { planRestore, runRestore } from "../bin/restore";
import { Button, Chip, Chips, Fact, Facts, Notice, ProvenanceDot, Select, Subhead, formatBytes, formatMoment } from "../../kit";
import { AccountLabelPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { useArtifactAccounts } from "./actions/accounts";
import { useFileVersions } from "./actions/versions";
import { VersionHistory } from "./VersionHistory";
import type { VersionEntry } from "./versions";
import { useOsConnection } from "../../live/connection";
import type { UploadProvider } from "../../items/upload";
import { rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";
import { kindGlyph } from "./BrowseSection";
import { OVER_LIMIT_SENTENCE, planDownload, runBufferedDownload } from "./actions/download";
import { downloadWorkerRegistration, runWorkerDownload } from "./actions/downloadWorker";
import type { FolderTree, TreeNode } from "./fold";
import { artifactName, fileStory, type ArtifactRow } from "./rows";

// The inspector (design D1): the file's story, its facts, and the five
// actions -- open in VS Code, send to desktop, download, upload a new version,
// archive -- plus the re-filing move and the version history. THE STORY LEADS:
// where a file came from is the fact this platform can tell that a generic
// file manager cannot, so it is the header, not a row in a table.
//
// THE HISTORY SITS UNDER THE ACTION THAT GROWS IT (epic memql#4806). "Upload
// new version" is beside Download, and the stack it appends to is directly
// below -- so the refusal renders next to the control that produced it and the
// result appears where the person is already looking.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** The tree flattened for the move picker, depth as indentation. */
function folderOptions(tree: FolderTree): Array<{ id: string; label: string }> {
  const out: Array<{ id: string; label: string }> = [];
  const walk = (node: TreeNode, depth: number) => {
    out.push({ id: node.folder.id, label: `${"  ".repeat(depth)}${node.folder.name}` });
    for (const child of node.children) walk(child, depth + 1);
  };
  for (const root of tree.roots) walk(root, 0);
  return out;
}

export function Inspector({
  row,
  folderNameOf,
  tree,
  presence,
  confirmBeforeArchive,
  uploads,
  onAsk,
  onClose,
}: {
  row: ArtifactRow;
  folderNameOf: (folderId: string) => string;
  tree: FolderTree;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  confirmBeforeArchive: boolean;
  /**
   * The ONE upload provider (epic memql#4806). A new version rides it exactly
   * as every other upload does, which is what gives it chunking, resume,
   * per-chunk retry and verbatim refusals for free -- and what keeps
   * test/files/onePath.test.ts's claim true: the provider is still the only
   * thing in this tree that speaks the artifact upload wire.
   *
   * It does NOT ride `useUploadTasks`: those create a placeholder ROW in the
   * list, and a new version of an existing artifact must not add a second row
   * to the very list it is proving it does not disturb.
   */
  uploads: UploadProvider;
  onAsk: (tag: string) => void;
  onClose: () => void;
}) {
  const { config } = useSession();
  const { actions } = useOs();
  const connection = useOsConnection();
  const authSource = useAuthSource();

  const name = artifactName(row);
  const machine = row.producedByWorkerId ? presence(row.producedByWorkerId) : null;
  const story = fileStory(row, machine);

  // Every action reports beside itself, in surface. One error slot per
  // action, so a download refusal never sits under the archive button.
  const [vsNoAnswer, setVsNoAnswer] = useState(false);
  const [downloadError, setDownloadError] = useState("");
  const [downloadBusy, setDownloadBusy] = useState(false);
  const [deskNote, setDeskNote] = useState("");
  const [moveError, setMoveError] = useState("");
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
  const versions = useFileVersions(fileId);
  const refreshVersions = versions.refresh;
  const picker = useRef<HTMLInputElement | null>(null);
  const [newVersionError, setNewVersionError] = useState("");
  const [newVersionNote, setNewVersionNote] = useState("");
  const [newVersionBusy, setNewVersionBusy] = useState(false);
  const [newVersionSent, setNewVersionSent] = useState(0);
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
    cancelHandoff.current = openInVsCode(config.domain, row.id, () => setVsNoAnswer(true));
  }, [config.domain, row.id]);

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
      // The 512 MiB decision needs the SIZE, which lives on the backing file
      // row -- the index deliberately does not carry it. Non-file kinds are
      // small rendered bodies (design D13) and skip the read.
      let sizeBytes = 0;
      let fileName = name;
      if (row.kind === "file" && connection) {
        const fileId = row.sourceConceptRef.split(":").pop() ?? "";
        if (fileId !== "") {
          const result = await connection.query.libraryFileById({ fileId });
          const fileRow = result.rows()[0] ?? null;
          if (fileRow) {
            sizeBytes = rowNumber(fileRow, "size");
            fileName = rowString(fileRow, "name") || name;
          }
        }
      }
      const registration = await downloadWorkerRegistration();
      const plan = planDownload({ workerAvailable: registration !== null, sizeBytes });
      if (plan.path === "refused") {
        setDownloadError(OVER_LIMIT_SENTENCE);
        return;
      }
      if (plan.path === "worker" && registration !== null) {
        await runWorkerDownload({
          artifactId: row.id,
          fileName,
          sizeBytes,
          bearer: () => authSource.bearer(),
          registration,
        });
      } else {
        await runBufferedDownload({
          artifactId: row.id,
          fileName,
          bearer: () => authSource.bearer(),
        });
      }
    } catch (err: unknown) {
      setDownloadError(describe(err));
    } finally {
      setDownloadBusy(false);
    }
  }, [row, name, connection, authSource]);

  // UPLOAD A NEW VERSION. One file, one provider call, one target.
  //
  // The file is HELD so "Try again" can retry the same bytes: a refusal that
  // makes the person re-pick the file they just picked is a refusal that costs
  // them the thing they were doing.
  const pending = useRef<File | null>(null);
  const sendNewVersion = useCallback(
    async (file: File) => {
      pending.current = file;
      setNewVersionBusy(true);
      setNewVersionError("");
      setNewVersionNote("");
      setNewVersionSent(0);
      const handle = uploads.upload(file, { targetArtifactId: row.id });
      const stop = handle.onProgress?.((progress) => setNewVersionSent(progress.sentBytes));
      try {
        const result = await handle.done;
        // The action keeps its own name through the whole flow: the button
        // says "Upload new version", and this says which version landed.
        setNewVersionNote(
          result.versionNumber === undefined
            ? "The new version landed."
            : `Version ${result.versionNumber} landed.`,
        );
        pending.current = null;
        // Read the history again: these rows carry no broadcast rule, so this
        // is what makes the stack below show what just happened. The LIST
        // updates on its own -- the artifact index is re-stamped server-side
        // and arrives on the feed the browse already reads.
        refreshVersions();
      } catch (err: unknown) {
        setNewVersionError(describe(err));
      } finally {
        stop?.();
        setNewVersionBusy(false);
      }
    },
    // `refreshVersions`, not the whole `versions` object: the hook returns a
    // fresh object every render, so depending on it would make this
    // useCallback a no-op that re-allocates on every keystroke elsewhere in
    // the panel. The refresh function itself is stable.
    [uploads, row.id, refreshVersions],
  );

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

  const moveTo = useCallback(
    async (folderId: string) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setMoveError("Not connected to the cluster, so nothing moved.");
        return;
      }
      setMoveError("");
      try {
        await query.moveArtifactToFolder({ artifactId: row.id, folderId });
        // Nothing is patched locally: the update broadcasts and the list,
        // the rail counts and this panel all move together on the same feed.
      } catch (err: unknown) {
        setMoveError(describe(err));
      }
    },
    [connection, row.id],
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

  // Restore, for a row being read in the Archive place (epic memql#4842,
  // #4846): the Bin's client-driven pair, verbatim -- the index first, then
  // the backing file -- so the two surfaces cannot drift apart on what
  // "putting back" means. The automation mirror deliberately does not exist
  // (apps/bin/restore.ts says why), which is why this is two writes.
  //
  // ONE addition over the Bin's flow (#4846 AC): a file whose folder is not
  // live -- archived, or gone -- re-files to the Library root, because a row
  // restored into an invisible folder is invisible everywhere except search.
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
      if (row.folderId !== "" && !tree.byId.has(row.folderId)) {
        await query.moveArtifactToFolder({ artifactId: row.id, folderId: "" });
        setDeskNote("Restored to the Library root -- its folder is still archived.");
      }
    } catch (err: unknown) {
      setArchiveError(describe(err));
    } finally {
      setArchiveBusy(false);
    }
  }, [connection, row, tree]);

  // Download is offered only where bytes or a body exist: a file always, the
  // rendered kinds by construction of the content route.
  const filedIn = folderNameOf(row.folderId);
  const options = folderOptions(tree);

  return (
    <aside className="os-files-inspector" aria-label="File details">
      <header className="os-files-inspector-head">
        <span className="os-files-inspector-glyph">{kindGlyph(row.kind, 18)}</span>
        <h3 className="os-files-inspector-name" title={name}>
          {name}
        </h3>
        <Button onClick={onClose} ariaLabel="Close details">
          <X size={14} aria-hidden />
        </Button>
      </header>

      {/* The provenance story -- the one sentence this platform can say that
          a folder of bytes cannot. The dot is the machine's presence where a
          machine is named, and absent where nothing is known. */}
      <p className="os-files-story">
        <ProvenanceDot tone={story.tone} />
        <span>{story.sentence}</span>
      </p>
      {row.archived ? <Chip tone="muted">archived</Chip> : null}

      <Facts>
        <Fact label="Kind" value={row.kind} mono />
        <Fact label="Filed in" value={filedIn} />
        <Fact label="Format" value={row.format || row.mimeType} mono />
        {row.kind === "document" ? (
          <Fact label="Validation" value={row.validationStatus} />
        ) : null}
        {row.producedByPlanId !== "" ? (
          <Fact label="Plan" value={row.producedByPlanId} mono />
        ) : null}
        <Fact label="Created" value={formatMoment(row.createdAt)} />
        <Fact label="Id" value={row.id} mono />
      </Facts>

      {row.summary.trim() !== "" ? <p className="os-files-summary">{row.summary}</p> : null}

      {row.labels.length > 0 ? (
        <Chips label="Labels">
          {row.labels.map((label) => (
            <Chip key={label}>{label}</Chip>
          ))}
        </Chips>
      ) : null}

      {/* WHO THIS IS FOR (epic memql#4800, D5). MULTIPLE, because the index's
          `accountIds` is a list -- a contract naming two clients is one file,
          and making it pick would be the schema disagreeing with the filing
          cabinet. Toggles rather than a multi-select: that control drops every
          selection on an unmodified click, which is the most destructive
          interaction available on a picker whose job is "one or two".

          The write is ordinary and ungated. An account is a record with no
          read effect, so labelling a file changes who it is ABOUT and nothing
          about who may read it. */}
      <div className="os-files-accounts">
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

      <div className="os-files-actions">
        <Button tone="primary" onClick={openVsCode}>
          Open in VS Code
        </Button>
        <Button onClick={sendToDesk}>
          <CornerUpRight size={13} aria-hidden /> Send to desktop
        </Button>
        <Button onClick={() => void download()} busy={downloadBusy} busyLabel="Downloading">
          <Download size={13} aria-hidden /> Download
        </Button>
        {/* NEW VERSION, beside Download and only for files. The person NAMES
            the file this replaces by acting from its own inspector, which is
            the whole identity story: a browser upload carries no honest
            machine or path identity, and matching by filename would silently
            merge two different files. */}
        {row.kind === "file" && !row.archived ? (
          <Button
            onClick={() => picker.current?.click()}
            busy={newVersionBusy}
            busyLabel="Uploading"
          >
            <FilePlus2 size={13} aria-hidden /> Upload new version
          </Button>
        ) : null}
        {!row.archived ? (
          <Button
            tone="danger"
            busy={archiveBusy}
            busyLabel="Archiving"
            onClick={() => (confirmBeforeArchive ? setConfirmingArchive(true) : void archive())}
          >
            <Archive size={13} aria-hidden /> Archive
          </Button>
        ) : (
          <Button
            tone="primary"
            busy={archiveBusy}
            busyLabel="Restoring"
            onClick={() => void restore()}
          >
            <RotateCcw size={13} aria-hidden /> Restore
          </Button>
        )}
      </div>
      {/* The picker itself is never seen: a bare file input cannot be styled
          into this shell's button language, and every surface here uses the
          same one. */}
      <input
        ref={picker}
        type="file"
        className="os-visually-hidden"
        aria-label="Choose a file to upload as the new version"
        onChange={(event) => {
          const file = event.target.files?.[0];
          // The input is cleared so picking the SAME file twice fires again --
          // a change event that never comes reads as a dead button.
          event.target.value = "";
          if (file) void sendNewVersion(file);
        }}
      />
      {newVersionBusy && newVersionSent > 0 ? (
        <p className="os-caption os-files-version-progress">{formatBytes(newVersionSent)} sent</p>
      ) : null}
      {newVersionNote !== "" ? <p className="os-caption">{newVersionNote}</p> : null}
      {newVersionError !== "" ? (
        <Notice
          tone="error"
          sentence="The new version was not accepted."
          next="This file still holds the version it had."
          detail={newVersionError}
        >
          {pending.current === null ? null : (
            <Button
              onClick={() => {
                const file = pending.current;
                if (file) void sendNewVersion(file);
              }}
            >
              Try again
            </Button>
          )}
        </Notice>
      ) : null}
      {vsNoAnswer ? <Notice tone="warn" sentence={VSCODE_NO_ANSWER_MESSAGE} /> : null}
      {deskNote !== "" ? <p className="os-caption">{deskNote}</p> : null}
      {downloadError !== "" ? (
        <Notice tone="error" sentence="The download did not land." detail={downloadError}>
          <Button onClick={() => void download()}>Try again</Button>
        </Notice>
      ) : null}
      {confirmingArchive ? (
        <Notice
          tone="warn"
          sentence={`Archive "${name}"?`}
          next="Archiving hides it from the default list; the bytes stay, and the archived filter brings it back."
        >
          <div className="os-files-confirm">
            <Button tone="danger" onClick={() => void archive()}>
              Archive
            </Button>
            <Button onClick={() => setConfirmingArchive(false)}>Cancel</Button>
          </div>
        </Notice>
      ) : null}
      {archiveError !== "" ? (
        <Notice tone="error" sentence="The archive was refused." detail={archiveError} />
      ) : null}

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

      <div className="os-files-move">
        <p className="os-caption">Move to folder</p>
        <Select
          id={`files-move-${row.id}`}
          label="Move to folder"
          value={row.folderId}
          onChange={(folderId) => void moveTo(folderId)}
        >
          <option value="">Library (top level)</option>
          {options.map((option) => (
            <option key={option.id} value={option.id}>
              {option.label}
            </option>
          ))}
        </Select>
      </div>
      {moveError !== "" ? (
        <Notice tone="error" sentence="The move was refused." detail={moveError} />
      ) : null}

      <Button onClick={() => onAsk(`app:files/browse file:${name}`)}>Ask about this file</Button>
    </aside>
  );
}

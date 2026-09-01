import { useCallback, useEffect, useRef, useState } from "react";
import { Archive, CornerUpRight, Download, X } from "lucide-react";

import { useAuthSource } from "../../auth/context";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { openInVsCode, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { Button, Chip, Chips, Fact, Facts, Notice, ProvenanceDot, Select, Subhead, formatMoment } from "../../kit";
import { AccountLabelPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { useArtifactAccounts } from "./actions/accounts";
import { useOsConnection } from "../../live/connection";
import { rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";
import { kindGlyph } from "./BrowseSection";
import { OVER_LIMIT_SENTENCE, planDownload, runBufferedDownload } from "./actions/download";
import { downloadWorkerRegistration, runWorkerDownload } from "./actions/downloadWorker";
import type { FolderTree, TreeNode } from "./fold";
import { artifactName, fileStory, type ArtifactRow } from "./rows";

// The inspector (design D1): the file's story, its facts, and the four
// actions -- open in VS Code, send to desktop, download, archive -- plus the
// re-filing move. THE STORY LEADS: where a file came from is the fact this
// platform can tell that a generic file manager cannot, so it is the header,
// not a row in a table.

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
  onAsk,
  onClose,
}: {
  row: ArtifactRow;
  folderNameOf: (folderId: string) => string;
  tree: FolderTree;
  presence: (workerId: string) => { name?: string; online: boolean } | null;
  confirmBeforeArchive: boolean;
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
        {!row.archived ? (
          <Button
            tone="danger"
            busy={archiveBusy}
            busyLabel="Archiving"
            onClick={() => (confirmBeforeArchive ? setConfirmingArchive(true) : void archive())}
          >
            <Archive size={13} aria-hidden /> Archive
          </Button>
        ) : null}
      </div>
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

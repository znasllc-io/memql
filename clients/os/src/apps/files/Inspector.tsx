import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Archive, CornerUpRight, Download, FilePlus2, RotateCcw, Sparkles, X } from "lucide-react";

import { useAuthSource } from "../../auth/context";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { canOpen } from "../../system/registry";
import { openInVsCode, VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { binItemFromArtifact } from "../bin/rows";
import { planRestore, runRestore } from "../bin/restore";
import { Button, Chip, CopyValue, Fact, Facts, Notice, ProvenanceDot, Subhead, formatBytes, formatMoment } from "../../kit";
import { LabelEditor } from "./LabelEditor";
import { AccountLabelPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { useArtifactAccounts } from "./actions/accounts";
import { useFileVersions } from "./actions/versions";
import { VersionHistory } from "./VersionHistory";
import type { VersionEntry } from "./versions";
import { useOsConnection } from "../../live/connection";
import type { UploadProvider } from "../../items/upload";
import { rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";
import { kindGlyph } from "./glyphs";
import { MATERIALIZER_APP, MATERIALIZER_COMPOSER } from "./materializer";
import {
  downloadArtifact,
  OVER_LIMIT_SENTENCE,
  planDownload,
  runBufferedDownload,
} from "./actions/download";
import { downloadWorkerRegistration, runWorkerDownload } from "./actions/downloadWorker";
import { artifactName, fileStory, type ArtifactRow, type CompositionRow } from "./rows";

// The inspector (design D1): the file's story, its facts, and the five
// actions -- open in VS Code, send to desktop, download, upload a new version,
// archive -- plus the version history. THE STORY LEADS: where a file came from
// is the fact this platform can tell that a generic file manager cannot, so it
// is the header, not a row in a table.
//
// THE HISTORY SITS UNDER THE ACTION THAT GROWS IT (epic memql#4806). "Upload
// new version" is beside Download, and the stack it appends to is directly
// below -- so the refusal renders next to the control that produced it and the
// result appears where the person is already looking.
//
// ===========================================================================
// THE PANEL IS FOUR GROUPS, AND THE TWO OPAQUE VALUES ARE COPYABLE
// ===========================================================================
// It was a flat stack of ten things and it scrolled SIDEWAYS. An artifact id
// is one unbreakable word, and the facts grid's `1fr` column refused to shrink
// below it, so the panel grew past its own container. The fix is at the cause
// (`minmax(0, 1fr)` in the stylesheet); an id nobody can select out of a
// truncated line is only half an answer, so `Id` and `Plan` render as
// `CopyValue` -- ellipsized, with the whole value on `title` and in the
// clipboard. The short human facts get no button: one beside "Created" is
// furniture.
//
// After the lead, the rest groups under Subheads (DESIGN.md rule 8): the
// file's own details, the clients it is about, the actions, the versions. Two
// rules the grouping had to respect.
//
//   - SAY IT ONCE (rule 7). The kind was a glyph in the header AND a `Kind`
//     row in the facts. The glyph stays -- it is the same mark the list row
//     carries, so the panel that opens is visibly the thing that was clicked
//     -- and it is NAMED now (`role="img"`), which is what keeps the kind in
//     the reading for somebody who never sees a glyph. The fact row goes.
//   - EVERY NOTICE STAYS WITH ITS ACTION. One error slot per action was a
//     recorded decision before this pass, so the whole block moved into the
//     Actions group in its existing order and nothing was consolidated. A
//     download refusal must never sit under the archive button.
//
// ONE PRIMARY, AND `Archive` KEEPS ITS TONE. "Open in VS Code" is the one
// thing this surface is for; "Restore" was a second primary and is quiet now.
// "Archive" stays `danger` because tone here is about CONSEQUENCE rather than
// emphasis (the Button contract says so) -- dropping it to quiet would make
// archiving look exactly like downloading.
//
// ASK IS THE SHELL'S ASK AFFORDANCE, IN THE HEADER (the deployable page's pattern),
// not a full-width button at the foot of the panel. The tag string is
// unchanged: it is a contract with the Ask surface.
//
// MOVING A FILE IS NOT HERE ANY MORE. Re-filing is something you do TO a row,
// so it lives on the row's own context menu rather than as a standing picker
// at the bottom of a reading surface.

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function Inspector({
  row,
  composition,
  folderNameOf,
  archivedFolderIds,
  presence,
  confirmBeforeArchive,
  uploads,
  onAsk,
  onClose,
}: {
  row: ArtifactRow;
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
  const { actions, registry, actorRole } = useOs();
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
    <aside className="os-files-inspector" aria-label="File details">
      <header className="os-files-inspector-head">
        {/* THE KIND IS SAID HERE AND NOWHERE ELSE (DESIGN.md rule 7). The
            glyph is the mark the list row already carries, so the panel that
            opens is visibly the thing that was clicked -- and it is NAMED,
            because a purely decorative glyph would drop the kind out of the
            reading of anybody who never sees one. */}
        <span className="os-files-inspector-glyph" role="img" aria-label={`Kind: ${row.kind}`}>
          {kindGlyph(row.kind, 18)}
        </span>
        <h3 className="os-files-inspector-name" title={name}>
          {name}
        </h3>
        {/* Ask, the shell's own affordance, where every other detail surface
            puts it. The tag is unchanged: the Ask surface reads it. */}
        <Button
          onClick={() => onAsk(`app:files/browse file:${name}`)}
          ariaLabel={`Ask about ${name}`}
        >
          <Sparkles size={13} aria-hidden /> Ask
        </Button>
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
      {/* The file's own words sit with the story rather than under the table:
          both are prose about this file, and what follows is the table. */}
      {row.summary.trim() !== "" ? <p className="os-files-summary">{row.summary}</p> : null}

      <div className="os-files-group">
        <Subhead>Details</Subhead>
        <Facts>
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
          {row.producedByPlanId !== "" ? (
            <Fact
              label="Plan"
              value={<CopyValue value={row.producedByPlanId} label="Plan" />}
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
                canOpen(registry, actorRole, MATERIALIZER_APP) ? (
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

      <div className="os-files-group">
        <Subhead>Actions</Subhead>
        {/* ONE PRIMARY LEADS, FULL WIDTH; THE REST PAIR UP UNDER IT. The row
            was six equal buttons wrapping raggedly at a width the panel picks
            for itself, so which action sat beside which changed with the
            label lengths. The grid below is either one column or two, and
            every cell is the same size -- predictable at any panel width. */}
        <div className="os-files-actions">
          <Button tone="primary" onClick={openVsCode}>
            Open in VS Code
          </Button>
          <div className="os-files-actions-more">
            <Button onClick={sendToDesk}>
              <CornerUpRight size={13} aria-hidden /> Send to desktop
            </Button>
            <Button onClick={() => void download()} busy={downloadBusy} busyLabel="Downloading">
              <Download size={13} aria-hidden /> Download
            </Button>
            {/* NEW VERSION, beside Download and only for files. The person
                NAMES the file this replaces by acting from its own inspector,
                which is the whole identity story: a browser upload carries no
                honest machine or path identity, and matching by filename would
                silently merge two different files. */}
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
              <Button busy={archiveBusy} busyLabel="Restoring" onClick={() => void restore()}>
                <RotateCcw size={13} aria-hidden /> Restore
              </Button>
            )}
          </div>
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
            // The input is cleared so picking the SAME file twice fires again
            // -- a change event that never comes reads as a dead button.
            event.target.value = "";
            if (file) void sendNewVersion(file);
          }}
        />
        {/* EVERY SLOT BELOW BELONGS TO THE ACTION ABOVE IT, in the order it
            has always been in. One error slot per action is a recorded
            decision: consolidating them would put a download refusal under
            the archive button. */}
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
    </aside>
  );
}

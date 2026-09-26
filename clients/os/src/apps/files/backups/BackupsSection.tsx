import { useMemo, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Head, RecordRow, listCount, LiveList, Notice, formatBytes, formatFreshness, useLiveView, useNow, type LiveListSource } from "../../../kit";
import { flatten } from "../../../kit/rows";
import { useMachines } from "../../../live/machines";
import { isWorkerOnline } from "../../fleet/online";
import { machineFromRow, machineName, isRevoked, type MachineRow } from "../../fleet/rows";
import { linkStateOf, LINK_LABEL, LINK_SENTENCE, type LinkState } from "../links";
import { folderFromRow, type FolderRow } from "../rows";
import { BackupForm } from "./BackupForm";
import {
  backupFingerprint,
  fileBelongsToBackup,
  linkToneOf,
  isOriginFault,
  ORIGIN_SENTENCE,
  TONE_LABEL,
  TONE_SENTENCE,
  worstFileState,
  type BackupRow,
  type LinkTone,
  type NewBackup,
} from "./rows";
import { CREATE_BUSY_KEY, useBackupWrites, type BackupWrites } from "./useBackups";

// Backups: the folders on this person's machines that keep arriving here.
//
// A backup uses the shared record row. Its two named ends and counts retain
// the one-way relationship: the machine's source and the Library destination.

export interface BackupsSectionProps {
  /** The Library's folder rows -- the destination names, and the picker. */
  folders: readonly Row[];
  /** The backing file rows, for the per-file origin link states. */
  files: readonly Row[];
  /** The backups feed, as a LiveList source. */
  source: LiveListSource<BackupRow> | null;
  /** Injectable so tests drive the writes without a cluster. */
  writes?: BackupWrites;
}

export function BackupsSection({ folders, files, source, writes }: BackupsSectionProps) {
  // ONE clock for the whole section (the Fleet rule): a `useNow` per row would
  // give every row its own timer and re-render them out of step.
  const now = useNow(15_000);
  const ownWrites = useBackupWrites();
  const write = writes ?? ownWrites;

  const [adding, setAdding] = useState(false);
  const [editingId, setEditingId] = useState("");
  const [confirmingId, setConfirmingId] = useState("");

  const { collection: machinesCollection } = useMachines();
  const machines = useLiveView<Row, MachineRow>(machinesCollection, "backups:machines", (rows) =>
    rows.map(machineFromRow).filter((machine) => machine.id !== "" && !isRevoked(machine)),
  );
  const machineList = machines?.snapshot.rows ?? [];

  const folderRows = useMemo(() => folders.map(folderFromRow), [folders]);
  const folderName = useMemo(() => {
    const byId = new Map<string, FolderRow>(folderRows.map((folder) => [folder.id, folder]));
    // An unresolvable destination renders as its raw id, in the data voice --
    // the view-kit rule for every lookup that does not come back. A folder the
    // caller cannot read is not an error to report; it is an id.
    return (id: string) => (id === "" ? "Library root" : (byId.get(id)?.name ?? id));
  }, [folderRows]);

  // The per-file origin states, keyed by (machine, path) so a backup's badge
  // is about ITS OWN files. See fileBelongsToBackup for why this is not a
  // folder rollup.
  const trackedFiles = useMemo(
    () =>
      files.map((raw) => {
        const row = flatten(raw);
        return {
          uploadedFromWorkerId: rowString(row, "uploadedFromWorkerId"),
          uploadedFromPath: rowString(row, "uploadedFromPath"),
          state: linkStateOf(raw),
        };
      }),
    [files],
  );

  const machineFor = useMemo(() => {
    const byId = new Map(machineList.map((machine) => [machine.id, machine]));
    return (id: string) => byId.get(id);
  }, [machineList]);

  const editing = editingId === "" ? null : (source?.snapshot.rows.find((row) => row.id === editingId) ?? null);

  return (
    <section className="os-backups" aria-label="Backups">
      <Head
        title="Backups"
        meta={listCount(source?.snapshot)}
      >
        <Button
          tone={adding ? "quiet" : "primary"}
          onClick={() => {
            setAdding((open) => !open);
            setEditingId("");
            write.clearError();
          }}
          ariaExpanded={adding}
        >
          {adding ? "Close" : "Back up a folder"}
        </Button>
      </Head>

      {adding ? (
        <BackupForm
          machines={machineList}
          folders={folderRows}
          now={now}
          busy={write.busyId === CREATE_BUSY_KEY}
          error={write.errorId === CREATE_BUSY_KEY ? write.actionError : ""}
          editing={null}
          onSubmit={async (spec: NewBackup) => {
            if (await write.create(spec)) setAdding(false);
          }}
          onCancel={() => {
            setAdding(false);
            write.clearError();
          }}
        />
      ) : null}

      {editing !== null ? (
        // KEYED ON THE ROW, so switching Edit from one backup to another
        // REMOUNTS the form. Without it React reconciles the same instance at
        // the same position, its useState initialisers do not re-run, and the
        // fields still hold the previous backup's values -- while the caption
        // above them names the new one. Saving then wrote A's destination and
        // A's exclusions onto B, silently, because this mutation is a full
        // replace of exactly those fields.
        <BackupForm
          key={editing.id}
          machines={machineList}
          folders={folderRows}
          now={now}
          busy={write.busyId === editing.id}
          error={write.errorId === editing.id ? write.actionError : ""}
          editing={editing}
          onSubmit={async (spec: NewBackup) => {
            const ok = await write.update(editing.id, {
              folderId: spec.folderId,
              excludeGlobs: spec.excludeGlobs,
              includeHidden: spec.includeHidden,
            });
            if (ok) setEditingId("");
          }}
          onCancel={() => {
            setEditingId("");
            write.clearError();
          }}
        />
      ) : null}

      <LiveList<BackupRow>
        source={source}
        rowId={(backup) => backup.id}
        fingerprint={backupFingerprint}
        label="Backups"
        emptyText="Nothing is being backed up yet. Choose a folder on one of your machines and everything inside it keeps arriving here."
        renderRow={(backup) => (
          <BackupLine
            backup={backup}
            machine={machineFor(backup.workerId)}
            destination={folderName(backup.folderId)}
            files={trackedFiles}
            now={now}
            write={write}
            editing={editingId === backup.id}
            onEdit={() => {
              setEditingId(backup.id);
              setAdding(false);
              write.clearError();
            }}
            confirming={confirmingId === backup.id}
            onConfirm={() => setConfirmingId(backup.id)}
            onDismiss={() => setConfirmingId("")}
          />
        )}
      />
    </section>
  );
}

/** The Head's quiet scope note: how many backups, across how many machines. */
export

interface BackupLineProps {
  backup: BackupRow;
  machine: MachineRow | undefined;
  destination: string;
  files: readonly { uploadedFromWorkerId: string; uploadedFromPath: string; state: LinkState | "" }[];
  now: Date;
  write: BackupWrites;
  editing: boolean;
  onEdit: () => void;
  confirming: boolean;
  onConfirm: () => void;
  onDismiss: () => void;
}

function BackupLine({
  backup,
  machine,
  destination,
  files,
  now,
  write,
  editing,
  onEdit,
  confirming,
  onConfirm,
  onDismiss,
}: BackupLineProps) {
  const mine = useMemo(() => files.filter((file) => fileBelongsToBackup(file, backup)), [files, backup]);
  const worst = useMemo(() => worstFileState(mine.map((file) => file.state)), [mine]);
  const tone = linkToneOf(backup, worst);
  const busy = write.busyId === backup.id;
  const name = machine === undefined ? backup.workerId : machineName(machine);
  const online = machine !== undefined && isWorkerOnline(machine, now);

  return (
    <article className="os-backup" data-tone={tone}>
      <RecordRow
        name={backup.localPath}
        secondary={`${name} to ${destination}`}
        state={TONE_LABEL[tone]}
        tone={tone === "settled" ? "accent" : tone === "paused" ? "muted" : "warn"}
        current={online}
        stateExtra={<span className="os-caption">{backup.lastSweepAt === "" ? "no report yet" : `checked ${formatFreshness(backup.lastSweepAt, now)}`}</span>}
        actions={<>
          <Button onClick={() => write.setStatus(backup.id, backup.status === "paused" ? "active" : "paused")} busy={busy} ariaLabel={backup.status === "paused" ? `Resume backing up ${backup.localPath}` : `Pause backing up ${backup.localPath}`}>
            {backup.status === "paused" ? "Resume" : "Pause"}
          </Button>
          <Button onClick={onEdit} ariaExpanded={editing} ariaLabel={`Edit the backup of ${backup.localPath}`}>Edit</Button>
          <Button onClick={onConfirm} ariaLabel={`Stop backing up ${backup.localPath}`}>Stop</Button>
        </>}
      >
        <span role="img" aria-label={`${name} to ${destination}: ${TONE_LABEL[tone]}. ${TONE_SENTENCE[tone]}`}>
          {backup.originState === "" ? "not counted yet" : `${backup.filesSeen.toLocaleString()} ${backup.filesSeen === 1 ? "file" : "files"} · ${formatBytes(backup.bytesSeen)}`}
          {" → "}<span>{`${mine.length.toLocaleString()} ${mine.length === 1 ? "file" : "files"} here`}</span>
        </span>
      </RecordRow>

      {/* SAY IT ONCE, and say the SPECIFIC one.
          - A settled backup's label plus "checked N ago" already says
            everything a sentence would, so healthy rows carry none. Repeating
            it under every one of them is what makes the rows that DO need
            reading hard to find.
          - Where the origin is at fault, the origin's own sentence replaces
            the tone's: it names what happened AND the repair, where the tone
            can only say that something is wrong.
          - Where a backup is PAUSED, the tone wins even over a fault, because
            the fault is the last thing seen before the pause rather than
            something happening now. */}
      {tone === "settled" ? null : (
        <Caption>
          {tone !== "paused" && isOriginFault(backup.originState)
            ? ORIGIN_SENTENCE[backup.originState]
            : TONE_SENTENCE[tone]}
        </Caption>
      )}

      {/* The machine's own words, verbatim and never paraphrased -- they name
          a path, a permission or an HTTP status somebody can act on, which is
          the part a friendlier rewording would lose. */}
      {backup.lastSweepError !== "" ? (
        <Notice tone="warn" sentence="That machine reported:" detail={backup.lastSweepError} />
      ) : null}

      {/* The per-FILE state, only when the folder itself is fine and the files
          beneath it are not -- which is the one case the folder-level tone
          cannot express. Where the tone already came FROM the file states,
          this line would be the same sentence twice in two voices. */}
      {backup.originState === "ok" && worst !== "" && worst !== "synced" && tone !== "behind" ? (
        <Caption>{`${LINK_LABEL[worst]}: ${LINK_SENTENCE[worst]}`}</Caption>
      ) : null}

      {confirming ? (
        <Notice
          tone="warn"
          sentence={`Stop backing up ${backup.localPath}?`}
          next="The files already here stay, and nothing on the machine is touched. Only the arrangement ends."
        >
          <div className="os-backup-confirm">
            <Button
              tone="danger"
              busy={busy}
              onClick={async () => {
                if (await write.stop(backup.id)) onDismiss();
              }}
            >
              Stop backing up
            </Button>
            <Button onClick={onDismiss}>Keep it</Button>
          </div>
        </Notice>
      ) : null}

      {/* A refusal belongs to the write that produced it, not to whichever
          control happens to be open. Keyed on errorId so a refused Pause on
          one row does not paint an error on the other three -- and so it says
          anything at all, which the confirm-gated first version did not. */}
      {write.errorId === backup.id && write.actionError !== "" ? (
        <Notice tone="error" sentence="That was refused." detail={write.actionError} />
      ) : null}
    </article>
  );
}

export type { LinkTone };

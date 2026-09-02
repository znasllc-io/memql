import { useId, useMemo, useState } from "react";

import { Button, Caption, Check, Field, Input, Notice, Panel, Select, Subhead } from "../../../kit";
import { isWorkerOnline } from "../../fleet/online";
import { machineName, type MachineRow } from "../../fleet/rows";
import { foldFolderTree, type TreeNode } from "../fold";
import type { FolderRow } from "../rows";
import type { BackupRow, NewBackup } from "./rows";

// The one form, in two modes.
//
// NOT A WIZARD, and there is no stepper anywhere in this shell to copy even if
// one were wanted. Setting up a backup is three decisions -- which machine,
// which folder there, where it lands -- and three decisions with no ordering
// law between them is a form, not a sequence. The Deployables stage rail earns
// its steps because a deploy has an actual law about its order; this does not.
//
// CREATE AND EDIT ARE THE SAME FORM because they are the same decisions, minus
// the two an existing backup cannot change. The machine and the path are the
// backup's IDENTITY -- the (machine, path) key every re-push is matched on --
// so editing them would orphan the machine's local ledger and start the whole
// folder again as new files. Changing where it lands is the edit people
// actually want, and it is one field.

export interface BackupFormProps {
  machines: readonly MachineRow[];
  folders: readonly FolderRow[];
  now: Date;
  busy: boolean;
  error: string;
  /** The backup being edited, or null to set a new one up. */
  editing: BackupRow | null;
  onSubmit: (spec: NewBackup) => void;
  onCancel: () => void;
}

/**
 * A placeholder path that looks like one from THAT machine.
 *
 * Read off the machine's own reported `os` rather than guessed from anything
 * in the browser: the person is typing a path that exists somewhere else, and
 * a macOS-shaped hint on a Windows machine is worse than no hint -- it is a
 * wrong answer presented as help.
 */
export function pathPlaceholderFor(machine: MachineRow | undefined): string {
  switch (machine?.os) {
    case "windows":
      return "C:\\Users\\you\\Clients";
    case "darwin":
      return "/Users/you/Clients";
    case "linux":
      return "/home/you/Clients";
    default:
      return "The full path to the folder";
  }
}

/**
 * Every folder as an indented path, in tree order, so a select can read like a
 * tree without being one.
 *
 * Archived folders are left out: filing new work into the Bin is not something
 * to offer. Deleted ones are left out for a blunter reason -- one held no file
 * anywhere, no folder read returns it, and a backup pointed at one would push
 * into a destination the person can never open. The indent is spaces rather
 * than a rendered hierarchy because a `<select>`'s options are text -- this is
 * the one place in the app where the tree has to flatten, and saying so beats a
 * component that pretends otherwise.
 */
export function folderChoices(folders: readonly FolderRow[]): { id: string; label: string }[] {
  const tree = foldFolderTree(folders.filter((folder) => !folder.archived && !folder.deleted));
  const out: { id: string; label: string }[] = [];
  const walk = (nodes: readonly TreeNode[], depth: number) => {
    for (const node of nodes) {
      out.push({ id: node.folder.id, label: `${"\u00a0\u00a0\u00a0\u00a0".repeat(depth)}${node.folder.name}` });
      walk(node.children, depth + 1);
    }
  };
  walk(tree.roots, 0);
  return out;
}

/** Comma-separated patterns to a clean list. Whitespace-only entries are
 *  dropped rather than stored: a pattern that matches nothing but reads like a
 *  rule is worse than no rule. */
export function parseGlobs(raw: string): string[] {
  return raw
    .split(",")
    .map((entry) => entry.trim())
    .filter((entry) => entry !== "");
}

export function BackupForm({
  machines,
  folders,
  now,
  busy,
  error,
  editing,
  onSubmit,
  onCancel,
}: BackupFormProps) {
  const ids = useId();
  const [workerId, setWorkerId] = useState(editing?.workerId ?? machines[0]?.id ?? "");
  const [localPath, setLocalPath] = useState(editing?.localPath ?? "");
  const [folderId, setFolderId] = useState(editing?.folderId ?? "");
  const [globs, setGlobs] = useState((editing?.excludeGlobs ?? []).join(", "));
  const [includeHidden, setIncludeHidden] = useState(editing?.includeHidden ?? false);

  const choices = useMemo(() => folderChoices(folders), [folders]);
  const chosen = useMemo(() => machines.find((machine) => machine.id === workerId), [machines, workerId]);

  const ready = workerId !== "" && localPath.trim() !== "";
  const repointing = editing !== null && folderId !== editing.folderId;

  const submit = () => {
    if (!ready || busy) return;
    onSubmit({
      workerId,
      localPath: localPath.trim(),
      folderId,
      excludeGlobs: parseGlobs(globs),
      includeHidden,
    });
  };

  return (
    <Panel label={editing === null ? "Back up a folder" : "Edit this backup"}>
      <Subhead>{editing === null ? "Back up a folder" : "Edit this backup"}</Subhead>

      {machines.length === 0 && editing === null ? (
        <Notice
          tone="info"
          sentence="You have no machines paired yet."
          next="A backup runs on a machine you own. Pair one in Fleet, then come back."
        />
      ) : null}

      {editing === null ? (
        <>
          <Field label="Machine">
            <Select id={`${ids}-machine`} label="Machine" value={workerId} onChange={setWorkerId}>
              {machines.map((machine) => (
                <option key={machine.id} value={machine.id}>
                  {isWorkerOnline(machine, now) ? machineName(machine) : `${machineName(machine)} (offline)`}
                </option>
              ))}
            </Select>
          </Field>

          <Field label="Folder on that machine">
            <Input
              id={`${ids}-path`}
              label="Folder on that machine"
              value={localPath}
              onChange={setLocalPath}
              onEnter={submit}
              placeholder={pathPlaceholderFor(chosen)}
            />
          </Field>

          <Caption>
            Type the full path. That machine checks it on its next sweep and reports back, and it can
            refuse a folder its own policy does not list -- so this is a request the machine still
            gets to turn down.
          </Caption>
        </>
      ) : (
        <Caption>
          {`Backing up ${editing.localPath}. The machine and the path are what every re-push is matched
            on, so they stay fixed: stop this backup and start another to move them.`}
        </Caption>
      )}

      <Field label="Where it lands">
        <Select id={`${ids}-folder`} label="Where it lands" value={folderId} onChange={setFolderId}>
          <option value="">Library root</option>
          {choices.map((choice) => (
            <option key={choice.id} value={choice.id}>
              {choice.label}
            </option>
          ))}
        </Select>
      </Field>

      {repointing ? (
        <Notice
          tone="warn"
          sentence="Files already backed up stay where they are."
          next="This decides where the next ones land. Nothing that has already arrived moves."
        />
      ) : null}

      <Field label="Also skip">
        <Input
          id={`${ids}-globs`}
          label="Also skip"
          value={globs}
          onChange={setGlobs}
          placeholder="drafts/**, *.tmp"
        />
      </Field>
      <Caption>
        Comma-separated, matched against each path inside the folder. The machine already skips the
        usual build and dependency directories, so this is for anything else.
      </Caption>

      <Check checked={includeHidden} onChange={setIncludeHidden}>
        Include hidden files
      </Check>

      {error !== "" ? <Notice tone="error" sentence="That was refused." detail={error} /> : null}

      <div className="os-backup-form-actions">
        <Button tone="primary" onClick={submit} busy={busy} disabled={!ready}>
          {editing === null ? "Start backing up" : "Save changes"}
        </Button>
        <Button onClick={onCancel}>Cancel</Button>
      </div>
    </Panel>
  );
}

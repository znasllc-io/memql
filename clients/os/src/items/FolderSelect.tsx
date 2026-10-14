import { foldFolderTree, type TreeNode } from "../apps/files/fold";
import type { FolderRow } from "../apps/files/rows";
import { Select } from "../kit";

// The folder picker: "Library (top level)", then the person's folders as a tree.
//
// PROMOTED TO items/ ON ITS SECOND USE (DESIGN.md, "Applying them"). Files' Move to folder drew it
// first, as a private `flatFolderOptions` over the rail's tree; the Materializer's Make a file draws
// it for its Folder step. Two apps, one picker: a folder is offered in one order and spelled one way
// wherever a person is asked "where".
//
// ===========================================================================
// THE ORDER AND THE RULES ARE THE RAIL'S, NOT A SECOND COPY
// ===========================================================================
// `folderOptions` folds the rows with Files' own `foldFolderTree` (apps/files/fold.ts) and walks the
// tree it makes, so the picker and the rail cannot disagree about what a folder's parent is, which
// siblings come first (name, then id), or what to do with a bad snapshot: a folder whose ancestry
// never reaches the top sits at the top with its own folders beneath it, a loop is broken at its
// first member in that order and every folder is listed exactly once. The picture degrades, it
// never breaks. Archived and deleted folders are not places a file can go, so they are not offered.
//
// ===========================================================================
// A VALUE THE LIST DOES NOT HOLD IS KEPT, NOT DROPPED
// ===========================================================================
// The value a picker holds is a folder id somebody chose earlier: a setup's folder, a draft's, a
// file's current one. That folder can be archived, deleted or simply not read yet, and a picker
// that quietly dropped it would show "Library (top level)" for a file that is not there, or let go
// of a claim the cluster has not been asked about. The value stays selected as one more option,
// named `missingLabel`, so the person sees the truth and can choose another.

/** One choice: the folder's own id, and its name indented two spaces for each level it sits below the top. */
export interface FolderOption {
  id: string;
  label: string;
}

const INDENT = "  ";

/**
 * The folders a file can be filed into, in tree order. Pass every row the feed holds: archived and
 * deleted ones are left out here, and a live folder under one of them is placed at the top.
 */
export function folderOptions(folders: readonly FolderRow[]): FolderOption[] {
  const tree = foldFolderTree(folders.filter((folder) => !folder.archived && !folder.deleted));
  const options: FolderOption[] = [];
  const walk = (node: TreeNode, depth: number) => {
    options.push({ id: node.folder.id, label: `${INDENT.repeat(depth)}${node.folder.name}` });
    for (const child of node.children) walk(child, depth + 1);
  };
  for (const root of tree.roots) walk(root, 0);
  return options;
}

/**
 * The picker. `value` is a folder id, "" for the Library's top level; `onChange` is given the id
 * chosen. `label` is the accessible name (the kit's Select draws no visible label).
 */
export function FolderSelect({
  id,
  label,
  value,
  onChange,
  options,
  missingLabel = "Unavailable folder",
}: {
  id: string;
  label: string;
  value: string;
  onChange: (folderId: string) => void;
  options: readonly FolderOption[];
  /** What a value that is not among the options is called. */
  missingLabel?: string;
}) {
  const missing = value !== "" && !options.some((option) => option.id === value);
  return (
    <Select id={id} label={label} value={value} onChange={onChange}>
      <option value="">Library (top level)</option>
      {options.map((option) => (
        <option key={option.id} value={option.id}>
          {option.label}
        </option>
      ))}
      {missing ? <option value={value}>{missingLabel}</option> : null}
    </Select>
  );
}

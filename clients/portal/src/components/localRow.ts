import { useCallback, useState } from "react";

// Local selection for a TABLE_ELEMENT that has no URL row and no other
// select action. One click opens the shared RowDetail dialog.

export function useLocalRowId(): {
  rowId: string;
  onSelect: (id: string) => void;
  onClose: () => void;
  open: boolean;
} {
  const [rowId, setRowId] = useState("");
  const onSelect = useCallback((id: string) => setRowId(id), []);
  const onClose = useCallback(() => setRowId(""), []);
  return { rowId, onSelect, onClose, open: rowId !== "" };
}

export function rowWithId<T extends { readonly id?: unknown }>(
  rows: readonly T[],
  id: string,
): T | undefined {
  return rows.find((row) => row.id === id);
}

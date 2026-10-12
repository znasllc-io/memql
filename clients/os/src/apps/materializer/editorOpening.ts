import { useCallback, useEffect, useRef, useState } from "react";
import { rowString, sameEntityId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { editorArtifactURL, type EditorPreference } from "../../items/editorPreference";
import type { HandoffPorts } from "../../items/vscode";
import type { MakePath } from "./make";
import type { DescribedFile } from "./actions";
import type { DescriptionOutcome, FileEntry } from "./descriptionHandOff";
import { compositionFromRow, modelsOf } from "./rows";

export interface Reservation { navigate(url: string): void; close(): void }
export function reserveEditor(path: MakePath, ports: HandoffPorts, preference: EditorPreference): { reservation: Reservation | null; refusal: string } {
  if (path === "composed" || preference !== "browser") return { reservation: null, refusal: "" };
  try { return { reservation: ports.reserve?.() ?? null, refusal: "" }; }
  catch (error) { return { reservation: null, refusal: error instanceof Error ? error.message : "Allow this app to open a new tab, then open the file again." }; }
}

export function useEditorOpening({ domain, compositions, entries, outcomes, described }: {
  domain: string; compositions: Row[]; entries: ReadonlyMap<string, FileEntry>; outcomes: DescriptionOutcome[]; described: DescribedFile[];
}): { hold(id: string, reservation: Reservation): void; release(id: string): void } {
  const held = useRef(new Map<string, Reservation>());
  const [version, setVersion] = useState(0);
  const release = useCallback((id: string) => {
    const reservation = held.current.get(id);
    held.current.delete(id);
    try { reservation?.close(); } catch { /* A person may have closed the tab. */ }
  }, []);
  const hold = useCallback((id: string, reservation: Reservation) => {
    release(id); held.current.set(id, reservation); setVersion(v => v + 1);
  }, [release]);
  useEffect(() => () => { for (const id of held.current.keys()) release(id); }, [release]);
  useEffect(() => {
    for (const [id, reservation] of held.current) {
      const row = compositions.find(row => sameEntityId(rowString(row, "id"), id));
      if (!row) continue;
      const c = compositionFromRow(row);
      if (c.status === "failed" || c.status === "cancelled" || c.archived) { release(id); continue; }
      if (c.status !== "ready") continue;
      if (modelsOf(row).length || c.provenanceEmbedded !== false) { release(id); continue; }
      if (described.some(file => sameEntityId(file.compositionId, id)) && !outcomes.some(outcome => sameEntityId(outcome.compositionId, id))) continue;
      const entry = entries.get(c.outputFileId);
      if (!entry?.artifactId) continue;
      held.current.delete(id);
      try { reservation.navigate(editorArtifactURL(domain, entry.artifactId, entry.title || c.name, "browser")); } catch { /* A person may have closed the tab. */ }
    }
  }, [domain, compositions, entries, outcomes, described, release, version]);
  return { hold, release };
}

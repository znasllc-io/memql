import { useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { rowNumber, rowString } from "@znasllc-io/memql-sdk-core/client";
import { useAuthSource } from "../auth/context";
import { useOsIfPresent } from "../chrome/state";
import { useOsConnection } from "../live/connection";
import { accessAdmits } from "../system/registry";
import { report } from "../logs/capture";
import { Button } from "../kit";
import { artifactContentPath, downloadArtifact } from "../apps/files/actions/download";
import "./fileDownload.css";

/** Recognize only our byte route. External and malformed links never get a bearer. */
export function downloadArtifactId(href: string | undefined): string | null {
  const match = /^\/_memql\/artifacts\/([^/?#]+)\/content$/.exec(href ?? "");
  if (!match) return null;
  try {
    const id = decodeURIComponent(match[1]!);
    return /^[a-zA-Z0-9_:-]+$/.test(id) ? id : null;
  } catch { return null; }
}

/** Ask, its widget and retained history use Files' authenticated byte download. */
export function FileDownloadLink({ artifactId, fileId, name, children }: {
  artifactId?: string; fileId?: string; name: string; children?: ReactNode;
}) {
  const connection = useOsConnection();
  const auth = useAuthSource();
  const os = useOsIfPresent();
  const active = useRef(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const download = async () => {
    if (active.current) return;
    active.current = true; setBusy(true); setFailed(false);
    try {
      if (!connection) throw new Error("The Library connection is unavailable.");
      const row = (await (artifactId
        ? connection.query.libraryArtifactById({ artifactId })
        : connection.query.libraryArtifactForFile({ fileId: fileId! }))).rows()[0];
      if (!row) throw new Error("This file is unavailable or you no longer have access to it.");
      const backing = rowString(row, "kind") === "file" ? rowString(row, "sourceConceptRef").split(":").pop() ?? "" : "";
      await downloadArtifact({ artifactId: rowString(row, "id"), name: rowString(row, "title") || name, fileId: backing,
        readFile: async id => {
          const file = (await connection.query.libraryFileById({ fileId: id })).rows()[0];
          return file ? { sizeBytes: rowNumber(file, "size"), name: rowString(file, "name") } : null;
        }, bearer: () => auth.bearer() });
    } catch (error) {
      report({ level: "error", app: "files", component: "download", message: "File download failed", error,
        attributes: { reason: error instanceof Error ? error.message : String(error) }, subject: artifactId || fileId });
      setFailed(true);
    } finally { active.current = false; setBusy(false); }
  };
  return <>
    <a href={artifactId ? artifactContentPath(import.meta.env.BASE_URL, artifactId) : "#download"}
      aria-disabled={busy || undefined} aria-busy={busy || undefined}
      onClick={event => { event.preventDefault(); void download(); }}>{children ?? `Download ${name}`}</a>
    {failed ? createPortal(<div role="status" className="os-download-toast"><span>File download failed. You can try the link again.</span>
      {os && accessAdmits("app:logs") ? <Button onClick={() => { os.actions.openApp("logs", "search", { subject: artifactId || fileId }); setFailed(false); }}>Open logs</Button> : null}
      <Button onClick={() => setFailed(false)}>Dismiss</Button>
    </div>, document.body) : null}
  </>;
}

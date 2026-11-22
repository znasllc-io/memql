export interface BackupTransfer {
  id: string; workerId: string; path: string; kind: "archive" | "sync_file" | "download";
  name: string; size: number; sha256: string; state: string; desiredState: string;
  committedBytes: number; etag: string; failure: string;
  metadata: { watchId?: string; fileId?: string; sourceTransferId?: string; versionNumber?: number };
}
export interface StorageUsage {
  used: number; reserved: number; available: number; limit: number;
  clusterUsed: number; clusterReserved: number; clusterLimit: number; exempt: boolean;
}
export type TransferAction = "pause" | "resume" | "cancel" | "trash" | "restore" | "purge";
export function transferActions(t: BackupTransfer): TransferAction[] {
  if (t.state === "bin") return ["restore", "purge"];
  if (t.state === "complete") return t.kind === "archive" ? ["trash"] : [];
  if (["cancelled", "deleted"].includes(t.state)) return [];
  if (t.desiredState === "paused" || ["paused", "waiting", "error"].includes(t.state)) return ["resume", "cancel"];
  return ["pause", "cancel"];
}
export function transferLabel(t: BackupTransfer): string {
  if (t.desiredState === "paused") return "Paused";
  return ({ checking: "Preparing checksums", preparing: "Reserving storage", uploading: "Uploading", downloading: "Downloading", waiting: "Waiting to retry", error: "Needs attention", complete: "Complete", bin: "In the Bin" } as Record<string, string>)[t.state] ?? t.state;
}
/** One Downloads row's second line (owner's pick 1A). Only figures the
 *  transfer carries: it has no speed, time left or last-checked time, so the
 *  line never shows them. `progress` (0..1) draws the thin bar while moving. */
export interface TransferLineView { text: string; warn: boolean; progress: number | null; backup: boolean }
const SETTLED = ["complete", "bin", "cancelled", "deleted"];
export function transferMoving(t: BackupTransfer): boolean {
  return !SETTLED.includes(t.state) && t.desiredState !== "paused" && !["paused", "waiting", "error"].includes(t.state);
}
export function transferLine(t: BackupTransfer, machine: string, bytes: (n: number) => string): TransferLineView {
  const backup = t.kind !== "download";
  const head = backup ? `Backup from ${machine} · ` : "";
  if (t.state === "error" || (t.failure !== "" && !SETTLED.includes(t.state))) {
    return { text: `${backup ? "Upload" : "Download"} failed · ${t.failure || "needs attention"} · progress is kept`, warn: true, progress: null, backup };
  }
  if (t.state === "complete") {
    return { text: backup ? `${head}up to date · ${bytes(t.size)}` : `Downloaded · ${bytes(t.size)} saved on ${machine}`, warn: false, progress: null, backup };
  }
  if (SETTLED.includes(t.state)) return { text: `${head}${transferLabel(t)}`, warn: false, progress: null, backup };
  const where = backup ? "" : ` to ${machine}`;
  const progress = transferMoving(t) && t.size > 0 ? Math.min(1, t.committedBytes / t.size) : null;
  return { text: `${head}${transferLabel(t)}${where} · ${bytes(t.committedBytes)} of ${bytes(t.size)}`, warn: false, progress, backup };
}
export function validArchive(path: string): boolean { return /\.(zip|tar|tar\.gz|tgz)$/i.test(path.trim()); }
export function absoluteBackupPath(path: string): boolean { return /^(\/|[A-Za-z]:[\\/])/.test(path) && !/[\r\n\0]/.test(path) && !/(^|[\\/])\.\.([\\/]|$)/.test(path); }

/** Quotas use decimal bytes, so a 10 GB allowance must display as 10 GB. */
export function formatStorageBytes(bytes: number): string {
  if (bytes < 1_000) return `${bytes} B`;
  const units = ["kB", "MB", "GB", "TB"];
  const index = Math.min(3, Math.floor(Math.log(bytes) / Math.log(1_000)) - 1);
  return `${Number((bytes / 1_000 ** (index + 1)).toFixed(2))} ${units[index]}`;
}

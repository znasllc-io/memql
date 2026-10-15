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
export function validArchive(path: string): boolean { return /\.(zip|tar|tar\.gz|tgz)$/i.test(path.trim()); }
export function absoluteBackupPath(path: string): boolean { return /^(\/|[A-Za-z]:[\\/])/.test(path) && !/[\r\n\0]/.test(path) && !/(^|[\\/])\.\.([\\/]|$)/.test(path); }

/** Quotas use decimal bytes, so a 10 GB allowance must display as 10 GB. */
export function formatStorageBytes(bytes: number): string {
  if (bytes < 1_000) return `${bytes} B`;
  const units = ["kB", "MB", "GB", "TB"];
  const index = Math.min(3, Math.floor(Math.log(bytes) / Math.log(1_000)) - 1);
  return `${Number((bytes / 1_000 ** (index + 1)).toFixed(2))} ${units[index]}`;
}

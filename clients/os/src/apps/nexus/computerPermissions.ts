/** The server's exact, machine-bound permission request. Never infer one from prose. */
export interface ComputerPermission {
  version: 1;
  kind: "directory_read" | "exact_command";
  registrationId: string;
  machineId: string;
  machineLabel: string;
  tool: string;
  action: string;
  argsHash: string;
  directory?: string;
  command?: string;
  cwd?: string;
  timeoutSeconds?: number;
  allowedModes: ("once" | "always")[];
}

export function computerPermission(subject: unknown): ComputerPermission | null {
  if (!subject || typeof subject !== "object" || Array.isArray(subject)) return null;
  const raw = (subject as Record<string, unknown>).computerPermission;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const value = raw as Record<string, unknown>;
  if (value.version !== 1 || !["directory_read", "exact_command"].includes(String(value.kind))) return null;
  if (["registrationId", "machineId", "machineLabel", "tool", "action", "argsHash"].some(key => typeof value[key] !== "string" || (value[key] as string).trim() === "")) return null;
  if (!Array.isArray(value.allowedModes) || !value.allowedModes.includes("once") || value.allowedModes.some(mode => mode !== "once" && mode !== "always")) return null;
  if (value.kind === "directory_read" && (typeof value.directory !== "string" || value.directory.trim() === "")) return null;
  if (value.kind === "exact_command" && (typeof value.command !== "string" || value.command.trim() === "" || typeof value.cwd !== "string" || value.cwd.trim() === "" || typeof value.timeoutSeconds !== "number" || !Number.isInteger(value.timeoutSeconds) || value.timeoutSeconds <= 0 || value.timeoutSeconds > 600)) return null;
  return value as unknown as ComputerPermission;
}

export function permissionRememberLabel(permission: ComputerPermission): string {
  return permission.kind === "directory_read" ? "Always allow reading this directory" : "Always allow this exact command";
}

export function computerSessionAccess(subject?: Record<string, unknown> | null): string | null {
  if (subject?.sessionKind === undefined) return subject?.scope === "observe" ? "Read and observe" : "Read, write and control";
  if (subject.scope !== undefined || !["observe", "full"].includes(String(subject.sessionScope))) return null;
  switch (subject.sessionKind) {
    case "desktop": return subject.sessionScope === "observe" ? "View the desktop" : "View and control the desktop";
    case "file_write": return subject.sessionScope === "full" ? "Write files" : null;
    case "http": return "Make HTTP requests";
    default: return null;
  }
}
export function executionDestinationChange(subject?: Record<string, unknown> | null): {
  from: "auto" | "workbench" | "computer";
  to: "workbench" | "computer";
} | null {
  const raw = subject?.executionDestination;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const change = raw as Record<string, unknown>;
  if (change.to === "workbench" && change.from === "computer" && subject?.computerPermission === undefined && subject?.sessionKind === undefined && subject?.scope === undefined) return { from: "computer", to: "workbench" };
  if (change.to === "computer" && (change.from === "auto" || change.from === "workbench")) return { from: change.from, to: "computer" };
  return null;
}

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
  if (value.kind === "exact_command" && (typeof value.command !== "string" || value.command.trim() === "" || typeof value.cwd !== "string" || value.cwd.trim() === "")) return null;
  return value as unknown as ComputerPermission;
}

export function permissionRememberLabel(permission: ComputerPermission): string {
  return permission.kind === "directory_read" ? "Always allow reading this directory" : "Always allow this exact command";
}

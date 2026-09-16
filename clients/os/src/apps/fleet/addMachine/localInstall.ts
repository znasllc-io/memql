export interface LocalCockpitInstall { base: string; version: string }

/** Explicit local-build override. Never honor it on a hosted OS or remote cluster. */
export function resolveLocalCockpit(value: unknown, hostname: string, domain: string): LocalCockpitInstall | null {
  if (hostname !== "os.memql.localhost" || domain !== "memql.localhost" || !value || typeof value !== "object") return null;
  const { base, version } = value as Partial<LocalCockpitInstall>;
  if (typeof base !== "string" || typeof version !== "string" || !/^\d+\.\d+\.\d+-dev\.[a-zA-Z0-9.-]+$/.test(version)) return null;
  try {
    const url = new URL(base);
    if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || url.username || url.password || url.pathname !== "/" || url.search || url.hash) return null;
    return { base: url.origin, version };
  } catch { return null; }
}

export function localCockpitInstall(domain: string): LocalCockpitInstall | null {
  return resolveLocalCockpit(typeof __OS_LOCAL_COCKPIT__ === "undefined" ? null : __OS_LOCAL_COCKPIT__, window.location.hostname, domain);
}

export function localInstallerEnvironment(source: LocalCockpitInstall): string {
  return `MEMQL_INSTALL_RAW_BASE=${source.base}/scripts/install MEMQL_INSTALL_VERSION=${source.version} MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP=1 `;
}

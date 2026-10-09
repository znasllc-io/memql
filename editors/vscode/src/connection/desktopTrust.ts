// The installer trusts its CA in the OS; Node has a separate trust store.
// Scope that CA to the registered local cluster's API and identity origins.
// Never change process-wide trust or disable certificate/hostname verification.
import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import { Agent } from "undici";
import type { ClusterConfig } from "../clusters/model.js";
import { identityBaseUrlFor, webSocketUrlFor } from "./endpoint.js";
import { DEFAULT_CAROOT_DIR } from "../install/stackPin.js";

export class DesktopTrust {
  private agents = new Map<string, { pem: string; agent: Agent }>();
  constructor(
    private readonly clusters: () => Promise<readonly ClusterConfig[]>,
    private readonly readCA: () => Promise<string> = () => readFile(join(homedir(), DEFAULT_CAROOT_DIR, "rootCA.pem"), "utf8"),
  ) {}

  async certificateFor(url: string): Promise<string | undefined> {
    const target = secureOrigin(url);
    if (!target) return undefined;
    const clusters = await this.clusters();
    const local = clusters.some(cluster => cluster.local === true &&
      [identityBaseUrlFor(cluster), webSocketUrlFor(cluster)].some(value => value && secureOrigin(value) === target));
    if (!local) return undefined;
    try { return await this.readCA(); } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
      throw error;
    }
  }

  readonly fetch = async (url: string, init: RequestInit = {}): Promise<Response> => {
    const pem = await this.certificateFor(url);
    if (pem === undefined) return globalThis.fetch(url, { ...init, redirect: init.redirect ?? "error" });
    const origin = secureOrigin(url)!;
    let entry = this.agents.get(origin);
    if (entry?.pem !== pem) {
      if (entry) void entry.agent.close().catch(() => undefined);
      entry = { pem, agent: new Agent({ connect: { ca: pem } }) };
      this.agents.set(origin, entry);
    }
    // A redirect must not carry the local trust anchor to another origin.
    // Auth callers consume redirects themselves and token calls reject them.
    const response = await globalThis.fetch(url, {
      ...init, redirect: init.redirect === "manual" ? "manual" : "error", dispatcher: entry.agent,
    } as RequestInit);
    return response;
  };

  dispose(): void {
    for (const { agent } of this.agents.values()) void agent.destroy().catch(() => undefined);
    this.agents.clear();
  }
}

function secureOrigin(value: string): string | undefined {
  try {
    const url = new URL(value);
    if (url.protocol === "wss:") url.protocol = "https:";
    return url.protocol === "https:" && !url.username && !url.password ? url.origin : undefined;
  } catch { return undefined; }
}

// The pack browser: what .memql files a cluster carries, and their source.
//
// Three single-reply calls over the dispatcher, in the shape of ConstructsClient.
// The Go SDK's sdk/go/pack is the mirror; the names are kept parallel on purpose.
//
// A MISSING FILE IS NOT AN ERROR. The engine answers ReadPackFile with
// found=false and no wire error, so readFile resolves with found=false; only
// a queryError or an unrecognised envelope throws. A caller rendering a file
// that is not there should say so, not show an exception.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type { PackDomainWire, PackFileWire } from "../client/wire.js";

export interface PackDomain {
  name: string;
  /** "embedded" | "pack:<domain>" */
  origin: string;
  fileCount: number;
}

export interface PackFile {
  /** Relative to the domain root, e.g. "queries.memql" or "prompts/x.tmpl". */
  path: string;
  size: number;
}

export interface PackFileSource {
  domain: string;
  path: string;
  source: string;
  /** "embedded" | "pack:<domain>" | "disk:<path>" */
  origin: string;
  /** false when the file does not exist -- a normal answer, not an error. */
  found: boolean;
}

export interface PackCallOptions {
  signal?: AbortSignal;
}

export class PackClient {
  constructor(private readonly dispatcher: Dispatcher) {
    if (!dispatcher) throw new Error("PackClient: dispatcher is required");
  }

  async listDomains(opts: PackCallOptions = {}): Promise<PackDomain[]> {
    const reply = await this.dispatcher.sendAndWait(
      { listPackDomains: { requestId: newShortId() } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(`listDomains: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind !== "listPackDomainsResult") {
      throw new Error("listDomains: unexpected reply envelope");
    }
    return (payload.value.domains ?? []).map(domainFromWire);
  }

  async listFiles(domain: string, opts: PackCallOptions = {}): Promise<PackFile[]> {
    const reply = await this.dispatcher.sendAndWait(
      { listPackFiles: { requestId: newShortId(), domain } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(`listFiles: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind !== "listPackFilesResult") {
      throw new Error("listFiles: unexpected reply envelope");
    }
    return (payload.value.files ?? []).map(fileFromWire);
  }

  async readFile(domain: string, path: string, opts: PackCallOptions = {}): Promise<PackFileSource> {
    const reply = await this.dispatcher.sendAndWait(
      { readPackFile: { requestId: newShortId(), domain, path } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(`readFile: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind !== "readPackFileResult") {
      throw new Error("readFile: unexpected reply envelope");
    }
    const v = payload.value;
    return {
      domain: v.domain ?? domain,
      path: v.path ?? path,
      source: v.source ?? "",
      origin: v.origin ?? "",
      found: v.found === true,
    };
  }
}

// Every field is defaulted: protojson omits zero values, so a raw read hands
// the consumer `undefined` for exactly the common case.
function domainFromWire(d: PackDomainWire): PackDomain {
  return { name: d.name ?? "", origin: d.origin ?? "", fileCount: d.fileCount ?? 0 };
}

function fileFromWire(f: PackFileWire): PackFile {
  const raw = f.size;
  const size = typeof raw === "string" ? Number(raw) : (raw ?? 0);
  return { path: f.path ?? "", size: Number.isFinite(size) ? size : 0 };
}

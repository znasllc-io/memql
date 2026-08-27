// The real upload provider (spec D3): a host file dropped on the desk
// lands in the Library through the bff's multipart route, reached through
// the edge's same-origin API marker -- `/_memql/artifacts` -- exactly the
// way the portal's Library page uploads (memql#4341, memql#3712). The 201
// carries {artifactId}; the desk file adopts it and the provenance dot
// goes green through the ordinary derivation ("uploaded" = bytes in the
// cluster).

import type { UploadHandle, UploadProvider, UploadResult } from "./upload";

export function artifactsUploadPath(baseUrl: string): string {
  const mount = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
  return mount + "_memql/artifacts";
}

export class EdgeUploadProvider implements UploadProvider {
  constructor(
    private readonly bearer: () => Promise<string | null>,
    private readonly path: string = artifactsUploadPath(import.meta.env.BASE_URL),
    private readonly fetchImpl: typeof fetch = fetch,
  ) {}

  upload(file: File): UploadHandle {
    const abort = new AbortController();
    const done = (async (): Promise<UploadResult> => {
      const token = await this.bearer();
      const body = new FormData();
      body.append("file", file);
      body.append("name", file.name);
      const response = await this.fetchImpl(this.path, {
        method: "POST",
        body,
        credentials: "same-origin",
        ...(token ? { headers: { Authorization: `Bearer ${token}` } } : {}),
        signal: abort.signal,
      });
      if (!response.ok) {
        throw new Error(`Upload failed (${response.status}).`);
      }
      const payload = (await response.json()) as { artifactId?: unknown };
      const artifactId = typeof payload.artifactId === "string" ? payload.artifactId : "";
      if (!artifactId) throw new Error("Upload landed but named no artifact.");
      return { artifactId, title: file.name, fileKind: "file", source: "uploaded" };
    })();
    return { done, abort: () => abort.abort() };
  }
}

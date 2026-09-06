import { describe, expect, it } from "vitest";

import { SdkAskTransport, type AskStreamFn } from "../../src/ask/sdkTransport";
import { artifactsUploadPath, EdgeUploadProvider } from "../../src/items/edgeUpload";
import { refreshAccessToken } from "../../src/auth/identityClient";
import { bridgePathFor } from "../../src/live/connection";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";

const CONFIG = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "https://identity.example.test",
  oauthClientId: "c",
  authEnabled: true,
  domain: "example.test",
};

function streamOf(deltas: string[]): AskStreamFn {
  return () => ({
    deltas: (async function* () {
      for (const d of deltas) yield { textDelta: d };
    })(),
    result: Promise.resolve({}),
  });
}

describe("SdkAskTransport", () => {
  it("streams deltas through the surface callbacks and prepends the context line", async () => {
    const seen: string[] = [];
    let sentMessages: Array<{ role: string; content: string }> = [];
    const stream: AskStreamFn = (_d, messages) => {
      sentMessages = messages;
      return streamOf(["hel", "lo"])(_d, messages, {});
    };
    const transport = new SdkAskTransport(() => ({}) as Dispatcher, stream);
    await new Promise<void>((resolve, reject) => {
      transport.ask("hi", "app:artifacts section:browse", {
        delta: (t) => seen.push(t),
        done: resolve,
        error: (m) => reject(new Error(m)),
      });
    });
    expect(seen.join("")).toBe("hello");
    expect(sentMessages[0]).toEqual({ role: "system", content: "Context: app:artifacts section:browse" });
    expect(sentMessages[1]).toEqual({ role: "user", content: "hi" });
  });

  it("reports an honest in-surface error with no connection", async () => {
    const transport = new SdkAskTransport(() => null);
    const message = await new Promise<string>((resolve) => {
      transport.ask("hi", null, { delta: () => {}, done: () => {}, error: resolve });
    });
    expect(message).toMatch(/Not connected/);
  });

  it("cancel stops delivery", async () => {
    const seen: string[] = [];
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const stream: AskStreamFn = () => ({
      deltas: (async function* () {
        yield { textDelta: "a" };
        await gate;
        yield { textDelta: "b" };
      })(),
      result: Promise.resolve({}),
    });
    const transport = new SdkAskTransport(() => ({}) as Dispatcher, stream);
    const handle = transport.ask("hi", null, {
      delta: (t) => seen.push(t),
      done: () => seen.push("<done>"),
      error: (m) => seen.push(`<err:${m}>`),
    });
    await Promise.resolve();
    await Promise.resolve();
    handle.cancel();
    release();
    await new Promise((r) => setTimeout(r, 10));
    expect(seen).toEqual(["a"]);
  });
});

describe("EdgeUploadProvider", () => {
  it("posts multipart to the edge marker path with the bearer and adopts the artifact id", async () => {
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return new Response(JSON.stringify({ artifactId: "art-1", fileId: "f-1" }), { status: 201 });
    }) as unknown as typeof fetch;
    const provider = new EdgeUploadProvider(async () => "tok-123", "/_memql/artifacts", fetchImpl);
    const result = await provider.upload(new File(["x"], "notes.txt")).done;
    // `fileId` rides through from the same 201 (epic memql#4970): the upload
    // route has always answered with it, and a surface about a FILE -- its
    // status, its summary, the domains it teaches -- needs the file row
    // rather than its index entry.
    expect(result).toEqual({
      artifactId: "art-1",
      fileId: "f-1",
      title: "notes.txt",
      fileKind: "file",
      source: "uploaded",
    });
    expect(calls[0]!.url).toBe("/_memql/artifacts");
    expect((calls[0]!.init.headers as Record<string, string>).Authorization).toBe("Bearer tok-123");
    expect(calls[0]!.init.body).toBeInstanceOf(FormData);
  });

  it("rejects with the server's sentence verbatim, or the status when there is none", async () => {
    // The v2 rule (epic #4721): over-cap and over-quota refusals reach the
    // surface as the engine's own words, which name the numbers -- a
    // paraphrase would drop the one fact that helps. The status is the
    // fallback for an empty body.
    const worded = (async () => new Response("file too large: max 32 MiB", { status: 413 })) as unknown as typeof fetch;
    await expect(
      new EdgeUploadProvider(async () => null, "/_memql/artifacts", worded)
        .upload(new File(["x"], "big.bin")).done,
    ).rejects.toThrow("file too large: max 32 MiB");

    const bare = (async () => new Response("", { status: 413 })) as unknown as typeof fetch;
    await expect(
      new EdgeUploadProvider(async () => null, "/_memql/artifacts", bare)
        .upload(new File(["x"], "big.bin")).done,
    ).rejects.toThrow(/413/);
  });

  it("derives the marker path from the base url", () => {
    expect(artifactsUploadPath("/")).toBe("/_memql/artifacts");
    expect(bridgePathFor("/")).toBe("/_memql/ws");
  });
});

describe("refreshAccessToken", () => {
  it("returns the access_token field on 200 and null otherwise", async () => {
    const ok = (async () =>
      new Response(JSON.stringify({ access_token: "tok" }), { status: 200 })) as unknown as typeof fetch;
    expect(await refreshAccessToken(CONFIG, ok)).toBe("tok");

    const noSession = (async () => new Response("{}", { status: 401 })) as unknown as typeof fetch;
    expect(await refreshAccessToken(CONFIG, noSession)).toBeNull();

    const empty = (async () => new Response("{}", { status: 200 })) as unknown as typeof fetch;
    expect(await refreshAccessToken(CONFIG, empty)).toBeNull();
  });

  it("never leaks the credential into the URL", async () => {
    const urls: string[] = [];
    const spy = (async (url: string) => {
      urls.push(url);
      return new Response("{}", { status: 200 });
    }) as unknown as typeof fetch;
    await refreshAccessToken(CONFIG, spy);
    expect(urls[0]).toBe("https://identity.example.test/auth/refresh");
  });
});

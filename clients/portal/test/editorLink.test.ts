import { afterEach, describe, expect, it } from "vitest";

import {
  EDITOR_SCHEME_STORAGE_KEY,
  clusterDomainFor,
  editorOpenUri,
  readStoredEditorScheme,
  storeEditorScheme,
} from "../src/cluster/editorLink";

describe("clusterDomainFor", () => {
  it("prefers the served domain", () => {
    expect(clusterDomainFor({ domain: "acme.example.com", identityUrl: "https://identity.other.test" })).toBe(
      "acme.example.com",
    );
  });
  it("derives the domain from identityUrl when the node omits it", () => {
    expect(clusterDomainFor({ domain: "", identityUrl: "https://identity.acme.example.com" })).toBe("acme.example.com");
    expect(clusterDomainFor({ domain: "", identityUrl: "https://identity.memql.localhost:443/" })).toBe("memql.localhost");
  });
  it("refuses to guess from a host that is not the identity role host", () => {
    expect(clusterDomainFor({ domain: "", identityUrl: "https://login.acme.example.com" })).toBe("");
    expect(clusterDomainFor({ domain: "", identityUrl: "" })).toBe("");
    expect(clusterDomainFor({ domain: "", identityUrl: "not a url" })).toBe("");
  });
});

describe("editorOpenUri", () => {
  it("composes the v=1 contract with every value encoded once", () => {
    expect(
      editorOpenUri({ scheme: "vscode", domain: "acme.example.com", kind: "concept", name: "v1:cognition:space" }),
    ).toBe("vscode://znasllc.memql/open?v=1&cluster=acme.example.com&kind=concept&name=v1%3Acognition%3Aspace");
  });
  it("swaps only the scheme for Insiders", () => {
    expect(editorOpenUri({ scheme: "vscode-insiders", domain: "d.test", kind: "query", name: "a b" })).toBe(
      "vscode-insiders://znasllc.memql/open?v=1&cluster=d.test&kind=query&name=a%20b",
    );
  });
});

describe("the remembered scheme", () => {
  afterEach(() => {
    globalThis.localStorage?.removeItem(EDITOR_SCHEME_STORAGE_KEY);
  });
  it("defaults to vscode and round-trips Insiders", () => {
    expect(readStoredEditorScheme()).toBe("vscode");
    storeEditorScheme("vscode-insiders");
    expect(readStoredEditorScheme()).toBe("vscode-insiders");
    storeEditorScheme("vscode");
    expect(readStoredEditorScheme()).toBe("vscode");
  });
  it("ignores a stored value it does not recognise", () => {
    globalThis.localStorage?.setItem(EDITOR_SCHEME_STORAGE_KEY, "emacs");
    expect(readStoredEditorScheme()).toBe("vscode");
  });
});

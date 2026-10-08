import { afterEach, expect, it, vi } from "vitest";
import { editorArtifactURL, editorTemplateURL } from "../../src/items/editorPreference";
import { EDITOR_COLOR_KEYS, readEditorAppearance } from "../../../../brand/editorAppearance";
import { VELLUM, COBALT } from "../../src/themes/builtins";

afterEach(()=>{document.documentElement.removeAttribute("style");delete document.documentElement.dataset.theme;vi.unstubAllGlobals();});
it.each(["browser","vscode","cursor"] as const)("hands the rendered colors to %s without changing resource identity",host=>{
  document.documentElement.dataset.theme="light";
  for(const key of EDITOR_COLOR_KEYS) document.documentElement.style.setProperty(`--os-${key}`,VELLUM.tokens.light[key]);
  const url=new URL(editorArtifactURL("cluster.example","file-1","Review.md",host));
  const appearance=readEditorAppearance(url.searchParams.get("appearance"));
  expect(appearance?.mode).toBe("light");
  expect(appearance?.colors.accent).toBe(VELLUM.tokens.light.accent);
  expect(appearance?.colors.ground).toBe(VELLUM.tokens.light.ground);
  expect(url.searchParams.get("resource")).toBe("memql-file://cluster.example/artifacts/file-1/Review.md");
  expect(document.querySelector("span")).toBeNull();
});
it("resolves System and includes template handoffs",()=>{
  vi.stubGlobal("matchMedia",vi.fn(()=>({matches:false})));
  for(const key of EDITOR_COLOR_KEYS) document.documentElement.style.setProperty(`--os-${key}`,COBALT.tokens.dark[key]);
  const appearance=readEditorAppearance(new URL(editorTemplateURL("cluster.example","template-1","Welcome")).searchParams.get("appearance"));
  expect(appearance?.mode).toBe("dark");expect(appearance?.colors.accent).toBe(COBALT.tokens.dark.accent);
});
it("a missing palette never prevents opening a file",()=>{
  expect(new URL(editorArtifactURL("cluster.example","file-1")).searchParams.has("appearance")).toBe(false);
});

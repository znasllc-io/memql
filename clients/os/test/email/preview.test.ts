import { describe, expect, it } from "vitest";
import { emailPreview } from "../../src/apps/email/preview";

describe("email preview isolation", () => {
  it("removes active content, tracking requests and dangerous links, retaining sign-in links", () => {
    const result = emailPreview(`<base href="https://evil.test"><style>body{background:url(https://track.test)}</style><script>alert(1)</script><p onclick="alert(2)" style="background:url(https://track.test)">Hello</p><img src="https://track.test/pixel"><iframe src="https://evil.test"></iframe><form action="https://evil.test"><input></form><svg onload="alert(3)"></svg><a href="javascript:alert(4)">Unsafe</a><a href="data:text/html,test">Data</a><a href="https://identity.example.test/complete?token=123">Sign in</a>`);
    const doc = new DOMParser().parseFromString(result, "text/html");
    expect(doc.querySelectorAll("script,img,iframe,form,input,svg,base,[onclick],[onload],[style]")).toHaveLength(0);
    expect(result).not.toContain("track.test");
    expect(result).not.toContain("evil.test");
    expect(result).not.toContain("javascript:");
    expect(result).not.toContain("data:text");
    expect(doc.querySelector("a[href]")?.getAttribute("href")).toBe("https://identity.example.test/complete?token=123");
    expect(doc.querySelector("a[href]")?.getAttribute("rel")).toBe("noopener noreferrer");
    expect(doc.querySelector("meta[http-equiv]")?.getAttribute("content")).toContain("default-src 'none'");
  });
});

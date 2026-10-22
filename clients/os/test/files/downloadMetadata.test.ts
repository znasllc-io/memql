import { describe, expect, it, vi } from "vitest";
import { downloadMetadata } from "../../src/apps/files/actions/downloadMetadata";
import { runBufferedDownload } from "../../src/apps/files/actions/download";

describe("download response metadata", () => {
  it("refuses a successful SPA fallback while allowing an actual HTML attachment", () => {
    expect(() => downloadMetadata(new Response("<html>app</html>", { headers: { "Content-Type": "text/html" } }), "report.pdf")).toThrow("web page");
    expect(downloadMetadata(new Response("<html>report</html>", { headers: { "Content-Type": "text/html", "Content-Disposition": 'attachment; filename="report.html"' } }), "title").name).toBe("report.html");
  });
  it("uses the server filename for a rendered body or retained version", async () => {
    const save = vi.fn();
    await runBufferedDownload({ artifactId: "a", fileName: "display title", bearer: async () => "token",
      fetchImpl: vi.fn(async () => new Response("# original", { headers: { "Content-Disposition": 'attachment; filename="README.md"', "Content-Type": "text/markdown" } })),
      createObjectUrl: () => "blob:test", revokeObjectUrl: () => {}, save });
    expect(save).toHaveBeenCalledWith("blob:test", "README.md");
  });
  it("decodes Unicode, preserves compound extensions and removes path/control characters", () => {
    const response = new Response("", { headers: { "Content-Disposition": "attachment; filename=archive.tar.gz; filename*=UTF-8''r%C3%A9sum%C3%A9.tar.gz", "Content-Type": "application/gzip", "Content-Length": "123" } });
    expect(downloadMetadata(response, "wrong.html")).toEqual({ name: "résumé.tar.gz", mimeType: "application/gzip", size: 123 });
    expect(downloadMetadata(new Response(), "../unsafe\r\n.png").name).toBe(".._unsafe__.png");
  });
});

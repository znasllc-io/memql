import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { AskMessage } from "../../src/ask/AskMessage";
import { downloadArtifactId } from "../../src/items/FileDownloadLink";

it("renders readable replies without activating model-provided HTML or media", () => {
  const { container } = render(<AskMessage text={'**Fleet**\n\n- `local`\n\n[Help](https://example.com)\n\n<script>alert(1)</script>\n\n![tracking](https://example.com/pixel)\n\n[unsafe](javascript:alert(1))'} />);
  expect(container.querySelector("strong")?.textContent).toBe("Fleet");
  expect(screen.getByRole("listitem").textContent).toBe("local");
  expect(screen.getByRole("link", { name: "Help" }).getAttribute("rel")).toBe("noopener noreferrer");
  expect(container.querySelector("script, img")).toBeNull();
  expect(screen.getByText("unsafe").getAttribute("href")).not.toMatch(/^javascript:/);
});

it("turns saved artifact references into authenticated downloads without a new tab", () => {
  render(<AskMessage text="[Download tool.zip](/_memql/artifacts/artifact-123/content)" />);
  const link = screen.getByRole("link", { name: "Download tool.zip" });
  expect(link.getAttribute("target")).toBeNull();
  expect(downloadArtifactId(link.getAttribute("href")!)).toBe("artifact-123");
  for (const href of ["https://evil.test/_memql/artifacts/a/content", "//evil.test/_memql/artifacts/a/content", "/_memql/artifacts/a%2fb/content", "/_memql/artifacts/a/content?redirect=elsewhere"]) expect(downloadArtifactId(href)).toBeNull();
});

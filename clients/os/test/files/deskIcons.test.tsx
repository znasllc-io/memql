import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { FileIcon } from "../../src/items/FileIcon";
import { FolderIcon } from "../../src/items/FolderIcon";
import { artifactFromRow, artifactName, deskEntryOf } from "../../src/apps/files/rows";
import { typeLabel } from "../../src/apps/files/Tiles";

// The desk draws a file the way the Library tile does (José, Nov 19 call;
// Jesús picked Draft B, Nov 20 2026): the same page with the kind written on
// it, the same folder, inside the Library's own glyph box -- the one rule
// block in styles/index.css that sizes both.

const noop = () => {};

function deskIcon(title: string, format?: string) {
  const { container } = render(
    <FileIcon
      entry={{ id: "i-1", artifactId: "a-1", title, fileKind: "document", source: "uploaded", ...(format ? { format } : {}) }}
      selected={false}
      onOpen={noop}
      onSelect={noop}
    />,
  );
  return container;
}

describe("desk icons wear the Library's glyphs", () => {
  it("draws a file as the Library's page, with its kind on it, in the Library's glyph box", () => {
    const icon = deskIcon("Quarterly report.pdf");
    expect(icon.querySelector(".os-files-tile-icon .os-files-sheet b")?.textContent).toBe("PDF");
    // No per-kind lucide glyph any more: one drawing, whatever the kind.
    expect(icon.querySelector(".lucide")).toBeNull();
  });

  it("names the kind from the recorded format when the title has no extension", () => {
    expect(deskIcon("Workshop notes", "md").querySelector(".os-files-sheet b")?.textContent).toBe("MD");
    // A shortcut kept before the format was: the outline stands alone, as a
    // Library tile with nothing to say does.
    expect(deskIcon("Workshop notes").querySelector(".os-files-sheet b")).toBeNull();
  });

  it("says what the Library tile says for the same row", () => {
    const row = artifactFromRow({ id: "a-9", kind: "document", source: "user_created", title: "Plan", format: "md" });
    const entry = deskEntryOf(row);
    expect(entry.format).toBe("md");
    expect(typeLabel(entry.title, entry.format)).toBe(typeLabel(artifactName(row), row.format));
  });

  it("draws a folder as the Library's folder", () => {
    const { container } = render(
      <FolderIcon
        id="f-1"
        name="Reports"
        selected={false}
        isDropTarget={false}
        renaming={false}
        onSelect={noop}
        onOpen={noop}
        onRenameCommit={noop}
        onRenameCancel={noop}
      />,
    );
    expect(container.querySelector(".os-files-tile-icon .os-files-glyph-folder")).not.toBeNull();
    expect(container.querySelector(".lucide")).toBeNull();
  });
});

import { File, FileText, Sparkles } from "lucide-react";

// One glyph per content kind, in one place.
//
// IT LIVES HERE RATHER THAN IN BrowseSection BECAUSE THE RAIL NEEDS IT NOW
// (epic memql#4981). The Bin place draws file rows, and BrowseSection already
// imports Rail -- so reaching back for the glyph would close a runtime import
// cycle between the two. A `import type` does not (types are erased), which is
// why the existing `DeskFolderShortcut` import was harmless and this one would
// not have been.
//
// It is deliberately NOT in `kit/`: the kit is the OS's shared vocabulary, and
// which glyph means "generated output" is one app's reading of one concept's
// enum. Four surfaces share it -- the list row, the inspector, the Bin's rows
// and its detail panel -- and they must not drift, which is the whole reason
// there is one function rather than four `<File />`s.

export function kindGlyph(kind: string, size = 16) {
  if (kind === "document") return <FileText size={size} aria-hidden />;
  if (kind === "generated_output") return <Sparkles size={size} aria-hidden />;
  return <File size={size} aria-hidden />;
}

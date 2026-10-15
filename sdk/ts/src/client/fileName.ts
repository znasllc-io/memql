// What a person may call a new file (design:
// docs/superpowers/specs/2026-10-11-materializer-front-door-design.md,
// amendment 11).
//
// A MODULE OF ITS OWN, apart from materialize.ts. The name rule is a client's
// check of what a person types, and it has no counterpart in the Library;
// materialize.ts holds copies of the Library's review-attachment limits, which
// integrations/library holds to the Library's own (every MAX_* there is one of
// those). A name limit kept beside them would read as one more.
//
// Each refusal says what is actually wrong, in the words a person reads where
// they type the name. VS Code's New Markdown and Create Email from Examples and
// the MemQL OS composer check a name with fileNameProblem as it is typed, so
// they say the same thing about the same name. The cluster refuses the names it
// cannot file in its own words, for a client that sends one anyway.
import type { MaterializeFormat } from "./materialize.js";

const NO_NAME = "Enter a file name.";
const NOT_A_FILE_NAME = "Enter a file name, without a folder path.";
const NAME_TOO_LONG = "Use a name up to 150 characters.";
const NO_NAME_BEFORE_EXTENSION = "Enter a name before the extension.";
const MAX_NAME_LENGTH = 150;
// The suffixes that already name a format: its file extension and, for Markdown,
// its own name too (formatNameExtensions in integrations/compose/materialize.go).
const formatExtensions = (format: MaterializeFormat): string[] => (format === "markdown" ? ["md", "markdown"] : [format]);

/**
 * The name a file of this format is filed under: the name without ONE trailing
 * extension that already names the format, because the cluster adds the
 * extension once when it files the file ("Plan.md" is filed as Plan.md, never
 * Plan.md.md). An extension of another format is part of the name. Mirrors
 * cleanFileName in integrations/compose/materialize.go.
 */
export function fileNameStem(name: string, format: MaterializeFormat): string {
  const trimmed = name.trim();
  for (const extension of formatExtensions(format)) {
    const suffix = `.${extension}`;
    if (trimmed.length >= suffix.length && trimmed.slice(-suffix.length).toLowerCase() === suffix) return trimmed.slice(0, -suffix.length).trim();
  }
  return trimmed;
}

/**
 * What is wrong with a name typed for a new file of this format, or undefined
 * when it can be used, in that order: there is no name; it is a place (a slash,
 * a backslash or a control character -- folderId says where a file goes); it is
 * over 150 characters; or there is nothing before the extension, because ".md"
 * on its own would file a document with no name.
 */
export function fileNameProblem(name: string, format: MaterializeFormat): string | undefined {
  if (!name.trim()) return NO_NAME;
  if (/[\\/\u0000-\u001f]/.test(name)) return NOT_A_FILE_NAME;
  if (name.length > MAX_NAME_LENGTH) return NAME_TOO_LONG;
  if (!fileNameStem(name, format)) return NO_NAME_BEFORE_EXTENSION;
  return undefined;
}

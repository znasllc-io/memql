import { editorArtifactURL, editorTemplateURL } from "../../../items/editorPreference";
import type { ActSpec } from "../acts";
import { isPublished, templateIsArchived, templateName, type TemplateRecord, type TemplateRow } from "../rows";

// WHAT A TEMPLATE'S PAGE OFFERS (DESIGN.md rule 12), as a pure table.
//
// A template is WRITTEN IN THE EDITOR (spec 1.6, Ruling 9): its page opens the
// editor, and puts the template away or brings it back. Nothing here edits it.
//
//   draft       Archive                  Open in editor
//   published   Archive (asks)           Open in editor
//   asking      Keep                     Archive (does it)
//   archived                             Restore
//
// A published template is asked about before it is archived, because
// campaigns and automations that use it stop being able to send it; a draft
// sends nothing, so its Archive does it. The Archive that asks and the Archive
// that does are two acts with two ids, so the page can tell them apart. An
// archived template comes back first and is opened after.

const ARCHIVE_NOW: ActSpec = { id: "archive-now", label: "Archive", text: true };
const ARCHIVE_ASKS: ActSpec = { id: "archive", label: "Archive", text: true };
const KEEP: ActSpec = { id: "keep", label: "Keep", text: true };
const ARCHIVE_CONFIRMED: ActSpec = { id: "archive-confirmed", label: "Archive", tone: "danger" };
const OPEN_IN_EDITOR: ActSpec = { id: "open-editor", label: "Open in editor", tone: "primary" };
const RESTORE: ActSpec = { id: "restore", label: "Restore", tone: "primary" };

/** What a template's page offers, given what is being asked of it. */
export function templateActs(t: TemplateRow, asking: "" | "archive"): ActSpec[] {
  if (templateIsArchived(t)) return [RESTORE];
  if (!isPublished(t)) return [ARCHIVE_NOW, OPEN_IN_EDITOR];
  return asking === "archive" ? [KEEP, ARCHIVE_CONFIRMED] : [ARCHIVE_ASKS, OPEN_IN_EDITOR];
}

/** A file name long enough to say the template and short enough to keep its extension. */
const NAME_CHARS = 160;

/**
 * Where "Open in editor" goes: the Library file the template was published
 * from, when it has one -- that is the email as it is written -- and otherwise
 * the template itself, as a file. Either way the extension picks the email
 * editor.
 */
export function templateEditorURL(domain: string, t: TemplateRow): string {
  const name = templateName(t).slice(0, NAME_CHARS);
  return t.sourceArtifactId !== "" ? editorArtifactURL(domain, t.sourceArtifactId, `${name}.email.json`) : editorTemplateURL(domain, t.id, name);
}

/**
 * The email a template holds, as the document it was saved from: the stored
 * document, or -- for a template saved before documents existed -- the custom
 * email its bodies make. What the template's page hands the renderer. From the
 * template read whole, by id: a list row holds none of it. (Archive and Restore
 * send none: the engine reads no content for either.)
 */
export function lifecycleContent(t: TemplateRecord): Record<string, unknown> {
  return t.document ?? { subject: t.subject, textBody: t.textBody, htmlBody: t.htmlBody };
}

import { editorNewEmailURL } from "../../../items/editorPreference";
import { openHandoff, type HandoffPorts } from "../../../items/vscode";
import { isPublished, templateIsArchived, templateName, type TemplateRow } from "../rows";

// THE WAIT FOR A NEW TEMPLATE (memql#6033, Ruling 9).
//
// "Create new template" creates nothing here. It opens the editor's New email
// page, and the template exists only once the person publishes the email there:
// in another window, written by another node. The OS learns of it the way it
// learns of everything, off the live templates feed, so the wait is a small pure
// model over that feed: remember which templates were already there, and report
// the first one that was not.
//
// NEW MEANS AN ID THE LIST DID NOT HOLD WHEN THE WAIT BEGAN. Not a status that
// changed and not a newer revision: publishing an older template again is that
// template changing, and it must not carry a campaign onto it. A template in
// another organization is somebody else's and an archived one is put away;
// neither ends the wait.
//
// The editor publishes in two writes, a draft and then the published row, so
// the same template is reported twice: first as a draft, then as published.
// `requirePublished` says which of the two ends the wait: the campaign wizard
// needs a template it can send, so only the published row does; the Templates
// wizard takes whatever arrives. The finder applies it, so a caller asks one
// question, did something arrive, and a draft that comes first (one the person
// never publishes) does not hide a published template that comes later.

export interface TemplateWait {
  /** The organization the new email is for: the only one whose templates end the wait. */
  accountId: string;
  /** Every template id the list held when the wait began, in any state and organization. */
  known: ReadonlySet<string>;
  /** When the wait began, in milliseconds, for the elapsed time the bar shows. */
  startedAt: number;
  /** True when only a published template ends the wait (the campaign wizard must be able to send what it picks); false takes whatever arrives. */
  requirePublished: boolean;
}

/**
 * Begins waiting for a template to appear in `accountId`.
 *
 * Hand it every template the list holds now, archived ones and other
 * organizations' included, and begin from a list that has finished loading: a
 * row that was always there but arrives late would look new.
 */
export function beginTemplateWait(
  templates: readonly TemplateRow[],
  accountId: string,
  requirePublished: boolean,
  now: number,
): TemplateWait {
  return { accountId, known: new Set(templates.map((template) => template.id)), startedAt: now, requirePublished };
}

/**
 * The first template in the wait's organization that ends the wait: its id was
 * not known when the wait began, it is not archived and, for a wait that
 * `requirePublished`, it is published. A caller asks only whether something
 * arrived: a draft that comes first does not hide a published template that
 * comes later. `published` is a fact about the template, so it is always true
 * for a wait that needs publication.
 *
 * A draft such a wait is still holding out for is not an arrival, but the bar
 * names it: ask the same question of a wait that takes whatever arrives,
 * `arrivedTemplate({ ...wait, requirePublished: false }, templates)`, and hand
 * the answer to `waitWords`.
 */
export function arrivedTemplate(
  wait: TemplateWait,
  templates: readonly TemplateRow[],
): { template: TemplateRow; published: boolean } | null {
  const template = templates.find(
    (candidate) =>
      candidate.accountId === wait.accountId &&
      !wait.known.has(candidate.id) &&
      !templateIsArchived(candidate) &&
      (!wait.requirePublished || isPublished(candidate)),
  );
  return template ? { template, published: isPublished(template) } : null;
}

/**
 * Opens the editor's New email page for the organization.
 *
 * Call it inside the click: a browser tab opened after anything asynchronous is
 * a blocked pop-up. Returns the way to stop listening for the editor's answer.
 */
export function openNewEmail(domain: string, accountId: string, onNoAnswer: () => void, ports?: HandoffPorts): () => void {
  return openHandoff(editorNewEmailURL(domain, accountId), onNoAnswer, ports);
}

const PUBLISH = "Publish it in the editor to continue.";

/**
 * What the bar says while it waits. A draft that has arrived is named, because it
 * is what is being waited on; `arrivedTemplate` says how to ask for it.
 */
export function waitWords(
  wait: TemplateWait,
  arrived: { template: TemplateRow; published: boolean } | null,
): { word: string; detail: string } {
  const word = "Waiting for the new template";
  if (wait.requirePublished && arrived && !arrived.published) {
    return { word, detail: `“${templateName(arrived.template)}” is a draft. ${PUBLISH}` };
  }
  return { word, detail: PUBLISH };
}

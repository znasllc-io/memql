// The notice the documents promise "in the platform" when a version changes.
//
// Before a person signs in the page cannot know who they are, so the moment
// to say "the Terms of Service changed" is right after sign-in. Identity
// already knows the fact: its session seam recorded every document whose
// version in force differed from what the person had accepted, stamped with
// the session it created, and the account overview lists what the caller's
// own session recorded (component/identity/web/legal_notice.go). The OS asks
// once per signed-in session, says so, and keeps only a per-tab dismissal --
// no acknowledgment is written anywhere, and nothing here blocks sign-in.

import type { OsRuntimeConfig } from "../cluster/config";
import { nativeIdentity, type IdentityPage } from "./nativeIdentity";
import type { OsAuthSource } from "./source";

export interface LegalNotice {
  /** "tos" or "privacy", also the path of the document (`/legal/<type>`). */
  documentType: string;
  /** The title identity shows for it. */
  document: string;
  version: string;
  /** The front matter's date; empty when identity could not say. */
  effectiveDate: string;
  /** True when an earlier version had been accepted: the version changed. */
  changed: boolean;
}

/** Reads identity's `LegalNotices` off the account overview; anything that
 * is not a list of {DocumentType, Version} objects reads as nothing. */
export function legalNoticesFrom(page: IdentityPage): LegalNotice[] {
  const raw = page.data?.LegalNotices;
  if (!Array.isArray(raw)) return [];
  const out: LegalNotice[] = [];
  for (const item of raw) {
    if (typeof item !== "object" || item === null) continue;
    const record = item as Record<string, unknown>;
    if (typeof record.DocumentType !== "string" || !record.DocumentType || typeof record.Version !== "string" || !record.Version) continue;
    out.push({
      documentType: record.DocumentType,
      document: typeof record.Document === "string" && record.Document ? record.Document : record.DocumentType,
      version: record.Version,
      effectiveDate: typeof record.EffectiveDate === "string" ? record.EffectiveDate : "",
      changed: record.Changed === true,
    });
  }
  return out;
}

/** What this sign-in accepted, from the account overview. Every failure is
 * an empty answer: a notice is never worth a broken desk. */
export async function loadLegalNotices(config: OsRuntimeConfig, authSource: OsAuthSource): Promise<LegalNotice[]> {
  try {
    const bearer = await authSource.bearer();
    if (!bearer) return [];
    return legalNoticesFrom(await nativeIdentity(config, "/me/", { bearer }));
  } catch {
    return [];
  }
}

const DISMISSED_KEY = "memql.os.legal-notice.dismissed";
const key = (notices: LegalNotice[]) => notices.map(n => `${n.documentType}:${n.version}`).join(",");

/** True when nothing is left to say: no notices, or these exact versions were dismissed in this tab. */
export function legalNoticesDismissed(notices: LegalNotice[]): boolean {
  if (notices.length === 0) return true;
  try { return sessionStorage.getItem(DISMISSED_KEY) === key(notices); } catch { return false; }
}

export function dismissLegalNotices(notices: LegalNotice[]): void {
  try { sessionStorage.setItem(DISMISSED_KEY, key(notices)); } catch { /* a private window forgets; the notice returns on reload */ }
}

/** One sentence per document, in the documents' own terms. */
export function legalNoticeSentence(notice: LegalNotice): string {
  const when = notice.effectiveDate ? `, effective ${notice.effectiveDate}` : "";
  return notice.changed
    ? `The ${notice.document} changed: version ${notice.version}${when}. Signing in accepted the new version.`
    : `By signing in you accepted the ${notice.document}, version ${notice.version}${when}.`;
}

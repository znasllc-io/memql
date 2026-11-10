import { useEffect, useState } from "react";
import { Button, Notice } from "../kit";
import { useOptionalAuth } from "./AuthProvider";
import { dismissLegalNotices, legalNoticeSentence, legalNoticesDismissed, loadLegalNotices, type LegalNotice } from "./legalNotice";
import { identityLocation } from "./nativeIdentity";
import { isRoleTestTab } from "./roleTest";
import "./legalNotice.css";

/** After sign-in, says which legal-document versions that sign-in accepted
 * (legalNotice.ts). Quiet, dismissible per tab, never in the way of signing in.
 * Outside an AuthProvider (the Shell's own tests) there is no sign-in, so it
 * reads and draws nothing (useOptionalAuth). */
export function LegalNoticeBanner() {
  const auth = useOptionalAuth();
  const status = auth?.status;
  const config = auth?.config;
  const authSource = auth?.authSource;
  const [notices, setNotices] = useState<LegalNotice[]>([]);
  useEffect(() => {
    if (!config || !authSource || status !== "signed-in" || !config.authEnabled || isRoleTestTab()) { setNotices([]); return; }
    let cancelled = false;
    void loadLegalNotices(config, authSource).then(found => { if (!cancelled) setNotices(found); });
    return () => { cancelled = true; };
  }, [status, config, authSource]);
  if (legalNoticesDismissed(notices)) return null;
  return <LegalNoticeView notices={notices} onDismiss={() => { dismissLegalNotices(notices); setNotices([]); }} />;
}

export function LegalNoticeView({ notices, onDismiss }: { notices: LegalNotice[]; onDismiss: () => void }) {
  return <aside className="os-legal-notice" role="status" aria-label="Legal documents">
    <Notice tone="info">
      {notices.map(notice => <p key={`${notice.documentType}:${notice.version}`} className="os-notice-line">{legalNoticeSentence(notice)}</p>)}
      <div className="os-legal-notice-actions">
        {notices.map(notice => <a key={notice.documentType} className="os-button" data-tone="quiet" href={identityLocation(`/legal/${notice.documentType}`)}>Read the {notice.document}</a>)}
        <Button onClick={onDismiss}>Dismiss</Button>
      </div>
    </Notice>
  </aside>;
}

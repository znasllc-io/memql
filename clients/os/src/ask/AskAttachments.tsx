import { FileText, X } from "lucide-react";

import type { AskAttachmentDraft, AskTurnAttachment } from "./conversationSession";

/**
 * A failed upload's chip text: a few words, with the cluster's own sentence
 * kept for the tooltip and screen readers. A quota refusal names itself
 * because it is the one failure the person fixes somewhere else.
 */
export function attachmentProblem(error: string | undefined): string {
  return /quota/i.test(error ?? "") ? "Over your storage quota" : "Upload failed";
}

/** The files in the message field, above the text: name, progress or the
 *  failure, and a remove control each. */
export function AskAttachmentChips({ attachments, onRemove }: {
  attachments: readonly AskAttachmentDraft[];
  onRemove: (id: string) => void;
}) {
  if (attachments.length === 0) return null;
  return (
    <ul className="os-ask-attachments" aria-label="Attached files">
      {attachments.map(item => (
        <li key={item.id} className="os-ask-attachment" data-state={item.state} title={item.state === "failed" && item.error ? `${item.name}: ${item.error}` : item.name}>
          <FileText size={13} aria-hidden />
          <span className="os-ask-attachment-name">{item.name}</span>
          {item.state === "uploading" ? (
            <span className="os-ask-attachment-progress" role="progressbar" aria-label={`Uploading ${item.name}`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round((item.progress ?? 0) * 100)}>
              <span style={{ width: `${Math.round((item.progress ?? 0) * 100)}%` }} />
            </span>
          ) : null}
          {item.state === "failed" ? <span className="os-ask-attachment-problem">{attachmentProblem(item.error)}</span> : null}
          <button type="button" className="os-ask-attachment-remove" aria-label={`Remove ${item.name}`} title="Remove" onClick={() => onRemove(item.id)}>
            <X size={12} aria-hidden />
          </button>
        </li>
      ))}
    </ul>
  );
}

/** The files a sent message carried, under its text in the conversation. */
export function AskSentAttachments({ attachments, onOpenFile }: {
  attachments: readonly AskTurnAttachment[] | undefined;
  onOpenFile?: (fileId: string) => void;
}) {
  if (!attachments?.length) return null;
  return (
    <ul className="os-ask-sent-attachments" aria-label="Attached files">
      {attachments.map(file => (
        <li key={file.artifactId}>
          {onOpenFile && file.fileId ? (
            <button type="button" className="os-ask-attachment" onClick={() => onOpenFile(file.fileId!)}>
              <FileText size={13} aria-hidden /><span className="os-ask-attachment-name">{file.name}</span>
            </button>
          ) : (
            <span className="os-ask-attachment"><FileText size={13} aria-hidden /><span className="os-ask-attachment-name">{file.name}</span></span>
          )}
        </li>
      ))}
    </ul>
  );
}

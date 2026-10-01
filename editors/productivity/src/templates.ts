export interface EmailTemplate { subject: string; textBody: string; htmlBody: string }
export function readTemplate(value: string): EmailTemplate {
  if (new TextEncoder().encode(value).byteLength > 2 * 1024 * 1024) throw new Error("Email templates must be 2 MiB or smaller.");
  const parsed: unknown = JSON.parse(value);
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("An email template must be a JSON object.");
  const row = parsed as Record<string, unknown>;
  for (const key of Object.keys(row)) if (!["subject", "textBody", "htmlBody"].includes(key)) throw new Error(`Unknown template field: ${key}`);
  if (typeof row.subject !== "string" || !row.subject.trim() || /[\u0000-\u001f\u007f]/.test(row.subject)) throw new Error("Enter a subject on one line.");
  if (typeof row.textBody !== "string" || typeof row.htmlBody !== "string") throw new Error("The template needs textBody and htmlBody strings.");
  if (new TextEncoder().encode(row.subject).byteLength > 998) throw new Error("The subject must be under 999 bytes.");
  if (!row.textBody.trim()) throw new Error("Include a plain-text version.");
  return { subject: row.subject, textBody: row.textBody, htmlBody: row.htmlBody };
}
export function newTemplate(): string {
  return JSON.stringify({ subject: "Thanks for subscribing", textBody: "Thanks for joining us. We're glad you're here.",
    htmlBody: "<h1>Welcome</h1><p>Thanks for joining us. We're glad you're here.</p>" }, null, 2) + "\n";
}

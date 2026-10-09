import { UserInputError } from "./problems.js";
export interface EmailTemplate { subject: string; textBody: string; htmlBody: string }
export function readTemplate(value: string): EmailTemplate {
  if (new TextEncoder().encode(value).byteLength > 2 * 1024 * 1024) throw new UserInputError("Email templates must be 2 MiB or smaller.");
  let parsed: unknown;
  try { parsed = JSON.parse(value); } catch { throw new UserInputError("This template has invalid JSON. Open Source and correct its formatting."); }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new UserInputError("An email template must be a JSON object.");
  const row = parsed as Record<string, unknown>;
  for (const key of Object.keys(row)) if (!["subject", "textBody", "htmlBody"].includes(key)) throw new Error(`Unknown template field: ${key}`);
  if (typeof row.subject !== "string" || !row.subject.trim() || /[\u0000-\u001f\u007f]/.test(row.subject)) throw new UserInputError("Enter a subject on one line.");
  if (typeof row.textBody !== "string" || typeof row.htmlBody !== "string") throw new UserInputError("The template needs textBody and htmlBody strings.");
  if (new TextEncoder().encode(row.subject).byteLength > 998) throw new UserInputError("The subject must be under 999 bytes.");
  if (!row.textBody.trim()) throw new UserInputError("Include a plain-text version.");
  return { subject: row.subject, textBody: row.textBody, htmlBody: row.htmlBody };
}
export function newTemplate(): string {
  return JSON.stringify({ subject: "Thanks for subscribing", textBody: "Thanks for joining us. We're glad you're here.",
    htmlBody: "<h1>Welcome</h1><p>Thanks for joining us. We're glad you're here.</p>" }, null, 2) + "\n";
}

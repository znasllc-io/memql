import { PDFDocument, degrees, StandardFonts } from "pdf-lib";

export type PDFChange = { kind: "rotate"; page: number } | { kind: "text"; page: number; text: string; x: number; y: number };
export async function changePDF(bytes: Uint8Array, change: PDFChange): Promise<Uint8Array> {
  const pdf = await PDFDocument.load(bytes);
  if (!Number.isInteger(change.page) || change.page < 0 || change.page >= pdf.getPageCount()) throw new Error("Choose an existing page.");
  const page = pdf.getPage(change.page);
  if (change.kind === "rotate") page.setRotation(degrees((page.getRotation().angle + 90) % 360));
  else {
    if (!change.text.trim() || change.text.length > 2000) throw new Error("Enter between 1 and 2,000 characters.");
    if (![change.x, change.y].every(Number.isFinite) || change.x < 0 || change.y < 0 || change.x > page.getWidth() || change.y > page.getHeight()) throw new Error("Choose a position inside the page.");
    page.drawText(change.text, { x: change.x, y: change.y, size: 12, font: await pdf.embedFont(StandardFonts.Helvetica) });
  }
  return pdf.save();
}

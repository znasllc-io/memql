import * as vscode from "vscode";
import { PDFDocument } from "pdf-lib";

function check(value: unknown, detail: string): asserts value { if (!value) throw new Error(detail); }
export async function run(): Promise<void> {
  const core = vscode.extensions.getExtension("znasllc.memql");
  const productivity = vscode.extensions.getExtension("znasllc.memql-productivity-tools");
  check(core && productivity, "Both extensions must be installed in this host.");
  const api = await core.activate();
  check(api.connection?.version === 1, "The core extension must export the connection API on this host.");
  const tools = await productivity.activate();
  check(tools.connectionVersion === 1, "Productivity must use the core connection API.");
  const commands = await vscode.commands.getCommands(true);
  for (const command of ["memql.clusters.add", "memql.clusters.signIn", "memql.productivity.newTemplate", "memql.productivity.previewTemplate"]) {
    check(commands.includes(command), `${command} was not registered.`);
  }
  await vscode.commands.executeCommand("memql.productivity.newTemplate");
  const document = vscode.window.activeTextEditor?.document;
  check(document?.languageId === "json", "New template must open in a JSON editor.");
  check(JSON.parse(document.getText()).subject === "Thanks for subscribing", "New template lost its subject.");
  await vscode.commands.executeCommand("memql.productivity.previewTemplate");
  // Actual VS Code file I/O, through a virtual workspace on the browser host.
  const root = vscode.workspace.workspaceFolders?.[0]?.uri;
  check(root, "The host test requires its isolated fixture workspace.");
  const emailURI = vscode.Uri.joinPath(root, "host.email.json");
  await vscode.workspace.fs.writeFile(emailURI, new TextEncoder().encode(document.getText()));
  await vscode.commands.executeCommand("vscode.openWith", emailURI, "memql.productivity.email");
  check(await tools.templateReady(emailURI) === "Thanks for subscribing", "Email preview did not render the subject.");
  await vscode.commands.executeCommand("memql.productivity.template.split", emailURI);
  const emailDocument = await vscode.workspace.openTextDocument(emailURI);
  check(vscode.window.visibleTextEditors.some(editor => editor.document === emailDocument), "Email split view lost the shared source document.");
  const template = JSON.parse(emailDocument.getText()); template.subject = "Updated live preview";
  const emailEdit = new vscode.WorkspaceEdit();
  emailEdit.replace(emailURI, new vscode.Range(emailDocument.positionAt(0), emailDocument.positionAt(emailDocument.getText().length)), JSON.stringify(template, null, 2));
  check(await vscode.workspace.applyEdit(emailEdit), "Email source edit failed.");
  check(await tools.templateReady(emailURI, emailDocument.version) === "Updated live preview", "Email preview did not follow unsaved source.");
  await vscode.commands.executeCommand("memql.productivity.template.source", emailURI);
  check(vscode.window.activeTextEditor?.document === emailDocument && emailDocument.isDirty, "Email source mode discarded unsaved changes.");
  await emailDocument.save();
  await vscode.commands.executeCommand("memql.productivity.template.preview", emailURI);
  await tools.templateReady(emailURI);
  const markdownURI = vscode.Uri.joinPath(root, "host-review.md");
  await vscode.workspace.fs.writeFile(markdownURI, new TextEncoder().encode("# Markdown review\n\nA **rendered** passage.\n"));
  await vscode.commands.executeCommand("vscode.openWith", markdownURI, "memql.productivity.markdown");
  check((await tools.markdownReady(markdownURI)).includes("A rendered passage."), "Reading view must render Markdown without source markers.");
  await vscode.commands.executeCommand("memql.productivity.markdown.split", markdownURI);
  const markdownDocument = await vscode.workspace.openTextDocument(markdownURI);
  check(vscode.window.visibleTextEditors.some(editor => editor.document === markdownDocument), "Split view must include the same source document.");
  const edit = new vscode.WorkspaceEdit();
  edit.insert(markdownURI, new vscode.Position(2, 0), "Unsaved ");
  check(await vscode.workspace.applyEdit(edit), "Source edit failed.");
  check((await tools.markdownReady(markdownURI, markdownDocument.version)).includes("Unsaved A rendered passage."), "Reading view must follow unsaved source edits.");
  await vscode.commands.executeCommand("memql.productivity.markdown.source", markdownURI);
  check(vscode.window.activeTextEditor?.document.getText().includes("Unsaved A **rendered** passage."), "Source mode discarded the dirty buffer.");
  await markdownDocument.save();
  await vscode.commands.executeCommand("memql.productivity.markdown.reading", markdownURI);
  await tools.markdownReady(markdownURI);
  const pdf = await PDFDocument.create(); pdf.addPage([400, 600]);
  const uri = vscode.Uri.joinPath(root, "host-smoke.pdf");
  await vscode.workspace.fs.writeFile(uri, await pdf.save());
  await vscode.commands.executeCommand("vscode.openWith", uri, "memql.productivity.pdf");
  await tools.pdfReady(uri);
  check((await vscode.workspace.fs.readFile(uri)).length > 100, "The PDF was not saved through the host file system.");
  console.log(`PASS: both MemQL extensions activate, share the API, and open templates, PDFs, and all three Markdown modes (${vscode.env.uiKind === vscode.UIKind.Web ? "web" : "desktop"}).`);
}

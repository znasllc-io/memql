import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { ContextKeyExpr } from '@codingame/monaco-vscode-api/vscode/vs/platform/contextkey/common/contextkey';
import { getAllCodicons } from '@codingame/monaco-vscode-api/vscode/vs/base/common/codicons';

const manifest = JSON.parse(readFileSync(new URL('../../productivity/package.json', import.meta.url), 'utf8'));
const modes = ['source', 'reading', 'review'];

test('Markdown view buttons stay in the editor header regardless of text-editor focus', () => {
  const entries = manifest.contributes.menus['editor/title'];
  const visible = context => modes.filter(mode => {
    const entry = entries.find(item => item.command === `memql.productivity.markdown.${mode}`);
    assert.ok(entry, `${mode} must have an editor header button`);
    assert.match(entry.group, /^navigation@\d+$/, 'View controls must be buttons, not overflow menu items');
    const icon = manifest.contributes.commands.find(command => command.command === entry.command)?.icon;
    assert.ok(getAllCodicons().some(codicon => icon === `$(${codicon.id})`), `${mode} needs an icon the host actually supplies`);
    return ContextKeyExpr.deserialize(entry.when).evaluate({ getValue: key => context[key] });
  });
  // The title's resource context survives native source/custom editor switches.
  // editorLangId belongs to the focused code editor, and can be absent or refer
  // to a different pane while clicking the title of a Markdown document.
  for (const editorLangId of [undefined, 'markdown', 'json']) {
    assert.deepEqual(visible({ resourceLangId: 'markdown', editorLangId }), modes);
  }
  for (const resourceScheme of ['file', 'memql-file']) {
    for (const resourceFilename of ['review.md', 'Review.MD', 'review.markdown']) {
      assert.deepEqual(visible({ resourceScheme, resourceFilename }), modes);
    }
  }
  for (const resourceFilename of ['review.pdf', 'review.md.png', 'host.email.json']) {
    assert.deepEqual(visible({ resourceFilename, resourceLangId: 'json', editorLangId: 'markdown' }), []);
  }
});

test('Email view buttons stay in the editor header for email files only', () => {
  const entries = manifest.contributes.menus['editor/title'];
  const emailModes = ['design', 'source', 'split'];
  const visible = context => emailModes.filter(mode => {
    const entry = entries.find(item => item.command === `memql.productivity.email.${mode}`);
    assert.ok(entry, `${mode} must have an editor header button`);
    assert.match(entry.group, /^(navigation|2_email)@\d+$/, 'Email view controls are header buttons in a group of their own');
    const command = manifest.contributes.commands.find(item => item.command === entry.command);
    assert.equal(command?.category, 'MemQL');
    assert.ok(getAllCodicons().some(codicon => command?.icon === `$(${codicon.id})`), `${mode} needs an icon the host actually supplies`);
    return ContextKeyExpr.deserialize(entry.when).evaluate({ getValue: key => context[key] });
  });
  assert.deepEqual(emailModes.map(mode => entries.find(item => item.command === `memql.productivity.email.${mode}`)?.group), ['navigation@1', 'navigation@2', '2_email@3']);
  for (const resourceScheme of ['file', 'memql-file']) {
    for (const resourceFilename of ['welcome.email.json', 'Welcome.EMAIL.JSON']) {
      assert.deepEqual(visible({ resourceScheme, resourceFilename, resourceLangId: 'json' }), emailModes);
    }
  }
  for (const resourceFilename of ['review.md', 'email.json', 'welcome.email.json.md', 'welcome.email']) {
    assert.deepEqual(visible({ resourceFilename, resourceLangId: 'markdown' }), [], resourceFilename);
  }
  // The template editor's commands are gone, with their buttons.
  assert.ok(!entries.some(item => /^memql\.productivity\.template\./.test(item.command)));
  assert.ok(!manifest.contributes.commands.some(item => /^memql\.productivity\.(template\.|previewTemplate$|newTemplate$)/.test(item.command)));
});

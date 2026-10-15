// The rule for what a person may call a new file (sdk/ts/src/client/fileName.ts):
// the one rule, and the words, that VS Code's New Markdown and Create Email from
// Examples and the MemQL OS composer check a name with as it is typed.

import test from "node:test";
import assert from "node:assert/strict";

import { fileNameProblem, fileNameStem } from "../src/client/fileName.js";

// Each problem has its own sentence, so the person knows what to change.

const NO_NAME = "Enter a file name.";
const NOT_A_FILE_NAME = "Enter a file name, without a folder path.";
const TOO_LONG = "Use a name up to 150 characters.";
const NO_NAME_BEFORE_EXTENSION = "Enter a name before the extension.";

test("fileNameProblem accepts a file name, with or without its extension", () => {
  for (const [name, format] of [
    ["Untitled.md", "markdown"], ["Plan", "markdown"], ["Q3.report.md", "markdown"], ["notes.markdown", "markdown"], ["a", "markdown"],
    ["Plan .md", "markdown"], ["  Plan  ", "markdown"], ["data.csv", "csv"], ["page", "html"], ["Plan.md", "txt"], [".md", "txt"],
    ["x".repeat(150), "markdown"],
  ] as const) {
    assert.equal(fileNameProblem(name, format), undefined, `"${name}" as ${format} should be accepted`);
  }
});

// Each refusal says what is actually wrong with the name, so the person knows what to change.
test("fileNameProblem asks for a name when there is none", () => {
  for (const name of ["", "   ", "\t \n"]) {
    assert.equal(fileNameProblem(name, "markdown"), NO_NAME, `"${name}" has no name in it`);
  }
});

test("fileNameProblem refuses a place: a slash, a backslash or a control character", () => {
  for (const name of ["a/b.md", "a\\b.md", "Notes 10/11", "/Plan", "tab\there.md", "bell\u0007.md", "x".repeat(151) + "/"]) {
    assert.equal(fileNameProblem(name, "markdown"), NOT_A_FILE_NAME, `"${name}" should be refused as a path`);
  }
});

test("fileNameProblem refuses a name over 150 characters, and says how long one may be", () => {
  assert.equal(fileNameProblem("x".repeat(151), "markdown"), TOO_LONG);
  assert.equal(fileNameProblem(`${"x".repeat(148)}.md`, "markdown"), TOO_LONG, "the extension counts");
  assert.equal(fileNameProblem("x".repeat(150), "markdown"), undefined);
});

test("fileNameProblem refuses an extension of the file's own format with nothing before it", () => {
  for (const [name, format] of [[".md", "markdown"], [".markdown", "markdown"], [" .MD ", "markdown"], [".Markdown", "markdown"], [".csv", "csv"], [".HTML", "html"], [".json", "json"]] as const) {
    assert.equal(fileNameProblem(name, format), NO_NAME_BEFORE_EXTENSION, `"${name}" as ${format} should be refused`);
  }
});

test("fileNameStem drops one extension of the file's own format, as the cluster does", () => {
  for (const [name, format, stem] of [
    ["Plan.md", "markdown", "Plan"], ["Plan.MD", "markdown", "Plan"], ["Plan.markdown", "markdown", "Plan"], ["  Plan.md  ", "markdown", "Plan"],
    ["Plan .md", "markdown", "Plan"], ["Plan.md.md", "markdown", "Plan.md"], ["Plan.txt", "markdown", "Plan.txt"], ["notes.txt", "txt", "notes"],
    ["data.CSV", "csv", "data"], ["v1.2 notes", "markdown", "v1.2 notes"], [".md", "markdown", ""],
  ] as const) {
    assert.equal(fileNameStem(name, format), stem, `"${name}" as ${format}`);
  }
});

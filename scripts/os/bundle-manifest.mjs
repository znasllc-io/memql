#!/usr/bin/env node
// scripts/os/bundle-manifest.mjs <dist-dir>
//
// Writes <dist-dir>/memql-bundle.json: the sha256 of every file of the built
// MemQL OS bundle, by dist-relative path. The edge image's layer carries it
// at /app/os/memql-bundle.json, so verify-rollout (cmd/memql-verify) can read
// what the image holds and compare it with what the public front door serves
// (epic memql#5480). Node stdlib only; deterministic (sorted keys, no
// timestamps), and it never lists itself.
import { createHash } from "node:crypto";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, relative, sep } from "node:path";

const NAME = "memql-bundle.json";
const root = process.argv[2];
if (!root) {
  console.error("usage: bundle-manifest.mjs <dist-dir>");
  process.exit(2);
}
const files = {};
function walk(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) walk(path);
    else if (entry.isFile()) {
      const rel = relative(root, path).split(sep).join("/");
      if (rel === NAME) continue;
      files[rel] = createHash("sha256").update(readFileSync(path)).digest("hex");
    }
  }
}
walk(root);
const sorted = Object.fromEntries(Object.keys(files).sort().map((k) => [k, files[k]]));
writeFileSync(join(root, NAME), JSON.stringify({ schema: 1, algorithm: "sha256", files: sorted }) + "\n");
console.error(`INFO: ${NAME}: ${Object.keys(sorted).length} files`);

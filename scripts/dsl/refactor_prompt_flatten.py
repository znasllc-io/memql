#!/usr/bin/env python3
# Flatten the input-wrapper block in every prompt .memql file under
# dsl/v1/prompts. Before:
#
#   prompt myPrompt {
#     <input-wrapper> {
#       fieldA  type  @anno
#       fieldB  type  @anno
#     <close-wrapper>
#   }
#
# After:
#
#   prompt myPrompt {
#     fieldA  type  @anno
#     fieldB  type  @anno
#   }
#
# Idempotent. Skips files that already on the flat form.
import os
import re
import sys

root = os.environ.get("ROOT")
if not root:
    sys.stderr.write("ROOT env var is required\n")
    sys.exit(1)

prompts_dir = os.path.join(root, "dsl", "v1", "prompts")
if not os.path.isdir(prompts_dir):
    sys.stderr.write(f"missing {prompts_dir}\n")
    sys.exit(1)

# Match a body-level wrapper block:
#
#   <indent>@input {
#     ...fields...
#   <indent>}
#
# Captures the inner body so we can dedent it by one level (2 spaces)
# before splicing back into the surrounding `prompt name { ... }`.
WRAPPER_RE = re.compile(
    r"^(?P<indent>[ \t]*)@input\s*\{\s*\n(?P<body>.*?)\n[ \t]*\}\s*\n",
    re.DOTALL | re.MULTILINE,
)

migrated = 0
unchanged = 0
skipped = 0

for cur_root, _, files in os.walk(prompts_dir):
    for fname in files:
        if not fname.endswith(".memql"):
            continue
        if fname.startswith("_"):
            continue
        path = os.path.join(cur_root, fname)
        with open(path, "r", encoding="utf-8") as f:
            content = f.read()
        m = WRAPPER_RE.search(content)
        if not m:
            if "@input" in content:
                skipped += 1
                print(f"  SKIP (couldn't match wrapper): {path}")
            else:
                unchanged += 1
            continue
        body = m.group("body")
        non_blank = [line for line in body.split("\n") if line.strip()]
        if not non_blank:
            skipped += 1
            print(f"  SKIP (empty wrapper body): {path}")
            continue
        min_indent = min(len(line) - len(line.lstrip(" ")) for line in non_blank)
        # Standard 4 -> 2 level shift (one level inside `prompt {}`).
        to_strip = min(2, min_indent)
        out_lines = []
        for line in body.split("\n"):
            if not line.strip():
                out_lines.append("")
                continue
            stripped = line[to_strip:] if line.startswith(" " * to_strip) else line
            out_lines.append(stripped)
        new_content = content[: m.start()] + "\n".join(out_lines) + "\n" + content[m.end():]
        with open(path, "w", encoding="utf-8") as f:
            f.write(new_content)
        migrated += 1

print(f"migrated: {migrated}  unchanged: {unchanged}  skipped: {skipped}")

---
title: Native security and quality checks
audience: ops
status: stable
area: ops
sinceVersion: 0.29.5
owner: znas
---

# Native security and quality checks

The engine's `memql-package.yaml` runs CodeQL for Go, JavaScript/TypeScript
and Python through `scripts/ci/codeql-scan.py`. Each language extracts the
source once and analyzes the union of `security-extended` and the bundled
language's `code-quality` suite. The existing exclusions in
`.github/codeql/codeql-config.yml` apply to the union. The CLI bundle is
versioned and SHA-256 verified; the quality pack is selected at its bundled
version. A missing or ambiguous quality suite fails instead of silently
reducing coverage.

The manifest places these scans on the weekly schedule and on manual and
release runs. The ordinary PR path retains its affected functional tests,
type checks, linters and introduced-commit secret scan. Scheduled heavy scans
do not repeat on every main push. The output includes `summary.json`, the
selected suite names, tool logs and `results.sarif`; the native report stage
publishes that exact retained SARIF and waits for confirmed GitHub ingestion.

A successful scan confirms that the analysis completed, not that it found no
problems. Findings remain available for triage. The native command does not
provide GitHub Code Quality's bot comments or Copilot Autofix suggestions;
those are separate hosted capabilities. Their absence must be recorded when
retiring the hosted workflow, not presented as equivalent behavior.

To reproduce one language with a preinstalled bundle of the pinned version:

```bash
python3 scripts/ci/codeql-scan.py --language=python \
  --threads=1 --ram=3072 --codeql=/path/to/codeql/codeql \
  --output-dir=/tmp/memql-python-analysis
```

The output directory must be new. Omit `--codeql` to download and verify the
bundle. Run from the repository root. This command does not upload reports or
change repository settings. Installing this scanner is not evidence that the
production cutover has passed; that acceptance remains on memql#5506 and
memql#5509.

References: GitHub's [CodeQL workflow configuration](https://docs.github.com/en/code-security/reference/code-scanning/workflow-configuration-options)
and [Code Quality behavior](https://docs.github.com/en/code-security/reference/code-quality/codeql-detection).

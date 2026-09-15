# Code-scanning triage: 2026-09-15

Reviewed all 22 open alerts against main's Go CodeQL analysis 1780323185
(commit `9bbb1ad6e427ed17eb53f826d7e0ec2354a50941`) and the source at
`6937b93c1aa8ebda7c2d56d8362f221987e373dc`.

## Allocation overflow fixes

- #1168, #1161, #1160, #1159, #1158: use the existing scope's length as
  the map capacity hint; new bindings grow the map without adding unchecked
  lengths before allocation.
- #1157, #1156: refuse list concatenation if the sum of the lengths cannot
  fit in an int, reporting an expression error before allocation.
- #1155, #1154: guard the extra edit-distance boundary cell against int overflow.
- #1165, #1164, #1153, #1152, #1151: bound alignment matrix dimensions at
  allocation and compare using division. Multiplying dimensions to enforce
  a memory limit can itself overflow and bypass that limit.
- #1150, #1135: remove multiplication/addition from slice capacity hints;
  append handles growth as entries are collected.

`TestLCSPairsRejectsOverflowingDimensions` exercises MaxInt dimensions
without allocating their input sequences. `TestLCSLinePairsBoundsAtAllocation`
checks the independent allocation guard and ordinary alignment behavior.

## Individual false positives

No additional CodeQL queries are excluded.

### SQL injection: #1148 and #1147

The SARIF paths reach `buildJSONBPathExpression` / `buildJSONPathExpression`,
then the compiled filter's SQL. Both builders reject empty segments and require
`isSafePathSegment`: every rune must be a letter, digit, underscore, or hyphen.
SQL quoting, backslashes, braces, commas, whitespace inside a segment, and
statement delimiters are rejected. Accepted segments are additionally quoted;
comparison values travel separately through Bun placeholders and `filter.args`.
The reported paths cross this custom validator without recognizing it as a
sanitizer. They do not show unvalidated SQL entering either `Where` call.

`TestPayloadPathBuildersRejectSQLSyntax` covers both builders with hostile
segments. Existing `TestCompilePayloadComparisonDoesNotInlineValues` verifies
that quoted comparison values remain outside the SQL template.

### Open redirect: #1149

The redirect destination is the nonempty result of `identity.SafeRelativeRedirect`.
It requires a leading single slash, rejects protocol-relative and slash-backslash
destinations, rejects CR/LF/NUL, parses the URL, and requires an empty scheme and
host. Go's URL parser also rejects embedded ASCII controls, including tabs.
The reported path crosses this custom sanitizer. First-party paths and their
query strings are intentionally accepted. `TestSafeRelativeRedirect*` exercises
the acceptance and rejection boundary, including browser-normalized host tricks.

### Weak sensitive-data hashing: #1139, #1134 and #1073

These are SHA-256 content fingerprints, not password verifiers:

- #1139: `fingerprintAccountSet` identifies actor/account scope for the plan cache.
- #1134: `FleetCallFingerprint` identifies repeated model calls for the loop breaker.
- #1073: `ArtifactHash` binds an approval to its artifact content.

The weak-sensitive-data-hashing query recommends a password KDF for stored
passwords. These callers need deterministic content identifiers and do not
store or verify passwords. SHA-256 is appropriate for that primitive; changing
them to a password KDF would not remediate the reported issue. Existing tests
cover stable fingerprints, differing scope/call inputs, and approval rejection
when the artifact hash changes. Dismiss these specific alerts as false positives;
retain the query to detect actual password hashing mistakes elsewhere.
